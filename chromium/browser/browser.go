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
	opts *config.Options

	allocCtx    context.Context
	allocCancel context.CancelFunc
	// rootCtx 是常驻的浏览器上下文：所有标签页上下文都作为它的子上下文派生，
	// 以共享同一个 browser 连接（chromedp 要求如此，否则新建标签页会失败）。
	// rootTargetID 是 rootCtx 自身占用的锚点 target——chromedp 用 RemoteAllocator 建连时
	// 会自动 CreateTarget 一个 about:blank 空白页（见 chromedp newTarget 的 !first 分支）。
	// 它只用于建连与读取默认浏览器上下文 ID，不纳入标签页管理，并在第一个真实标签页
	// 就绪后由 closeAnchorOnce 关闭，免得窗口里留下一个永远空白的标签页。
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	rootTargetID target.ID
	// anchorOnce 保证锚点空白页只被关闭一次，见 closeAnchorOnce。
	anchorOnce sync.Once
	// defaultBrowserContextID 是「默认浏览器上下文」的 ID，Connect 时从 rootCtx 的
	// 锚点 target 上取一次，之后只读（与 rootTargetID 同一套约定）。
	//
	// 为什么要存它：CDP 的 Target.getTargets 会给每个 target 带上 browserContextId，
	// 但**默认上下文的 ID 不一定是空串**——实测 Chrome 会给默认上下文也分配一个 GUID，
	// 扩展页面、浏览器内部页面与普通页面共用它。于是「只收 BrowserContextID == ""」
	// 会把所有页面一起滤掉；改成「等于锚点所在上下文」则新旧行为都正确。
	defaultBrowserContextID cdp.BrowserContextID
	launched                bool
	chromeCmd               *exec.Cmd           // 启动的 Chrome 进程，Close 时用于杀进程
	lock                    *chrome.ProfileLock // 数据目录排他锁，仅自己启动 Chrome 时持有
	closed                  bool

	mu   sync.Mutex
	tabs []*page.Tab

	// targetListMu 串行「一切会拿着 target 快照去改动 b.tabs 的操作」：
	// NewTab 的「建 target → 比对前后快照定位新 ID → 登记」，以及 Tabs 的
	// 「取快照 → syncTabs」。
	//
	// 为什么两者必须互斥（这不是性能优化，是正确性要求）：
	// syncTabs 会把「快照里没有、但 b.tabs 里还在」的标签页当成已关闭，调用它的
	// cancel —— 而 chromedp 的上下文取消会**真的 CloseTarget**（见 chromedp.go 里
	// ctx.Done 分支的 target.CloseTarget）。若不互斥，就会出现这样的交错：
	//
	//	Tabs:   取快照（此时新标签页尚未创建，快照里没有 X）
	//	NewTab: createTarget 建出 X，并把 X 登记进 b.tabs，然后返回
	//	Tabs:   拿旧快照跑 syncTabs → b.tabs 里的 X 不在快照中 → cancel → 把 X 关掉
	//
	// 结果是「一次只读的 Tabs() 会把并发新建的标签页销毁掉」，且此后永远不再出现。
	// 互斥之后这个交错不可能发生：NewTab 释放锁时 target 与 b.tabs 已经一致，
	// 因此 Tabs 的快照必然包含 b.tabs 里的每一个 target。
	//
	// 代价只是 NewTab 与 Tabs 互相短暂排队（两三次 getTargets + 一次 createTarget，
	// 毫秒级），换来的是「读操作绝不销毁资源」。b.tabs 自身的读写仍由 mu 保护，
	// 锁序为 targetListMu → mu。
	targetListMu sync.Mutex

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
		return errs.ErrClosed
	}
	if !connected || b.rootCtx == nil || b.rootCancel == nil {
		return errs.ErrNotConnected
	}
	return nil
}

// NewBrowser 创建 Browser
// port 传 0 表示随机端口，传具体值表示固定端口
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

	// 用户数据目录：未显式指定时，按端口生成默认路径（需在端口确定之后）。
	// 必须转为绝对路径：相对路径会让 Chrome handoff 到已存在的浏览器会话，
	// 从而不新开实例，请求的调试端口也就永远无法就绪。
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

// removeContext 从命名上下文注册表中移除指定名字的隔离上下文。
// 仅持有 ctxMu，不触碰 mu，避免与 Browser.Close 产生锁顺序环。
func (b *Browser) removeContext(name string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	delete(b.contexts, name)
}

