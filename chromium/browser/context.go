package browser

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/errs"
	"github.com/yymm456/go-drission/chromium/page"
)

// ContextOption 用于配置隔离上下文，等价于 CDP Target.createBrowserContext 的参数选项。
// 可直接传入 chromedp.CreateBrowserContextOption，或使用本包提供的便捷封装（如 WithContextProxy）。
type ContextOption = chromedp.CreateBrowserContextOption

// WithContextProxy 为隔离上下文设置独立代理（各上下文互不影响），
// 例如 WithContextProxy("http://127.0.0.1:7891")。
func WithContextProxy(proxy string) ContextOption {
	return func(p *target.CreateBrowserContextParams) *target.CreateBrowserContextParams {
		return p.WithProxyServer(proxy)
	}
}

// BrowserContext 是同一 Chrome 实例内的一个隔离上下文（类似无痕窗口，但可并存多个）。
// 拥有独立的 Cookie / 缓存 / 存储，可选独立代理；同一上下文内可开多个标签页共享登录态。
//
// 与 ProfileManager（每档案一个独立 Chrome 进程）相比更轻量：多账户共用一个浏览器进程，
// 创建/销毁快、占用低。代价是内存态——Chrome 关闭后登录态不保留（如需持久化，配合
// Tab.ExportCookies / ImportCookies 在关闭前导出、新建后导入）。
//
// 实现：不用 chromedp.WithNewBrowserContext（其 createTarget 未带 newWindow，Chrome 153+ 报
// "no browser is open"），改为手动编排 CDP：createBrowserContext → createTarget(newWindow=true)
// → WithTargetID attach，销毁时 disposeBrowserContext。首个 target 必须开新窗口，后续 target 才能落入。
//
// 生命周期：调用 Close 销毁整个上下文（其下所有标签页与 Cookie 一并清除）。
type BrowserContext struct {
	name    string
	browser *Browser

	// bcID 是 CDP Target.createBrowserContext 返回的隔离上下文 ID。
	bcID cdp.BrowserContextID

	// 首个 target：创建上下文时用 newWindow=true 预先建好（Chrome 要求首个 target 必须开新窗口），
	// NewTab 首次调用时消费它。
	firstTargetID  target.ID
	firstTabCtx    context.Context
	firstTabCancel context.CancelFunc

	mu        sync.Mutex
	firstUsed bool
	tabs      []*page.Tab
	closed    bool
}

// Context 按名字返回一个隔离上下文；同名复用，不存在则新建。
// opts 可传入 WithContextProxy 等选项配置该上下文（仅在首次创建时生效）。
//
// 典型用法（多账户，共用一个浏览器进程）：
//
//	ctxA, _ := browser.Context(ctx, "account_001", chromium.WithContextProxy("http://127.0.0.1:7891"))
//	tabA, _ := ctxA.NewTab(ctx)
//
//	ctxB, _ := browser.Context(ctx, "account_002", chromium.WithContextProxy("http://127.0.0.1:7892"))
//	tabB, _ := ctxB.NewTab(ctx) // 与 account_001 完全隔离
func (b *Browser) Context(ctx context.Context, name string, opts ...ContextOption) (*BrowserContext, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Context: 上下文名字不能为空")
	}

	// 先在 mu 下读取连接状态，再释放，避免与 ctxMu 形成嵌套锁
	b.mu.Lock()
	err := b.ensureConnected()
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}

	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()

	if bc, ok := b.contexts[name]; ok && !bc.isClosed() {
		return bc, nil
	}

	// 手动创建隔离 browser context，并在其中开首个 target（必须 newWindow=true）。
	// browser 级 CDP 调用用 boundedRootCtx 约束：保留 browser 路由，又受调用方 ctx 取消/超时保护。
	var bcID cdp.BrowserContextID
	var firstID target.ID
	runCtx, cancelRun := b.boundedRootCtx(ctx)
	defer cancelRun()
	err = chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)

		p := target.CreateBrowserContext()
		for _, o := range opts {
			p = o(p)
		}
		id, e := p.Do(bexec)
		if e != nil {
			return e
		}
		bcID = id

		tid, e := target.CreateTarget("about:blank").
			WithBrowserContextID(id).
			WithNewWindow(true). // 关键：Chrome 153+ 首个 target 不开新窗口会报 no browser is open
			Do(bexec)
		if e != nil {
			_ = target.DisposeBrowserContext(id).Do(bexec) // 回收已建的空上下文
			return e
		}
		firstID = tid
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("创建隔离上下文 %q 失败: %w", name, err)
	}

	// attach 一个 chromedp 上下文到首个 target，作为该上下文的根标签页。
	//
	// 走 newTabCtx 而非就地手写 NewContext + tabInitBudget + RunAbandonable：首次 attach 的
	// 超时约束（BUG-07）与 Browser.NewTab 是同一条规则。ctx 只约束等待预算，Run 仍跑在长命
	// tab 上下文上。失败时回收已建的隔离上下文，避免留下空上下文。
	firstCtx, firstCancel, err := b.newTabCtx(ctx, chromedp.WithTargetID(firstID))
	if err != nil {
		b.disposeBrowserContext(bcID)
		return nil, fmt.Errorf("attach 隔离上下文 %q 首个标签页失败: %w", name, err)
	}

	bc := &BrowserContext{
		name:           name,
		browser:        b,
		bcID:           bcID,
		firstTargetID:  firstID,
		firstTabCtx:    firstCtx,
		firstTabCancel: firstCancel,
	}
	b.contexts[name] = bc
	b.opts.Logger.Info("已创建隔离上下文", "name", name, "browserContextID", string(bcID))
	return bc, nil
}

