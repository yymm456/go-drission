package chromium

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// defaultCDPTimeout 是 browser 级 CDP 调用（创建/销毁隔离上下文、上下文内新建标签页等）
// 在调用方 ctx 未设 deadline 时采用的默认超时，防止 Chrome 无响应导致永久阻塞。
const defaultCDPTimeout = 30 * time.Second

// defaultTimeout 是 Tab 级操作的默认超时。
// 只在调用方传入的 ctx 没有 deadline 时生效，作为「Chrome 卡死也不会永久阻塞」的兜底。
// 可用 WithDefaultTimeout 全局调整，或用 Tab.SetTimeout 单独调整某个标签页。
const defaultTimeout = 30 * time.Second

// Browser 持有整个浏览器连接和所有标签页
//
// # 锁序
//
// 本类型有三把锁，加锁必须遵守下面的顺序，否则会形成环：
//
//	mu（tabs 与 closed） → connMu（连接状态） → ctxMu（contexts 注册表）
//
// 实践中没有任何路径会同时持有两把以上的锁做 I/O；Browser.Close 是唯一
// 按 mu → ctxMu 顺序取两把锁的地方，且中途会释放 mu 再做销毁（销毁是 I/O）。
type Browser struct {
	port int
	opts *options

	allocCtx    context.Context
	allocCancel context.CancelFunc
	// rootCtx 是常驻的浏览器上下文：所有标签页上下文都作为它的子上下文派生，
	// 以共享同一个 browser 连接（chromedp 要求如此，否则新建标签页会失败）。
	// rootTargetID 是 rootCtx 自身占用的锚点 target，不纳入标签页管理。
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	rootTargetID target.ID
	launched     bool
	chromeCmd    *exec.Cmd    // 启动的 Chrome 进程，Close 时用于杀进程
	lock         *profileLock // 数据目录排他锁，仅自己启动 Chrome 时持有
	closed       bool

	mu   sync.Mutex
	tabs []*Tab

	// newTabMu 只保护 NewTab 的「建 target → 比对列表定位新 ID」这一段。
	// 该流程依赖前后两次全局 target 快照，必须串行；但它与 tabs 的状态无关，
	// 所以独立成一把锁，避免新建标签页时把 Tabs/GetTab 一起堵住。
	newTabMu sync.Mutex

	// connMu 保护连接状态字段，与 mu（保护 tabs）分开，避免连接握手长时间持锁
	connMu    sync.Mutex
	connected bool

	// ctxMu 保护 contexts（命名隔离上下文注册表），与 mu 分开以降低锁竞争
	ctxMu    sync.Mutex
	contexts map[string]*BrowserContext
}

// ensureConnected 校验浏览器处于可用状态。
//
// 单独抽出是为了在 NewTab / Context 等入口统一拦截「未 Connect 就使用」：
// 否则 b.rootCtx 为 nil，chromedp.NewContext(nil) 会直接 panic，报错完全不可读。
//
// 判据是 connMu 下的 connected 标志，并且要求 rootCtx / rootCancel 都在位。
// 早期实现只判 `rootCtx != nil`：initRootContext 先赋值 rootCtx 再 Run，
// 而 Run 失败时 Connect 的清理分支只把 rootCancel 置空、留下非 nil 的 rootCtx，
// 于是这里会放行，后续所有操作都作用在一个已死的上下文上，
// 报出一堆含糊的 context canceled / invalid context。
//
// 调用方需持有 b.mu（读 b.closed）——锁序见类型注释。
func (b *Browser) ensureConnected() error {
	b.connMu.Lock()
	connected := b.connected
	b.connMu.Unlock()

	if b.closed {
		return ErrClosed
	}
	if !connected || b.rootCtx == nil || b.rootCancel == nil {
		return ErrNotConnected
	}
	return nil
}

// NewBrowser 创建 Browser
// port 传 0 表示随机端口，传具体值表示固定端口
func NewBrowser(port int, opts ...Option) *Browser {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	if port == 0 {
		if free, err := findFreePort(); err == nil {
			port = free
		} else {
			port = 9222 // 分配失败时退回默认端口
		}
	}

	// 用户数据目录：未显式指定时，按端口生成默认路径（需在端口确定之后）。
	// 必须转为绝对路径：相对路径会让 Chrome handoff 到已存在的浏览器会话，
	// 从而不新开实例，请求的调试端口也就永远无法就绪。
	if o.userDataDir == "" {
		o.userDataDir = defaultUserDataDir(port)
	} else if abs, err := filepath.Abs(o.userDataDir); err == nil {
		o.userDataDir = abs
	}

	return &Browser{
		port:     port,
		opts:     o,
		contexts: map[string]*BrowserContext{},
	}
}

