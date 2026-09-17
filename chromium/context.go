package chromium

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
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

// BrowserContext 是同一个 Chrome 实例内的一个隔离上下文（类似无痕窗口，但可并存多个）。
// 它拥有独立的 Cookie / 缓存 / 存储，并可选独立代理；同一上下文内可开多个标签页共享登录态。
//
// 与 ProfileManager（每个档案一个独立 Chrome 进程）相比，BrowserContext 更轻量：
// 多账户共用一个浏览器进程，创建/销毁快、资源占用低，适合账户数量较多的场景。
// 代价是它是内存态——Chrome 关闭后登录态不保留（如需持久化，可配合 Tab.ExportCookies /
// ImportCookies 在关闭前导出、新建后导入）。
//
// 实现说明：不使用 chromedp.WithNewBrowserContext，因为它内部 createTarget 未带 newWindow，
// 在 Chrome 153+ 上会报 "no browser is open (-32000)"。这里改为手动编排 CDP：
// createBrowserContext → createTarget(browserContextID, newWindow=true) → WithTargetID attach，
// 销毁时 disposeBrowserContext。首个 target 必须开新窗口，后续 target 才能作为标签页落入该窗口。
//
// 生命周期：调用 Close 会销毁整个上下文（其下所有标签页与 Cookie 一并清除）。
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
	tabs      []*Tab
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

	// 先在 mu 下读取 closed 状态，再释放，避免与 ctxMu 形成嵌套锁
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}

	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()

	// 同名复用
	if bc, ok := b.contexts[name]; ok && !bc.isClosed() {
		return bc, nil
	}

	// 手动创建隔离 browser context，并在其中开首个 target（必须 newWindow=true）。
	// browser 级 CDP 调用用 boundedRootCtx 约束：既保留 browser 路由，又受调用方 ctx
	// 取消/超时保护，Chrome 无响应时不会永久阻塞。
	var bcID cdp.BrowserContextID
	var firstID target.ID
	runCtx, cancelRun := b.boundedRootCtx(ctx)
	defer cancelRun()
	err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
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

	// attach 一个 chromedp 上下文到首个 target，作为该上下文的根标签页
	firstCtx, firstCancel := chromedp.NewContext(b.rootCtx, chromedp.WithTargetID(firstID))
	if err := chromedp.Run(firstCtx); err != nil {
		firstCancel()
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
	b.opts.logger.Info("已创建隔离上下文", "name", name, "browserContextID", string(bcID))
	return bc, nil
}

// disposeBrowserContext 在 browser 级连接上销毁指定的 CDP 隔离上下文。
func (b *Browser) disposeBrowserContext(bcID cdp.BrowserContextID) {
	if bcID == "" {
		return
	}
	// teardown 无调用方 ctx，套用默认超时，避免 Chrome 无响应时 Close 永久阻塞。
	runCtx, cancel := context.WithTimeout(b.rootCtx, defaultCDPTimeout)
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

// NewTab 在该隔离上下文内新建一个标签页并返回。
// 首次调用复用创建上下文时预建的 target（那个新窗口）；之后每次调用都在同一上下文内新建标签页，
// 落入首个窗口。返回的 *Tab 与 Browser 上的 Tab 用法完全一致（ctx 仍需从 tab.Ctx 派生）。
func (bc *BrowserContext) NewTab(ctx context.Context) (*Tab, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if bc.closed {
		return nil, fmt.Errorf("隔离上下文 %q 已关闭", bc.name)
	}

	// 首次：消费创建上下文时预建的首个 target
	if !bc.firstUsed {
		bc.firstUsed = true
		tab := &Tab{ID: bc.firstTargetID, Ctx: bc.firstTabCtx, cancel: bc.firstTabCancel, URL: "about:blank"}
		bc.tabs = append(bc.tabs, tab)
		return tab, nil
	}

	// 后续：在同一 browser context 内新建 target（已有窗口，无需 newWindow，作为标签页落入）
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

	tabCtx, cancel := chromedp.NewContext(bc.browser.rootCtx, chromedp.WithTargetID(tid))
	if err := chromedp.Run(tabCtx); err != nil {
		cancel()
		return nil, err
	}
	tab := &Tab{ID: tid, Ctx: tabCtx, cancel: cancel, URL: "about:blank"}
	bc.tabs = append(bc.tabs, tab)
	return tab, nil
}

// Tabs 返回该隔离上下文内当前托管的所有标签页。
func (bc *BrowserContext) Tabs() []*Tab {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	out := make([]*Tab, len(bc.tabs))
	copy(out, bc.tabs)
	return out
}

// CloseTab 关闭该上下文内的指定标签页。
// 注意：关闭首个（新窗口）标签页可能连同窗口一起关掉，隔离上下文本身仍存活，可继续 NewTab。
func (bc *BrowserContext) CloseTab(ctx context.Context, tab *Tab) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	// 通过 browser 级连接关闭目标，避免依赖调用方传入的 ctx 是否携带路由
	_ = chromedp.Run(bc.browser.rootCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		return target.CloseTarget(tab.ID).Do(bexec)
	}))
	if tab.cancel != nil {
		tab.cancel()
	}
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
		if t.cancel != nil {
			t.cancel()
		}
	}
	// 若首个 target 从未被 NewTab 消费，也要释放它的上下文
	if !firstUsed && firstCancel != nil {
		firstCancel()
	}
	// dispose 整个隔离上下文（关闭其下所有 target 与全部 Cookie）
	b.disposeBrowserContext(bcID)
	b.removeContext(bc.name)
	b.opts.logger.Info("已销毁隔离上下文", "name", bc.name)
}

// isClosed 返回上下文是否已关闭（内部复用判断用）。
func (bc *BrowserContext) isClosed() bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.closed
}