// disposeBrowserContext 在 browser 级连接上销毁指定的 CDP 隔离上下文。
func (b *Browser) disposeBrowserContext(bcID cdp.BrowserContextID) {
	if bcID == "" {
		return
	}
	// rootCtx 为 nil 说明连接已失效（并发 Close 会置 nil，见 connectedRootCtx）：该 CDP 上下文
	// 随整条连接消失，既无 browser executor 可发 dispose，也无必要再发。少了这步会真实 panic：
	// WithDefaultTimeout(nil,...) 原样返回 nil → chromedp.Run(nil,...) → FromContext(nil) 对 nil
	// 接口调 ctx.Value → nil pointer dereference。触发路径是 teardown 与建号并发。
	//
	// 快照刻意不加锁：本方法经 Browser.Context 在持有 ctxMu 时被调用，而全局锁序是
	// mu → connMu → ctxMu，在此补 b.mu.Lock() 会与 Close「持 mu 等 ctxMu」构成 ABBA。
	rootCtx := b.rootCtx
	if rootCtx == nil {
		return
	}
	// teardown 无调用方 ctx，套用默认超时，避免 Chrome 无响应时 Close 永久阻塞。
	runCtx, cancel := cdpkit.WithDefaultTimeout(rootCtx, cdpkit.DefaultCallTimeout)
	defer cancel()
	_ = chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		return target.DisposeBrowserContext(bcID).Do(bexec)
	}))
}

// Contexts 返回当前所有已创建的隔离上下文名字。
func (b *Browser) Contexts() []string {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()

	names := make([]string, 0, len(b.contexts))
	for name := range b.contexts {
		names = append(names, name)
	}
	return names
}

// Name 返回隔离上下文的名字。
func (bc *BrowserContext) Name() string { return bc.name }

