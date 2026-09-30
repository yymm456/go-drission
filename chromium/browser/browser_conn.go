package browser

import (
	"context"
	"os/exec"
	"sync"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
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
