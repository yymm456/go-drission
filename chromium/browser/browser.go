package browser

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/chrome"
	"github.com/yymm456/go-drission/chromium/config"
	"github.com/yymm456/go-drission/chromium/errs"
	"github.com/yymm456/go-drission/chromium/page"
)

// Browser 持有浏览器连接与全部标签页。
//
// # 锁序
//
// 三把锁必须按固定顺序获取，否则会成环：
//
//	mu（tabs / closed） → connMu（连接状态） → ctxMu（contexts 注册表）
//
// Close 是唯一同时取 mu 与 ctxMu 的路径，且会先释放 mu 再做销毁 I/O。
type Browser struct {
	port int
	opts *config.Options

	allocCtx    context.Context
	allocCancel context.CancelFunc
	// rootCtx 是常驻浏览器上下文：所有标签页上下文从它派生，以共享同一条 browser 连接
	// （chromedp 要求，否则新建标签页失败）。
	// rootTargetID 是 rootCtx 建连时 chromedp 自动创建的 about:blank 锚点 target，
	// 仅用于建连与读取默认上下文 ID，不纳入标签页管理，第一个真实标签页就绪后由
	// closeAnchorOnce 关闭。
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	rootTargetID target.ID
	// anchorOnce 保证锚点空白页只关闭一次。
	anchorOnce sync.Once
	// defaultBrowserContextID 是默认浏览器上下文的 ID，Connect 时从锚点 target 读取一次，之后只读。
	// Target.getTargets 给每个 target 带 browserContextId，但默认上下文的 ID 不一定是空串
	// （Chrome 会给默认上下文分配 GUID），故按「等于锚点所在上下文」过滤，而非「等于空串」。
	defaultBrowserContextID cdp.BrowserContextID
	launched                bool
	chromeCmd               *exec.Cmd           // 启动的 Chrome 进程，Close 时用于杀进程
	lock                    *chrome.ProfileLock // 数据目录排他锁，仅自己启动 Chrome 时持有
	closed                  bool

	mu   sync.Mutex
	tabs []*page.Tab

	// targetListMu 串行所有「基于 target 快照修改 b.tabs」的操作：NewTab（建 target →
	// 比对前后快照定位新 ID → 登记）与 Tabs（取快照 → syncTabs）。
	//
	// 两者必须互斥（正确性要求）：syncTabs 会把「快照里没有、但 b.tabs 里还在」的标签页
	// cancel 掉，而 chromedp 的上下文取消会真正 CloseTarget。若不互斥，Tabs 可能拿旧快照
	// 把 NewTab 刚建好的标签页关掉。互斥后 NewTab 释放锁时 target 与 b.tabs 已一致，
	// Tabs 的快照必然覆盖 b.tabs。
	//
	// b.tabs 自身的读写仍由 mu 保护，锁序 targetListMu → mu。
	targetListMu sync.Mutex

	// connMu 保护连接状态字段，与 mu 分开，避免握手长时间持锁。
	connMu    sync.Mutex
	connected bool

	// ctxMu 保护 contexts（命名隔离上下文注册表），与 mu 分开以降低锁竞争。
	ctxMu    sync.Mutex
	contexts map[string]*BrowserContext
}

// ensureConnected 校验浏览器处于可用状态，供 NewTab / Context 等入口统一拦截「未 Connect 就使用」。
//
// 判据是 connMu 下的 connected 标志，且要求 rootCtx / rootCancel 都在位。不能只判
// rootCtx != nil：initRootContext 先赋值 rootCtx 再 Run，Run 失败时清理分支只置空
// rootCancel、留下非 nil 的 rootCtx，会让后续操作作用在已死的上下文上。
//
// 调用方需持有 b.mu（读 b.closed）。
func (b *Browser) ensureConnected() error {
	b.connMu.Lock()
	connected := b.connected
	b.connMu.Unlock()

	if b.closed {
		return errs.ErrClosed
	}
	if !connected || b.rootCtx == nil || b.rootCancel == nil {
		return errs.ErrNotConnected
	}
	return nil
}