// NewTab 在该隔离上下文内新建并返回一个标签页。
// 首次调用复用创建上下文时预建的 target（那个新窗口），之后每次在同一上下文内新建标签页落入首个窗口。
// 返回的 *Tab 与 Browser 上的 Tab 用法一致（ctx 仍需从 tab.Ctx 派生）。
func (bc *BrowserContext) NewTab(ctx context.Context) (*page.Tab, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if bc.closed {
		return nil, fmt.Errorf("%w: %s", errs.ErrContextClosed, bc.name)
	}

	// 首次：消费创建上下文时预建的首个 target
	if !bc.firstUsed {
		bc.firstUsed = true
		tab := page.NewTabHandle(bc.firstTabCtx, bc.firstTargetID, bc.firstTabCancel, bc.browser.opts)
		page.SetTabURL(tab, "about:blank")
		bc.tabs = append(bc.tabs, tab)
		bc.applyAntiDetect(tab)
		return tab, nil
	}

	// 后续：在同一 browser context 内新建 target（已有窗口，无需 newWindow，作为标签页落入）。
	// browser 级 CDP 调用用 boundedRootCtx 约束，受调用方 ctx 取消/超时保护。
	var tid target.ID
	runCtx, cancelRun := bc.browser.boundedRootCtx(ctx)
	defer cancelRun()
	err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		t, e := target.CreateTarget("about:blank").WithBrowserContextID(bc.bcID).Do(bexec)
		tid = t
		return e
	}))
	if err != nil {
		return nil, fmt.Errorf("隔离上下文内新建标签页失败: %w", err)
	}

	// 派生 tab 上下文并完成 attach；首次 Run 与超时预算的细节见 newTabCtx。
	tabCtx, cancel, err := bc.browser.newTabCtx(ctx, chromedp.WithTargetID(tid))
	if err != nil {
		return nil, err
	}
	tab := page.NewTabHandle(tabCtx, tid, cancel, bc.browser.opts)
	page.SetTabURL(tab, "about:blank")
	bc.tabs = append(bc.tabs, tab)
	bc.applyAntiDetect(tab)
	return tab, nil
}

// applyAntiDetect 给隔离上下文内的新建标签页注入反检测脚本。
//
// 刻意不返回 error：注入属增强能力，失败只告警，绝不让 NewTab 失败。
func (bc *BrowserContext) applyAntiDetect(tab *page.Tab) {
	if err := page.InjectAntiDetect(tab.Ctx, tab, bc.browser.opts); err != nil {
		bc.browser.opts.Logger.Warn("注入反检测脚本失败", "context", bc.name, "tab", tab.ID, "err", err)
	}
}

// Tabs 返回该隔离上下文内当前托管的所有标签页。
func (bc *BrowserContext) Tabs() []*page.Tab {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	out := make([]*page.Tab, len(bc.tabs))
	copy(out, bc.tabs)
	return out
}

// CloseTab 关闭该上下文内的指定标签页。
// 注意：关闭首个（新窗口）标签页可能连同窗口一起关掉，隔离上下文本身仍存活，可继续 NewTab。
func (bc *BrowserContext) CloseTab(ctx context.Context, tab *page.Tab) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	// 通过 browser 级连接关闭目标，不依赖调用方 ctx 是否携带路由；用 boundedRootCtx 约束，
	// Chrome 无响应时不会把调用方永久挂住。
	runCtx, cancelRun := bc.browser.boundedRootCtx(ctx)
	defer cancelRun()
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		return target.CloseTarget(tab.ID).Do(bexec)
	})); err != nil {
		bc.browser.opts.Logger.Warn("关闭隔离上下文内标签页失败", "context", bc.name, "tab", tab.ID, "err", err)
	}
	page.ReleaseTab(tab)
	for i, t := range bc.tabs {
		if t.ID == tab.ID {
			bc.tabs = append(bc.tabs[:i], bc.tabs[i+1:]...)
			break
		}
	}
}

// Close 销毁整个隔离上下文：关闭其下所有标签页并清除 Cookie / 存储。
// 遵循 io.Closer 惯例不接受 ctx。销毁后该名字的上下文可从 Browser 重新创建。
func (bc *BrowserContext) Close() {
	bc.mu.Lock()
	if bc.closed {
		bc.mu.Unlock()
		return
	}
	bc.closed = true
	tabs := bc.tabs
	bc.tabs = nil
	firstUsed := bc.firstUsed
	firstCancel := bc.firstTabCancel
	bcID := bc.bcID
	b := bc.browser
	bc.mu.Unlock()

	// 先释放各标签页的 chromedp 上下文
	for _, t := range tabs {
		page.ReleaseTab(t)
	}
	// 若首个 target 从未被 NewTab 消费，也要释放它的上下文
	if !firstUsed && firstCancel != nil {
		firstCancel()
	}
	// dispose 整个隔离上下文（关闭其下所有 target 与全部 Cookie）
	b.disposeBrowserContext(bcID)
	b.removeContext(bc.name)
	b.opts.Logger.Info("已销毁隔离上下文", "name", bc.name)
}

// isClosed 返回上下文是否已关闭（内部复用判断用）。
func (bc *BrowserContext) isClosed() bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.closed
}