// removeContext 从命名上下文注册表中移除指定名字的隔离上下文。
// 仅持有 ctxMu，不触碰 mu，避免与 Browser.Close 产生锁顺序环。
func (b *Browser) removeContext(name string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	delete(b.contexts, name)
}

// boundedRootCtx 基于常驻 rootCtx 派生一个运行上下文：既携带 browser 级 CDP 路由信息，
// 又受调用方 ctx 的取消与超时约束。browser 级命令必须在 rootCtx 分支上执行（依赖
// FromContext(c).Browser 路由），但直接用无超时的 rootCtx 会在 Chrome 卡死时永久阻塞。
//
// 规则：调用方 ctx 带 deadline 时取 min(defaultCDPTimeout, 剩余时间)；否则套用默认超时；
// 调用方 ctx 被取消时同步取消。返回的 cancel 必须由调用方 defer 调用。
// 取消 runCtx 只结束本次调用，不影响 rootCtx 上的 browser 长连接
// （该连接的生命周期绑定在 initRootContext 首次 Run 的 rootCtx 上）。
func (b *Browser) boundedRootCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := defaultCDPTimeout
	if dl, ok := ctx.Deadline(); ok {
		if until := time.Until(dl); until < d {
			d = until
		}
	}
	runCtx, cancel := context.WithTimeout(b.rootCtx, d)
	// runCtx 的父是 rootCtx 而不是 ctx，调用方对 ctx 的取消传导不过来，得自己接上。
	// AfterFunc 正好做这件事，且 stop() 能立刻解除注册——比常驻一个 select goroutine 干净。
	stop := context.AfterFunc(ctx, cancel)
	return runCtx, func() {
		stop()
		cancel()
	}
}

// Port 返回当前 Browser 使用的调试端口
func (b *Browser) Port() int {
	return b.port
}

// PID 返回本实例启动的 Chrome 主进程 PID；接管已有 Chrome 时返回 0。
// 可与 Tab.WindowID 配合证明「多个窗口同属一个浏览器进程」。
func (b *Browser) PID() int {
	if b.chromeCmd == nil || b.chromeCmd.Process == nil {
		return 0
	}
	return b.chromeCmd.Process.Pid
}