// NewBrowser 创建 Browser。port 传 0 为随机端口，否则为固定端口。
func NewBrowser(port int, opts ...config.Option) *Browser {
	o := config.Defaults()
	for _, opt := range opts {
		opt(o)
	}

	if port == 0 {
		if free, err := chrome.FindFreePort(); err == nil {
			port = free
		} else {
			port = 9222 // 分配失败时退回默认端口
		}
	}

	// 用户数据目录：未显式指定时按端口生成默认路径（须在端口确定之后）。
	// 必须转为绝对路径：相对路径会让 Chrome handoff 到已有会话而不新开实例，调试端口永远无法就绪。
	if o.UserDataDir == "" {
		o.UserDataDir = chrome.DefaultUserDataDir(port)
	} else if abs, err := filepath.Abs(o.UserDataDir); err == nil {
		o.UserDataDir = abs
	}

	return &Browser{
		port:     port,
		opts:     o,
		contexts: map[string]*BrowserContext{},
	}
}

// removeContext 从命名上下文注册表移除指定上下文。仅持 ctxMu，不触碰 mu，避免与 Close 成锁序环。
func (b *Browser) removeContext(name string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	delete(b.contexts, name)
}

// connectedRootCtx 在锁内取「已连接的常驻上下文」快照并返回 rootCtx。
//
// 必须在锁内取且与后续派生紧挨着：并发的 Close 会把 rootCtx 置 nil，
// 而 chromedp.NewContext(nil) 会 panic。
func (b *Browser) connectedRootCtx() (context.Context, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConnected(); err != nil {
		return nil, err
	}
	return b.rootCtx, nil
}

// boundedRootCtx 基于常驻 rootCtx 派生运行上下文：既携带 browser 级 CDP 路由，
// 又受调用方 ctx 的取消与超时约束。browser 级命令必须走 rootCtx 分支，但无超时的
// rootCtx 会在 Chrome 卡死时永久阻塞。
//
// 规则：调用方 ctx 带 deadline 时取 min(cdpkit.DefaultCallTimeout, 剩余时间)，否则套用默认超时；
// 调用方 ctx 取消时同步取消。返回的 cancel 须由调用方 defer。取消 runCtx 只结束本次调用，
// 不影响 rootCtx 上的长连接。
//
// rootCtx 已被 Close 置 nil 时返回一个立即结束的上下文，使后续 Run 以错误返回而非 panic。
func (b *Browser) boundedRootCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	parent := b.rootCtx
	if parent == nil {
		dead, cancel := context.WithCancel(ctx)
		cancel()
		return dead, cdpkit.NoopCancel
	}
	runCtx, cancel := context.WithTimeout(parent, cdpkit.BudgetDuration(ctx))
	// runCtx 的父是 rootCtx 而非 ctx，调用方对 ctx 的取消传导不过来，用 AfterFunc 接上；
	// stop() 能立刻解除注册，比常驻 select goroutine 干净。
	stop := context.AfterFunc(ctx, cancel)
	return runCtx, func() {
		stop()
		cancel()
	}
}

// tabInitBudget 返回「建立标签页会话」（新建 / attach target）的等待预算：
// min(调用方剩余时间, cdpkit.DefaultCallTimeout)，与 boundedRootCtx 共用超时策略。
//
// 它只能作 cdpkit.RunAbandonable 的 watchCtx，绝不能直接用于 chromedp.Run：
// chromedp 在 attach 时会把 Target 事件分发 goroutine 绑到传入的 ctx，若该 ctx 会被
// 取消 / 超时，标签页之后所有命令都等不到应答（表现为句柄已拿到但每个操作都卡到超时）。
// 正确做法是把 Run 留在长生命周期的 tabCtx 上，只让调用方按预算决定「放弃等待」，
// Chrome 无响应时调用方不会被永久挂住（BUG-07）。
func tabInitBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	// 挂到调用方 ctx 而非 rootCtx：该预算只决定「何时放弃等待」，不参与 CDP 路由。
	return context.WithTimeout(ctx, cdpkit.BudgetDuration(ctx))
}