// connectedRootCtx 取一次「已连接的常驻上下文」快照，并把 rootCtx 一起带出来。
//
// 与 guard 的差别就在这个 rootCtx：调用方紧接着要拿它去 chromedp.NewContext 派生
// 子上下文，而并发的 Close 会把 rootCtx 置 nil——chromedp.NewContext(nil) 内部
// 会 context.WithCancel(nil) 并直接 panic。所以快照必须在锁内取，且与派生紧挨着，
// 否则 guard 通过之后 Close 仍可能插进来。
func (b *Browser) connectedRootCtx() (context.Context, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureConnected(); err != nil {
		return nil, err
	}
	return b.rootCtx, nil
}

// boundedRootCtx 基于常驻 rootCtx 派生一个运行上下文：既携带 browser 级 CDP 路由信息，
// 又受调用方 ctx 的取消与超时约束。browser 级命令必须在 rootCtx 分支上执行（依赖
// FromContext(c).Browser 路由），但直接用无超时的 rootCtx 会在 Chrome 卡死时永久阻塞。
//
// 规则：调用方 ctx 带 deadline 时取 min(cdpkit.DefaultCallTimeout, 剩余时间)；否则套用默认超时；
// 调用方 ctx 被取消时同步取消。返回的 cancel 必须由调用方 defer 调用。
// 取消 runCtx 只结束本次调用，不影响 rootCtx 上的 browser 长连接
// （该连接的生命周期绑定在 initRootContext 首次 Run 的 rootCtx 上）。
//
// 与 Close 并发（rootCtx 已被置 nil）时返回一个立即结束的上下文：后续 chromedp.Run
// 会以错误返回，而不是对 nil 调 context.WithTimeout 直接 panic。
func (b *Browser) boundedRootCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	parent := b.rootCtx
	if parent == nil {
		dead, cancel := context.WithCancel(ctx)
		cancel()
		return dead, cdpkit.NoopCancel
	}
	runCtx, cancel := context.WithTimeout(parent, cdpkit.BudgetDuration(ctx))
	// runCtx 的父是 rootCtx 而不是 ctx，调用方对 ctx 的取消传导不过来，得自己接上。
	// AfterFunc 正好做这件事，且 stop() 能立刻解除注册——比常驻一个 select goroutine 干净。
	stop := context.AfterFunc(ctx, cancel)
	return runCtx, func() {
		stop()
		cancel()
	}
}

// tabInitBudget 返回「建立标签页会话」（新建 / 附着 target）这一步的等待预算：
// min(调用方剩余时间, cdpkit.DefaultCallTimeout)。与 boundedRootCtx 共用同一条超时策略
// （算法见 cdpkit.BudgetDuration）。
//
// 它只能当 cdpkit.RunAbandonable 的 watchCtx 用，**绝不能**直接拿去做 chromedp.Run：
// chromedp 会在 attach 时把 Target 的事件分发 goroutine 绑到传入的 ctx 上
// （见 chromedp.Context.attachTarget 里的 `go c.Target.run(ctx)`）。那个 ctx 若是
// 一个会被取消 / 超时的子上下文，标签页之后所有命令都会永远等不到应答——
// 表现为「标签页建出来了、句柄也拿到了，但每个操作都卡到自己的超时」。
// 这一点已用真实 Chrome 复现过：给 attach 套 30s 超时子上下文后，导航直接卡满 30s。
//
// 正确做法是把 Run 留在长生命周期的 tabCtx 上，只让调用方按这个预算决定
// 「放弃等待」——Chrome 无响应时调用方不会被永久挂住（历史缺陷 BUG-07），
// 标签页本身也不受影响。
func tabInitBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	// 挂到调用方 ctx 上而不是 rootCtx 上：这个预算只用来决定「何时放弃等待」，
	// 不参与 CDP 路由，所以父上下文取调用方的最直观。
	return context.WithTimeout(ctx, cdpkit.BudgetDuration(ctx))
}