// Connect 探测端口：活着就连，空闲就启动。
//
// ctx 控制整个握手阶段（端口探测 / 启动 Chrome / 首次连接），取消或到期即中止。
// ctx 未设置 deadline 时，退回 opts.connectTimeout（默认 10s）作为握手超时。
//
// 注意：握手结束后，长连接会绑定到内部 background ctx 而非调用方 ctx——
// 连接的生命周期应当跟随 Browser 由 Close() 回收，若绑在调用方的一次性 ctx 上，
// ctx 一到期浏览器连接就被切断，后续所有操作都会报 context canceled。
func (b *Browser) Connect(ctx context.Context) error {
	b.connMu.Lock()
	defer b.connMu.Unlock()

	if b.connected {
		// 重复连接会覆盖并泄漏上一条 allocator 与浏览器连接，显式拒绝而非静默覆盖
		return ErrAlreadyConnected
	}
	if b.closed {
		return ErrClosed
	}

	// 握手超时：ctx 自带 deadline 时完全听调用方的；否则套用 connectTimeout
	handshakeCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		timeout := b.opts.connectTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		var cancel context.CancelFunc
		handshakeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if isPortAlive(handshakeCtx, b.port) {
		b.opts.logger.Info("检测到已有 Chrome，直接连接", "port", b.port)
		b.launched = false
		// 关键一步：核实端口上那个 Chrome 确实是我们期望的档案。
		// 少了这步，显式指定的 WithUserDataDir 会被静默忽略——你以为在用档案 A，
		// 实际接管了别人的浏览器（别的登录态），多账号隔离无声失效。
		if err := b.verifyPortOwner(); err != nil {
			return err
		}
	} else {
		b.opts.logger.Info("未检测到 Chrome，正在启动", "port", b.port)
		// 先拿数据目录的 OS 级排他锁：两个实例并发用同一目录启动 Chrome 时，
		// 后启动者会弹「无法对其数据目录执行读写操作」对话框，这里提前转为明确的 Go 错误。
		lock, err := acquireProfileLock(b.opts.userDataDir)
		if err != nil {
			return err
		}
		cmd, err := launchChrome(handshakeCtx, b.port, b.opts)
		if err != nil {
			lock.release()
			return err
		}
		b.lock = lock
		b.launched = true
		b.chromeCmd = cmd
		// 留下档案标记，供下次接管时核实「端口上的浏览器属于哪个目录」。
		// 写入失败不阻断启动，只记日志（最坏结果是下次接管走「无法证实」分支）。
		if err := writeProfileMarker(b.opts.userDataDir, b.port, b.PID()); err != nil {
			b.opts.logger.Warn("写入档案标记失败，下次接管将无法核实用户数据目录",
				"dir", b.opts.userDataDir, "err", err)
		}
	}

	b.allocCtx, b.allocCancel = chromedp.NewRemoteAllocator(
		context.Background(),
		fmt.Sprintf("http://127.0.0.1:%d", b.port),
	)

	if err := b.initRootContext(handshakeCtx); err != nil {
		// 连接没建成：就地回收本次 Connect 已申请的全部资源，不依赖调用方随后一定调 Close()。
		// 否则直接 NewBrowser+Connect 的调用方在失败后若不 Close，会泄漏一个僵尸 Chrome 进程，
		// 且数据目录排他锁一直被占，导致下次启动报「无法读写数据目录」。
		// 回收顺序与 Close() 保持一致：子 ctx → allocator → 杀进程树 → 释放目录锁。
		//
		// rootCtx 也必须一起置 nil：只清 rootCancel 会留下「rootCtx 非 nil 但连接已死」
		// 的中间态，让 ensureConnected 误判为已连接（见该方法注释）。
		if b.rootCancel != nil {
			b.rootCancel()
			b.rootCancel = nil
		}
		b.rootCtx = nil
		if b.allocCancel != nil {
			b.allocCancel()
			b.allocCancel = nil
		}
		if b.launched {
			killProcessTree(b.chromeCmd)
			b.launched = false
		}
		b.chromeCmd = nil
		if b.lock != nil {
			b.lock.release()
			b.lock = nil
		}
		return err
	}
	b.connected = true
	b.opts.logger.Info("已连接 Chrome", "port", b.port, "launched", b.launched)
	return nil
}

// initRootContext 建立常驻的浏览器上下文 rootCtx，并借此确认 Chrome 真的可连。
//
// chromedp 中，只有从「已分配 browser 连接的上下文」派生的子上下文，才会共享该连接并通过
// Target.createTarget 新建标签页；若直接从 allocator(allocCtx) 派生，每个上下文都会各自新建
// 一条 browser 连接，在已有连接存在时 createTarget 可能失败（no browser is open）。
// 因此这里先建立并 Run 一个 rootCtx 作为所有标签页上下文的父级。rootCtx 自身会占用一个空白
// target 作为锚点（rootTargetID），不纳入标签页管理，Browser.Close 时随 rootCancel 一并释放。
//
// ctx 用于给首次 Run 兜底超时，但不会被用作连接的生命周期 ctx（见 runAbandonable）。
func (b *Browser) initRootContext(ctx context.Context) error {
	b.rootCtx, b.rootCancel = chromedp.NewContext(b.allocCtx)

	// 首次 Run 必须直接作用于 b.rootCtx，不能包一层「可取消的超时子 ctx」：
	// RemoteAllocator 会把浏览器连接的生命周期绑定到首次 Run 传入的 ctx，
	// 一旦该 ctx 被取消，连接随之关闭，rootCtx 立即失效（后续操作报 context canceled）。
	// 所以用 runAbandonable：超时只是「放弃等待」，不去取消 rootCtx。
	err := runAbandonable(ctx, b.rootCtx, func(runCtx context.Context) error {
		return chromedp.Run(runCtx, chromedp.ActionFunc(func(context.Context) error {
			return nil
		}))
	})
	if err != nil {
		return fmt.Errorf("连接 Chrome (端口 %d) 失败: %w", b.port, err)
	}

	// 记录 rootCtx 自身占用的锚点 target，后续同步标签页时跳过它
	var info *target.Info
	_ = runAbandonable(ctx, b.rootCtx, func(runCtx context.Context) error {
		return chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
			var e error
			info, e = target.GetTargetInfo().Do(c)
			return e
		}))
	})
	if info != nil {
		b.rootTargetID = info.TargetID
	}
	return nil
}

