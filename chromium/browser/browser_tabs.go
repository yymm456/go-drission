package browser

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/errs"
	"github.com/yymm456/go-drission/chromium/page"
)

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