// newTabCtx 从常驻 rootCtx 派生一个标签页上下文并完成首次 Run（新建 target 或 attach 已有 target）。
//
// 这套流程在三个入口重复：Browser.NewTab、BrowserContext.NewTab、attachTarget，
// 三处都要求「从 rootCtx 派生 → 首次 Run → 超时只加在等待预算上」。
// 最后一条是 BUG-07 的教训：Run 必须留在长生命周期的 tabCtx 上，只让调用方按
// tabInitBudget 决定「放弃等待」，绝不能把超时子上下文直接喂给 chromedp.Run
// （那会掐断 target 的事件分发 goroutine）。集中在这里，就不会再有人写错第二遍。
//
// opts 用于 attach 已有 target（chromedp.WithTargetID）；新建标签页时不传。
// 失败时内部已 cancel 派生的子上下文，调用方无需再清理。
func (b *Browser) newTabCtx(ctx context.Context, opts ...chromedp.ContextOption) (context.Context, context.CancelFunc, error) {
	// 在锁内取 rootCtx 快照：并发 Close 会把它置 nil，chromedp.NewContext(nil) 会 panic
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
// ctx 未设置 deadline 时，退回 opts.ConnectTimeout（默认 10s）作为握手超时。
//
// 注意：握手结束后，长连接会绑定到内部 background ctx 而非调用方 ctx——
// 连接的生命周期应当跟随 Browser 由 Close() 回收，若绑在调用方的一次性 ctx 上，
// ctx 一到期浏览器连接就被切断，后续所有操作都会报 context canceled。
func (b *Browser) Connect(ctx context.Context) error {
	b.connMu.Lock()
	// connHeld 记录 connMu 是否还握在手里：失败回滚必须先交还它、再按全局锁序去拿 mu，
	// 交还之后这里就不能再重复解锁（原因见下面失败分支的说明）。
	connHeld := true
	defer func() {
		if connHeld {
			b.connMu.Unlock()
		}
	}()

	if b.connected {
		// 重复连接会覆盖并泄漏上一条 allocator 与浏览器连接，显式拒绝而非静默覆盖
		return errs.ErrAlreadyConnected
	}
	if b.closed {
		return errs.ErrClosed
	}

	// 握手超时：ctx 自带 deadline 时完全听调用方的；否则套用 connectTimeout
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
		// 关键一步：核实端口上那个 Chrome 确实是我们期望的档案。
		// 少了这步，显式指定的 WithUserDataDir 会被静默忽略——你以为在用档案 A，
		// 实际接管了别人的浏览器（别的登录态），多账号隔离无声失效。
		if err := chrome.VerifyPortOwner(b.opts, b.port); err != nil {
			return err
		}
	} else {
		b.opts.Logger.Info("未检测到 Chrome，正在启动", "port", b.port)
		// 先拿数据目录的 OS 级排他锁：两个实例并发用同一目录启动 Chrome 时，
		// 后启动者会弹「无法对其数据目录执行读写操作」对话框，这里提前转为明确的 Go 错误。
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
		// 留下档案标记，供下次接管时核实「端口上的浏览器属于哪个目录」。
		// 写入失败不阻断启动，只记日志（最坏结果是下次接管走「无法证实」分支）。
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
		// 连接没建成：就地回收本次 Connect 已申请的全部资源，不依赖调用方随后一定调 Close()。
		// 否则直接 NewBrowser+Connect 的调用方在失败后若不 Close，会泄漏一个僵尸 Chrome 进程，
		// 且数据目录排他锁一直被占，导致下次启动报「无法读写数据目录」。
		// 回收内容与顺序见 releaseConnectionResources（与 Close 的第 3 步共用同一份实现）。
		//
		// 锁序：本方法一进来就握着 connMu，而 releaseConnectionResources 要求持着 mu。
		// 若直接在持 connMu 的状态下补一次 b.mu.Lock()，就变成了「持 connMu 等 mu」，
		// 与本仓库全局锁序 **mu → connMu → ctxMu** 相反；而 Close 恰好是在 mu 临界区里
		// 去拿 connMu 的（Close 第 0 步置 connected=false），两者相遇即 ABBA 死锁。
		// 所以这里先把 connMu 交还，再按正常顺序拿 mu 回收——不是「局部例外」，而是守序。
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

// initRootContext 建立常驻的浏览器上下文 rootCtx，并借此确认 Chrome 真的可连。
//
// chromedp 中，只有从「已分配 browser 连接的上下文」派生的子上下文，才会共享该连接并通过
// Target.createTarget 新建标签页；若直接从 allocator(allocCtx) 派生，每个上下文都会各自新建
// 一条 browser 连接，在已有连接存在时 createTarget 可能失败（no browser is open）。
// 因此这里先建立并 Run 一个 rootCtx 作为所有标签页上下文的父级。rootCtx 自身会占用一个空白
// target 作为锚点（rootTargetID），不纳入标签页管理；它会在第一个真实标签页就绪后由
// closeAnchorOnce 关闭（若始终没有真实标签页，则最迟在 Browser.Close 时随 rootCancel 释放）。
//
// ctx 用于给首次 Run 兜底超时，但不会被用作连接的生命周期 ctx（见 cdpkit.RunAbandonable）。
func (b *Browser) initRootContext(ctx context.Context) error {
	b.rootCtx, b.rootCancel = chromedp.NewContext(b.allocCtx)

	// 首次 Run 必须直接作用于 b.rootCtx，不能包一层「可取消的超时子 ctx」：
	// RemoteAllocator 会把浏览器连接的生命周期绑定到首次 Run 传入的 ctx，
	// 一旦该 ctx 被取消，连接随之关闭，rootCtx 立即失效（后续操作报 context canceled）。
	// 所以用 cdpkit.RunAbandonable：超时只是「放弃等待」，不去取消 rootCtx。
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
		// 锚点 target 就落在默认上下文里，它的 browserContextId 即默认上下文的 ID。
		// 这一步失败（info == nil）会让 listTargets 无法排除浏览器自身的锚点页面，
		// 因此单独告警，便于定位。
		b.defaultBrowserContextID = info.BrowserContextID
	} else {
		b.opts.Logger.Warn("未能读取锚点 target 信息，标签页列表可能包含锚点空白页")
	}
	return nil
}

// closeAnchorOnce 关闭 rootCtx 建立 browser 连接时 chromedp 自动创建的那个 about:blank
// 锚点标签页（rootTargetID）。它只用于建连与读取默认浏览器上下文 ID，不承载任何业务，
// 却会在窗口里留下一个永远空白的标签页。
//
// 关闭它是安全的：browser 连接的生命周期绑定在 rootCtx 上而非这个 target，库内所有
// browser 级 CDP 调用都走 Browser executor（不依赖锚点的 Target executor），子标签页
// 各自新建 target。必须走 Browser executor 下发 CloseTarget：若用默认的 Target executor，
// chromedp 会直接拒绝（"to close the target, cancel its context"）。
//
// 必须用 sync.Once 且**在已经存在一个真实标签页之后**才调用（NewTab / attachTarget
// 成功登记后）：若锚点是浏览器里最后一个 target，关掉它会让非 headless 的 Chrome
// 直接退出，连接随之断开。
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
//
// 「取快照 + 同步」整体与 NewTab 互斥（targetListMu）：syncTabs 会把不在快照里的
// 标签页 cancel 掉，而 cancel 等于 CloseTarget；不互斥的话，一次 Tabs() 就可能
// 把并发新建的标签页真的关掉。详见 targetListMu 字段的注释。
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

// NewTab 新建一个标签页并托管
//
// 全程在锁外做 I/O（HTTP 查询 + CDP 往返），只在最后登记标签页时短暂持锁；
// 但「新建 target → 比对前后 target 列表定位新 ID → 登记进 b.tabs」整段必须与
// Tabs()（取快照 + syncTabs）互斥，否则两个并发的 NewTab 会互相把对方的新 target
// 认成自己的，而且 Tabs() 还会拿旧快照把刚建好的 target 当成已关闭取消掉
// （cancel == CloseTarget）。因此用 targetListMu 保护——它不保护 b.tabs 的状态
// （那是 mu 的职责），所以不会和 GetTab 之类只读 b.tabs 的路径争锁。
func (b *Browser) NewTab(ctx context.Context) (*page.Tab, error) {
	b.targetListMu.Lock()
	defer b.targetListMu.Unlock()

	before, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}

	// 从常驻 rootCtx 派生子上下文：子上下文共享 browser 连接，Run 时通过 createTarget
	// 新建标签页。首次 Run 与超时预算的细节见 newTabCtx（BUG-07 的教训）。
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

	// 已有一个真实标签页落地，可以安全关闭 rootCtx 建连时 chromedp 自动创建的锚点空白页。
	b.closeAnchorOnce(ctx)

	// 反检测脚本只对新建的标签页注入；失败不影响使用，仅记录告警
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

	// 先在锁外真正关闭 Chrome 里的 target（CDP 往返不持锁）
	//
	// 调用方传裸 context 时回退到 rootCtx，避免 chromedp.Run 另起临时浏览器
	runCtx := ctx
	if chromedp.FromContext(ctx) == nil {
		runCtx = b.rootCtx
	}
	// 同样套一层默认超时：Chrome 无响应时 CloseTab 不该把调用方永久挂住
	// （与库内其它 browser 级调用的做法一致，见 BUG-07 一节的说明）。
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

// releaseConnectionResources 释放「一次连接」占用的全部资源，顺序是刻意安排的：
// rootCtx（顺带关掉它的锚点空白标签页）→ allocator → Chrome 进程树 → 数据目录排他锁。
//
// 调用方**必须持有 b.mu**：本方法读写 rootCtx / rootCancel / allocCtx / allocCancel /
// launched / chromeCmd / lock，这些字段都受 mu 保护。
//
// 两个调用点共用它：
//   - Connect 的失败回滚（连接没建成时不能把回收责任推给调用方）；
//   - Close 的第 3 步。
//
// 幂等：每个句柄都是「判空 → 用 → 置 nil」，重复调用不会 panic，
// 也不会重复 kill 进程树或重复释放目录锁——teardown 路径天然会被重复触发
// （Close 之后又走一次失败回滚、Close 与回滚并发等），这是它必须成立的性质。
func (b *Browser) releaseConnectionResources() {
	if b.rootCancel != nil {
		b.rootCancel()
		b.rootCancel = nil
	}
	// rootCtx 也必须一起置 nil：只清 rootCancel 会留下「rootCtx 非 nil 但连接已死」
	// 的中间态，让 ensureConnected 误判为已连接（见该方法注释）。
	b.rootCtx = nil
	if b.allocCancel != nil {
		b.allocCancel()
		b.allocCancel = nil
	}
	// 如果是自己启动的 Chrome，杀掉整棵进程树（含 renderer/gpu/crashpad 等子进程），
	// 否则残留子进程会继续占用 user-data-dir 文件锁，导致下次启动报「无法读写数据目录」。
	if b.launched {
		chrome.KillProcessTree(b.chromeCmd)
		b.launched = false
	}
	b.chromeCmd = nil
	// 进程树杀完后再释放数据目录排他锁，让等待方能立刻接管
	if b.lock != nil {
		b.lock.Release()
		b.lock = nil
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
		page.ReleaseTab(t)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 3) 释放常驻 rootCtx（会关闭其锚点空白标签页）、allocator、Chrome 进程树与数据目录锁。
	//    与 Connect 的失败回滚共用同一份实现，免得「一处补了释放、另一处漏了」。
	b.releaseConnectionResources()
}

// OpenPage 创建并连接浏览器，返回一个可用标签页
// port 传 0 表示随机端口，传具体值表示固定端口
// 无论是新启动还是接管已有 Chrome，都优先复用最近打开的标签页，没有则新建一个
func OpenPage(ctx context.Context, port int, opts ...config.Option) (*Browser, *page.Tab, error) {
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
		b.closeOtherTabs(ctx, tab)
	}

	// 复用来的标签页（接管已有 Chrome、或新启动时的初始空白页）没有经过 NewTab，
	// 这里补一次注入；ensureAntiDetect 幂等，NewTab 已注入过的不会重复执行。
	if err := page.InjectAntiDetect(tab.Ctx, tab, b.opts); err != nil {
		b.opts.Logger.Warn("注入反检测脚本失败", "tab", tab.ID, "err", err)
	}

	return b, tab, nil
}

// closeOtherTabs 关闭除 keep 之外所有由本 Browser 托管的标签页。
// 仅用于新启动 Chrome 后清理多余的初始空白页——接管已有 Chrome 时绝不能调用，
// 否则会关掉用户自己开的标签页。
//
// 只在锁内挑出 keep 之外的标签页，真正的关闭与「从 b.tabs 摘除」都交给 CloseTab：
// 那套「CloseTarget + cancel + 从 b.tabs 摘除」已经有一份实现，不在别处重写。
// 因此这里不再写回 b.tabs——写回是多余的，CloseTab 随即又会逐个摘掉。
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

	// 在锁外关闭，避免长时间持锁；由 CloseTab 负责 cancel 并从 b.tabs 摘除。
	for _, t := range others {
		b.CloseTab(ctx, t)
	}
}