// guard 取一次「连接可用」的快照，供随后在锁外做 I/O 的方法使用。
// 需要持 b.mu 读 closed，锁序见类型注释。
func (b *Browser) guard() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ensureConnected()
}

// Tabs 返回当前浏览器里所有 page 类型的标签页。
//
// 会在锁外查询 target 列表并附着新出现的标签页，避免端口卡顿时把
// 所有标签页操作一起串行卡住（见 syncTabs 的说明）。
func (b *Browser) Tabs(ctx context.Context) ([]*Tab, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}
	infos, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}
	return b.syncTabs(infos)
}

// NewTab 新建一个标签页并托管
//
// 全程在锁外做 I/O（HTTP 查询 + CDP 往返），只在最后登记标签页时短暂持锁；
// 但「新建 target → 比对前后 target 列表定位新 ID」这一段必须串行，
// 否则两个并发的 NewTab 会互相把对方的新 target 认成自己的，因此用 newTabMu 单独保护
// （它不保护 tabs，所以不会和 Tabs/GetTab 争锁）。
func (b *Browser) NewTab(ctx context.Context) (*Tab, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}

	b.newTabMu.Lock()
	defer b.newTabMu.Unlock()

	before, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}

	// 从常驻 rootCtx 派生子上下文：子上下文共享 browser 连接，Run 时通过 createTarget 新建标签页
	tabCtx, cancel := chromedp.NewContext(b.rootCtx)
	if err := chromedp.Run(tabCtx); err != nil {
		cancel()
		return nil, err
	}

	after, err := b.listTargets(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	newID := findNewTargetID(before, after)
	if newID == "" {
		cancel()
		return nil, fmt.Errorf("新建标签页成功，但未能定位它的 target ID")
	}

	tab := &Tab{
		ID:      newID,
		Ctx:     tabCtx,
		cancel:  cancel,
		timeout: b.opts.defaultTimeout,
		logger:  b.opts.logger,
	}
	tab.setURL("about:blank")

	b.mu.Lock()
	b.tabs = append(b.tabs, tab)
	b.mu.Unlock()

	// 反检测脚本只对新建的标签页注入；失败不影响使用，仅记录告警
	if err := tab.ensureAntiDetect(tabCtx, b.opts); err != nil {
		b.opts.logger.Warn("注入反检测脚本失败", "tab", newID, "err", err)
	}
	return tab, nil
}

// GetTab 返回指定索引的标签页
func (b *Browser) GetTab(ctx context.Context, index int) (*Tab, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(tabs) {
		return nil, fmt.Errorf("标签页索引 %d 越界（共 %d 个）", index, len(tabs))
	}
	return tabs[index], nil
}

// GetTabByURL 按 URL 包含关系查找标签页
func (b *Browser) GetTabByURL(ctx context.Context, substr string) (*Tab, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	for _, tab := range tabs {
		if contains(tab.URL(), substr) {
			return tab, nil
		}
	}
	return nil, fmt.Errorf("未找到 URL 包含 %q 的标签页", substr)
}

// LatestTab 返回最后打开的标签页
func (b *Browser) LatestTab(ctx context.Context) (*Tab, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, ErrNoTab
	}
	return tabs[len(tabs)-1], nil
}

// CloseTab 显式关闭某个标签页
func (b *Browser) CloseTab(ctx context.Context, tab *Tab) {
	if tab == nil {
		return
	}

	// 先在锁外真正关闭 Chrome 里的 target（CDP 往返不持锁）
	//
	// 调用方传裸 context 时回退到 rootCtx，避免 chromedp.Run 另起临时浏览器
	runCtx := ctx
	if chromedp.FromContext(ctx) == nil {
		runCtx = b.rootCtx
	}
	_ = chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return target.CloseTarget(tab.ID).Do(ctx)
	}))
	if tab.cancel != nil {
		tab.cancel()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, t := range b.tabs {
		if t.ID == tab.ID {
			b.tabs = append(b.tabs[:i], b.tabs[i+1:]...)
			break
		}
	}
}