// newTabCtx 从常驻 rootCtx 派生标签页上下文并完成首次 Run（新建或 attach target）。
//
// Browser.NewTab、BrowserContext.NewTab、attachTarget 三个入口共用此流程。Run 必须留在
// 长生命周期的 tabCtx 上，超时只加在等待预算（tabInitBudget）上，绝不能把超时子上下文
// 直接喂给 chromedp.Run（会掐断 target 事件分发 goroutine，见 BUG-07）。
//
// opts 用于 attach 已有 target（chromedp.WithTargetID），新建时不传。
// 失败时内部已 cancel 派生的子上下文，调用方无需清理。
func (b *Browser) newTabCtx(ctx context.Context, opts ...chromedp.ContextOption) (context.Context, context.CancelFunc, error) {
	// 锁内取 rootCtx 快照：并发 Close 会置 nil，chromedp.NewContext(nil) 会 panic
	rootCtx, err := b.connectedRootCtx()
	if err != nil {
		return nil, nil, err
	}
	tabCtx, cancel := chromedp.NewContext(rootCtx, opts...)
	budget, cancelBudget := tabInitBudget(ctx)
	defer cancelBudget()
	if err := cdpkit.RunAbandonable(budget, tabCtx, func(runCtx context.Context) error {
		return chromedp.Run(runCtx)
	}); err != nil {
		cancel()
		return nil, nil, err
	}
	return tabCtx, cancel, nil
}

// Port 返回当前调试端口。
func (b *Browser) Port() int {
	return b.port
}

// PID 返回本实例启动的 Chrome 主进程 PID；接管已有 Chrome 时返回 0。
func (b *Browser) PID() int {
	if b.chromeCmd == nil || b.chromeCmd.Process == nil {
		return 0
	}
	return b.chromeCmd.Process.Pid
}

// Connect 探测端口：存活则连接，空闲则启动 Chrome。
//
// ctx 控制整个握手阶段（探测 / 启动 / 首次连接），取消或到期即中止；未设 deadline 时
// 退回 opts.ConnectTimeout（默认 10s）。
//
// 握手结束后长连接绑定到内部 background ctx 而非调用方 ctx：连接生命周期应随 Browser
// 由 Close 回收，绑在一次性 ctx 上会导致 ctx 到期后所有操作报 context canceled。
func (b *Browser) Connect(ctx context.Context) error {
	b.connMu.Lock()
	// connHeld 标记 connMu 是否仍持有：失败回滚须先释放它再按锁序取 mu，释放后不得重复解锁。
	connHeld := true
	defer func() {
		if connHeld {
			b.connMu.Unlock()
		}
	}()

	if b.connected {
		// 重复连接会覆盖并泄漏上一条 allocator 与连接，显式拒绝
		return errs.ErrAlreadyConnected
	}
	if b.closed {
		return errs.ErrClosed
	}

	// 握手超时：ctx 自带 deadline 时以调用方为准，否则套用 connectTimeout
	handshakeCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		timeout := b.opts.ConnectTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		var cancel context.CancelFunc
		handshakeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if chrome.IsPortAlive(handshakeCtx, b.port) {
		b.opts.Logger.Info("检测到已有 Chrome，直接连接", "port", b.port)
		b.launched = false
		// 核实端口上的 Chrome 确实是期望的档案：否则显式指定的 WithUserDataDir 会被静默忽略，
		// 接管到别人的浏览器（别的登录态），多账号隔离失效。
		if err := chrome.VerifyPortOwner(b.opts, b.port); err != nil {
			return err
		}
	} else {
		b.opts.Logger.Info("未检测到 Chrome，正在启动", "port", b.port)
		// 先取数据目录的 OS 级排他锁：两实例并发用同一目录启动时，后启动者会弹
		// 「无法读写数据目录」对话框，这里提前转为明确的 Go 错误。
		lock, err := chrome.AcquireProfileLock(b.opts.UserDataDir)
		if err != nil {
			return err
		}
		cmd, err := chrome.LaunchChrome(handshakeCtx, b.port, b.opts)
		if err != nil {
			lock.Release()
			return err
		}
		b.lock = lock
		b.launched = true
		b.chromeCmd = cmd
		// 写档案标记，供下次接管核实端口浏览器属于哪个目录。写入失败不阻断启动，仅记日志。
		if err := chrome.WriteProfileMarker(b.opts.UserDataDir, b.port, b.PID()); err != nil {
			b.opts.Logger.Warn("写入档案标记失败，下次接管将无法核实用户数据目录",
				"dir", b.opts.UserDataDir, "err", err)
		}
	}

	b.allocCtx, b.allocCancel = chromedp.NewRemoteAllocator(
		context.Background(),
		fmt.Sprintf("http://127.0.0.1:%d", b.port),
	)

	if err := b.initRootContext(handshakeCtx); err != nil {
		// 连接未建成：就地回收本次 Connect 申请的全部资源，不依赖调用方随后调 Close。
		// 否则会泄漏僵尸 Chrome 进程，且数据目录锁一直被占，导致下次启动失败。
		//
		// 锁序：本方法进来即持 connMu，而 releaseConnectionResources 要求持 mu。
		// 全局锁序是 mu → connMu → ctxMu，Close 又在 mu 临界区内取 connMu，
		// 故此处必须先释放 connMu 再取 mu，否则 ABBA 死锁。
		b.connMu.Unlock()
		connHeld = false

		b.mu.Lock()
		b.releaseConnectionResources()
		b.mu.Unlock()
		return err
	}
	b.connected = true
	b.opts.Logger.Info("已连接 Chrome", "port", b.port, "launched", b.launched)
	return nil
}

// initRootContext 建立常驻浏览器上下文 rootCtx，并借此确认 Chrome 可连。
//
// chromedp 中只有从「已分配 browser 连接的上下文」派生的子上下文才共享该连接并能
// createTarget 新建标签页；直接从 allocator 派生会各自新建连接，导致 createTarget 失败。
// 因此先建立并 Run 一个 rootCtx 作为所有标签页上下文的父级。rootCtx 自身占用一个空白
// 锚点 target（rootTargetID），不纳入标签页管理，第一个真实标签页就绪后由 closeAnchorOnce 关闭。
//
// ctx 仅用于首次 Run 的兜底超时，不作为连接的生命周期 ctx（见 cdpkit.RunAbandonable）。
func (b *Browser) initRootContext(ctx context.Context) error {
	b.rootCtx, b.rootCancel = chromedp.NewContext(b.allocCtx)

	// 首次 Run 不能包超时子 ctx：RemoteAllocator 把连接生命周期绑在首次 Run 的 ctx 上，取消即断连。
	err := cdpkit.RunAbandonable(ctx, b.rootCtx, func(runCtx context.Context) error {
		return chromedp.Run(runCtx, chromedp.ActionFunc(func(context.Context) error {
			return nil
		}))
	})
	if err != nil {
		return fmt.Errorf("连接 Chrome (端口 %d) 失败: %w", b.port, err)
	}

	// 记录 rootCtx 自身占用的锚点 target，后续同步标签页时跳过它
	var info *target.Info
	_ = cdpkit.RunAbandonable(ctx, b.rootCtx, func(runCtx context.Context) error {
		return chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
			var e error
			info, e = target.GetTargetInfo().Do(c)
			return e
		}))
	})
	if info != nil {
		b.rootTargetID = info.TargetID
		// 锚点 target 落在默认上下文，其 browserContextId 即默认上下文 ID。
		// 读取失败会使 listTargets 无法排除锚点页，故单独告警。
		b.defaultBrowserContextID = info.BrowserContextID
	} else {
		b.opts.Logger.Warn("未能读取锚点 target 信息，标签页列表可能包含锚点空白页")
	}
	return nil
}

// closeAnchorOnce 关闭 chromedp 建连时自动创建的 about:blank 锚点标签页（rootTargetID）。
// 它不承载业务，却会在窗口留下一个永久空白页。
//
// 关闭是安全的：连接生命周期绑在 rootCtx 而非该 target，browser 级 CDP 调用走 Browser
// executor，子标签页各自新建 target。CloseTarget 必须走 Browser executor，用默认 Target
// executor 会被 chromedp 拒绝。
//
// 必须用 sync.Once 且在已存在真实标签页之后调用：若锚点是最后一个 target，关掉它会让
// 非 headless 的 Chrome 退出。
func (b *Browser) closeAnchorOnce(ctx context.Context) {
	b.anchorOnce.Do(func() {
		id := b.rootTargetID
		if id == "" {
			return
		}
		runCtx, cancel := b.boundedRootCtx(ctx)
		defer cancel()
		err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
			return target.CloseTarget(id).Do(cdp.WithExecutor(c, chromedp.FromContext(c).Browser))
		}))
		if err != nil {
			b.opts.Logger.Warn("关闭锚点空白标签页失败", "target", string(id), "err", err)
		}
	})
}

// guard 取一次「连接可用」快照，供随后在锁外做 I/O 的方法使用。需持 b.mu 读 closed。
func (b *Browser) guard() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ensureConnected()
}