// Close 断开连接并释放资源。默认不关标签页。
// 属于 teardown 操作，遵循 io.Closer 惯例不接受 ctx。
//
// 释放顺序是刻意安排的：隔离上下文要靠 CDP 的 disposeBrowserContext 销毁，
// 必须在 rootCtx 仍然存活时执行，因此排在 rootCancel 之前。
func (b *Browser) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	// 同步把连接标志置回未连接：ensureConnected 先看 closed，
	// 但它随后还会检查 rootCtx/rootCancel，两个状态保持一致才不会有中间态
	b.connMu.Lock()
	b.connected = false
	b.connMu.Unlock()

	// 1) 取出隔离上下文快照并清空注册表，随后在锁外逐个销毁（销毁是 I/O，不宜持锁）
	b.ctxMu.Lock()
	ctxs := make([]*BrowserContext, 0, len(b.contexts))
	for _, bc := range b.contexts {
		ctxs = append(ctxs, bc)
	}
	b.contexts = make(map[string]*BrowserContext)
	b.ctxMu.Unlock()

	// 2) 释放各标签页上下文
	tabs := b.tabs
	b.tabs = nil
	b.mu.Unlock()

	for _, bc := range ctxs {
		bc.Close()
	}
	for _, t := range tabs {
		if t.cancel != nil {
			t.cancel()
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 3) 释放常驻 rootCtx（会关闭其锚点空白标签页），再释放 allocator
	if b.rootCancel != nil {
		b.rootCancel()
		b.rootCancel = nil
	}
	b.rootCtx = nil
	if b.allocCancel != nil {
		b.allocCancel()
		b.allocCancel = nil
	}
	// 如果是自己启动的 Chrome，杀掉整棵进程树（含 renderer/gpu/crashpad 等子进程），
	// 否则残留子进程会继续占用 user-data-dir 文件锁，导致下次启动报「无法读写数据目录」。
	if b.launched {
		killProcessTree(b.chromeCmd)
		b.launched = false
	}
	b.chromeCmd = nil
	// 进程树杀完后再释放数据目录排他锁，让等待方能立刻接管
	if b.lock != nil {
		b.lock.release()
		b.lock = nil
	}
}

// OpenPage 创建并连接浏览器，返回一个可用标签页
// port 传 0 表示随机端口，传具体值表示固定端口
// 无论是新启动还是接管已有 Chrome，都优先复用最近打开的标签页，没有则新建一个
func OpenPage(ctx context.Context, port int, opts ...Option) (*Browser, *Tab, error) {
	b := NewBrowser(port, opts...)
	if err := b.Connect(ctx); err != nil {
		b.Close() // 连接失败也要清理：若已启动 Chrome 则结束进程，避免汄漏僵尸实例
		return nil, nil, err
	}

	// 优先获取最新（最后打开）的标签页；若当前没有任何标签页
	// （启动瞬间 target 未就绪、或已有浏览器标签全被关掉），则新建一个。
	tab, err := b.LatestTab(ctx)
	if err != nil {
		tab, err = b.NewTab(ctx)
		if err != nil {
			b.Close()
			return nil, nil, err
		}
	}

	// 仅对新启动的 Chrome 清理多余标签页（如初始空白页）；
	// 接管已有 Chrome 时绝不能关闭用户自己开的其他标签页。
	if b.launched {
		b.mu.Lock()
		keep := tab.ID
		var others []*Tab
		for _, t := range b.tabs {
			if t.ID != keep {
				others = append(others, t)
			}
		}
		b.tabs = []*Tab{tab}
		b.mu.Unlock()

		// 在锁外关闭其他标签，避免长时间持锁
		for _, t := range others {
			_ = chromedp.Run(t.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				return target.CloseTarget(t.ID).Do(ctx)
			}))
			if t.cancel != nil {
				t.cancel()
			}
		}
	}

	// 复用来的标签页（接管已有 Chrome、或新启动时的初始空白页）没有经过 NewTab，
	// 这里补一次注入；ensureAntiDetect 幂等，NewTab 已注入过的不会重复执行。
	if err := tab.ensureAntiDetect(tab.Ctx, b.opts); err != nil {
		b.opts.logger.Warn("注入反检测脚本失败", "tab", tab.ID, "err", err)
	}

	return b, tab, nil
}