// Tabs 返回当前浏览器里所有 page 类型的标签页。
//
// 在锁外查询 target 列表并附着新标签页，避免端口卡顿时串行阻塞所有标签页操作。
// 「取快照 + 同步」整体与 NewTab 互斥（targetListMu），否则一次 Tabs() 可能把并发
// 新建的标签页 cancel 掉（详见 targetListMu 字段注释）。
func (b *Browser) Tabs(ctx context.Context) ([]*page.Tab, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}

	b.targetListMu.Lock()
	defer b.targetListMu.Unlock()

	infos, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}
	return b.syncTabs(ctx, infos)
}

// NewTab 新建并托管一个标签页。
//
// I/O 全程在锁外，仅登记时短暂持锁。「新建 target → 比对前后列表定位新 ID → 登记」整段
// 须与 Tabs()（取快照 + syncTabs）互斥（targetListMu），否则并发 NewTab 会互相认错 target，
// Tabs() 还会用旧快照把刚建好的 target 当作已关闭取消。targetListMu 不保护 b.tabs 状态
// （那是 mu 的职责），故不与只读 b.tabs 的路径争锁。
func (b *Browser) NewTab(ctx context.Context) (*page.Tab, error) {
	b.targetListMu.Lock()
	defer b.targetListMu.Unlock()

	before, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}

	// 从常驻 rootCtx 派生子上下文：共享 browser 连接，Run 时 createTarget 新建标签页。
	// 首次 Run 与超时预算见 newTabCtx（BUG-07）。
	tabCtx, cancel, err := b.newTabCtx(ctx)
	if err != nil {
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

	tab := page.NewTabHandle(tabCtx, newID, cancel, b.opts)
	page.SetTabURL(tab, "about:blank")

	b.mu.Lock()
	b.tabs = append(b.tabs, tab)
	b.mu.Unlock()

	// 已有真实标签页落地，可安全关闭建连时的锚点空白页
	b.closeAnchorOnce(ctx)

	// 反检测脚本只对新建标签页注入；失败不影响使用，仅告警
	if err := page.InjectAntiDetect(tabCtx, tab, b.opts); err != nil {
		b.opts.Logger.Warn("注入反检测脚本失败", "tab", newID, "err", err)
	}
	return tab, nil
}

// GetTab 返回指定索引的标签页
func (b *Browser) GetTab(ctx context.Context, index int) (*page.Tab, error) {
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
func (b *Browser) GetTabByURL(ctx context.Context, substr string) (*page.Tab, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	for _, tab := range tabs {
		if strings.Contains(tab.URL(), substr) {
			return tab, nil
		}
	}
	return nil, fmt.Errorf("未找到 URL 包含 %q 的标签页", substr)
}

// LatestTab 返回最后打开的标签页
func (b *Browser) LatestTab(ctx context.Context) (*page.Tab, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, errs.ErrNoTab
	}
	return tabs[len(tabs)-1], nil
}

// CloseTab 显式关闭某个标签页
func (b *Browser) CloseTab(ctx context.Context, tab *page.Tab) {
	if tab == nil {
		return
	}

	// 先在锁外关闭 Chrome 里的 target（CDP 往返不持锁）。
	// 调用方传裸 context 时回退到 rootCtx，避免 chromedp.Run 另起临时浏览器。
	runCtx := ctx
	if chromedp.FromContext(ctx) == nil {
		runCtx = b.rootCtx
	}
	// 同样套默认超时：Chrome 无响应时 CloseTab 不该把调用方永久挂住（见 BUG-07）。
	runCtx, cancelRun := cdpkit.WithDefaultTimeout(runCtx, cdpkit.DefaultCallTimeout)
	defer cancelRun()
	_ = chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return target.CloseTarget(tab.ID).Do(ctx)
	}))
	page.ReleaseTab(tab)

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, t := range b.tabs {
		if t.ID == tab.ID {
			b.tabs = append(b.tabs[:i], b.tabs[i+1:]...)
			break
		}
	}
}

// releaseConnectionResources 释放一次连接占用的全部资源，顺序固定：
// rootCtx（含其锚点空白页）→ allocator → Chrome 进程树 → 数据目录排他锁。
//
// 调用方必须持有 b.mu（本方法读写 rootCtx / allocCtx / launched / chromeCmd / lock 等受 mu 保护的字段）。
// 两个调用点共用：Connect 失败回滚与 Close 第 3 步。
//
// 幂等：每个句柄「判空 → 用 → 置 nil」，重复调用不 panic，也不会重复 kill 或重复释放锁
// （teardown 路径天然会被重复触发）。
func (b *Browser) releaseConnectionResources() {
	if b.rootCancel != nil {
		b.rootCancel()
		b.rootCancel = nil
	}
	// rootCtx 也须置 nil：只清 rootCancel 会留下「rootCtx 非 nil 但连接已死」的中间态，
	// 使 ensureConnected 误判为已连接。
	b.rootCtx = nil
	if b.allocCancel != nil {
		b.allocCancel()
		b.allocCancel = nil
	}
	// 自己启动的 Chrome 须杀整棵进程树（含 renderer/gpu/crashpad 等子进程），
	// 否则残留子进程继续占用 user-data-dir 文件锁，导致下次启动失败。
	if b.launched {
		chrome.KillProcessTree(b.chromeCmd)
		b.launched = false
	}
	b.chromeCmd = nil
	// 进程树杀完后再释放数据目录锁，让等待方立刻接管
	if b.lock != nil {
		b.lock.Release()
		b.lock = nil
	}
}

// Close 断开连接并释放资源，默认不关标签页。属 teardown，遵循 io.Closer 惯例不接受 ctx。
//
// 释放顺序固定：隔离上下文靠 CDP disposeBrowserContext 销毁，须在 rootCtx 存活时执行，
// 故排在 rootCancel 之前。
func (b *Browser) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	// 同步置回未连接标志：ensureConnected 先看 closed，随后还查 rootCtx/rootCancel，须保持一致
	b.connMu.Lock()
	b.connected = false
	b.connMu.Unlock()

	// 1) 取隔离上下文快照并清空注册表，随后在锁外逐个销毁（销毁是 I/O）
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
		page.ReleaseTab(t)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 3) 释放常驻 rootCtx（含其锚点空白页）、allocator、Chrome 进程树与数据目录锁，与 Connect 失败回滚共用实现
	b.releaseConnectionResources()
}

// OpenPage 创建并连接浏览器，返回一个可用标签页。port 传 0 为随机端口，否则固定。
// 无论新启动还是接管，都优先复用最近打开的标签页，没有则新建。
func OpenPage(ctx context.Context, port int, opts ...config.Option) (*Browser, *page.Tab, error) {
	b := NewBrowser(port, opts...)
	if err := b.Connect(ctx); err != nil {
		b.Close() // 连接失败也要清理：结束已启动的 Chrome，避免泄漏僵尸实例
		return nil, nil, err
	}

	// 优先取最新标签页；若当前没有任何标签页（启动瞬间未就绪、或标签全被关），则新建
	tab, err := b.LatestTab(ctx)
	if err != nil {
		tab, err = b.NewTab(ctx)
		if err != nil {
			b.Close()
			return nil, nil, err
		}
	}

	// 仅对新启动的 Chrome 清理多余标签页；接管已有 Chrome 时绝不能关用户自己开的标签页
	if b.launched {
		b.closeOtherTabs(ctx, tab)
	}

	// 复用来的标签页未经 NewTab，这里补注入；ensureAntiDetect 幂等，已注入的不会重复执行
	if err := page.InjectAntiDetect(tab.Ctx, tab, b.opts); err != nil {
		b.opts.Logger.Warn("注入反检测脚本失败", "tab", tab.ID, "err", err)
	}

	return b, tab, nil
}

// closeOtherTabs 关闭除 keep 外所有本 Browser 托管的标签页。仅用于新启动 Chrome 后清理
// 多余初始空白页；接管已有 Chrome 时绝不能调用。
//
// 只在锁内挑出 keep 之外的标签页，关闭与摘除都交给 CloseTab（已有一份实现），故此处不写回 b.tabs。
func (b *Browser) closeOtherTabs(ctx context.Context, keep *page.Tab) {
	if keep == nil {
		return
	}

	b.mu.Lock()
	others := make([]*page.Tab, 0, len(b.tabs))
	for _, t := range b.tabs {
		if t.ID != keep.ID {
			others = append(others, t)
		}
	}
	b.mu.Unlock()

	// 锁外关闭，避免长时间持锁；由 CloseTab 负责 cancel 并摘除
	for _, t := range others {
		b.CloseTab(ctx, t)
	}
}
