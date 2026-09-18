package chromium

import (
	"context"
	"fmt"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/yymm456/go-drission/chromium/internal/page"
)

// targetInfo 是「一个归 Browser 托管的 page target」的概要。
type targetInfo struct {
	ID   string
	Type string
	URL  string
}

// listTargets 通过 CDP Target.getTargets 获取「默认浏览器上下文」里的 page target 列表。
//
// 为什么不用 HTTP /json 端点：那个端点**不返回 browserContextId**，隔离上下文
// （CDP BrowserContext）里的页面与默认上下文的页面在它眼里长得一模一样，无从区分。
// 库早期因此把隔离上下文里的标签页也附着成 Browser 级 *Tab：同一个 target 被
// Browser 与 BrowserContext 两套管理器各持一份，GetTab / LatestTab 会返回隔离上下文里的
// 页面，一侧关掉后另一侧还留着陈旧条目（历史缺陷 BUG-06）。
//
// 这里改走 CDP（TargetInfo 带 browserContextId），只收默认上下文的 page target。
//
// 注意默认上下文的判据**不能**写成「BrowserContextID == ""」：实测 Chrome 会给默认
// 上下文也分配一个 GUID（扩展页面、浏览器内部页面与普通页面共用它），按空串过滤会把
// 所有页面一起滤掉，连锚点 target 都不剩。判据改为「等于锚点 target 所在的上下文」，
// 即 b.defaultBrowserContextID（Connect 时从 rootCtx 的锚点 target 上取一次）。
// 这样新旧两种行为都成立：老版本默认上下文 ID 是空串，判据同样成立。
//
// 调用跑在常驻 rootCtx 的 browser 连接上，用 boundedRootCtx 约束：
// 既拿到 browser 级路由，又受调用方 ctx 的取消/超时与 30s 兜底保护。
func (b *Browser) listTargets(ctx context.Context) ([]targetInfo, error) {
	runCtx, cancelRun := b.boundedRootCtx(ctx)
	defer cancelRun()

	var infos []*target.Info
	err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		var e error
		infos, e = target.GetTargets().Do(bexec)
		return e
	}))
	if err != nil {
		return nil, fmt.Errorf("查询 target 列表失败: %w", err)
	}

	out := make([]targetInfo, 0, len(infos))
	for _, info := range infos {
		// 只收默认上下文的 page：隔离上下文的页面归 BrowserContext 管。
		// 判据是与锚点 target 所在上下文比对，不是「为空」——理由见上面的注释。
		if info.Type != "page" || info.BrowserContextID != b.defaultBrowserContextID {
			continue
		}
		out = append(out, targetInfo{ID: string(info.TargetID), Type: info.Type, URL: info.URL})
	}
	return out, nil
}

// syncTabs 把「浏览器里真实存在的 target 列表」同步到托管的标签页集合，
// 并返回按 target 顺序排列的标签页。
//
// infos 必须由调用方在锁外取好（见 guard + listTargets）：查询 target 列表虽然快，
// 但端口卡顿时会一直等，持锁做这件事会把所有标签页操作一起卡住。
//
// ctx 会一路传到 attachTarget：附着外部 target 是一次 CDP 往返，必须受调用方的
// 取消 / 超时约束（配合 tabInitBudget 的 30s 兜底），否则 Chrome 假死时
// Tabs() 会永久挂起。
//
// 内部按「锁内读状态 → 锁外做 I/O → 锁内写状态」三段式组织，
// 附着外部标签页（一次 chromedp 往返）全部发生在锁外。
func (b *Browser) syncTabs(ctx context.Context, infos []targetInfo) ([]*Tab, error) {
	alive := make(map[target.ID]string, len(infos))
	for _, info := range infos {
		if info.Type == "page" {
			alive[target.ID(info.ID)] = info.URL
		}
	}
	// rootTargetID 在 Connect 里写一次，之后只读，无需加锁
	rootID := b.rootTargetID

	// 第一段：锁内只做状态读写——摘掉已消失的标签页、刷新已知标签页的 URL、
	// 记下需要新附着的外部 target。
	known := make(map[target.ID]*Tab, len(b.tabs))
	var pending []target.ID
	func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		kept := make([]*Tab, 0, len(b.tabs))
		for _, t := range b.tabs {
			url, ok := alive[t.ID]
			if !ok {
				page.ReleaseTab(t)
				continue
			}
			page.SetTabURL(t, url)
			known[t.ID] = t
			kept = append(kept, t)
		}
		b.tabs = kept

		for id := range alive {
			// rootCtx 自身的锚点 target，不作为普通标签页管理
			if id == rootID {
				continue
			}
			if _, ok := known[id]; !ok {
				pending = append(pending, id)
			}
		}
	}()

	// 第二段：锁外附着新增的外部标签页。
	attached := make([]*Tab, 0, len(pending))
	for _, id := range pending {
		tab, err := b.attachTarget(ctx, id, alive[id])
		if err != nil {
			// 该 target 无法附加（可能正在关闭），跳过而不是让整次同步失败
			b.opts.Logger.Warn("附加外部标签页失败", "id", id, "err", err)
			continue
		}
		attached = append(attached, tab)
	}

	// 第三段：锁内写回，并按 infos 的顺序产出结果，保证返回顺序稳定。
	b.mu.Lock()
	defer b.mu.Unlock()

	// 写回前必须重新核对一次 b.tabs：从第一段到这里隔着锁外的附着（每个外部标签页
	// 一次 chromedp 往返），期间另一次并发的 syncTabs 完全可能已经把同一个 target
	// 附着好了。少了这次核对，b.tabs 里就会出现两条 ID 相同的记录——
	// 返回值看不出问题（结果列表是按 infos 构造并去重的），但多出来的那条 Tab
	// 所持有的 chromedp 会话此后没有任何人会 cancel，要等 Browser.Close 才回收。
	// 让位的一方把自己的那份释放掉，并把 known 指向已登记的那条，
	// 这样本次返回值依然完整。
	byID := make(map[target.ID]*Tab, len(b.tabs))
	for _, t := range b.tabs {
		byID[t.ID] = t
	}
	for _, tab := range attached {
		if exist, dup := byID[tab.ID]; dup {
			page.ReleaseTab(tab)
			known[tab.ID] = exist
			continue
		}
		byID[tab.ID] = tab
		b.tabs = append(b.tabs, tab)
		known[tab.ID] = tab
	}

	result := make([]*Tab, 0, len(alive))
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		id := target.ID(info.ID)
		if id == rootID {
			continue
		}
		if tab, ok := known[id]; ok {
			result = append(result, tab)
		}
	}
	return result, nil
}

// attachTarget 接管一个「浏览器里已有、但不属于本 Browser」的标签页。
//
// 必须从常驻 rootCtx 派生子上下文：只有共享同一个 browser 连接的子上下文才能正常
// 操作 target，从 allocCtx 派生会另起一条连接。派生后还要 Run 一次完成 attach，
// 否则后续命令会报 "no browser is open"。
//
// attach 这一步必须带超时：tabCtx 继承自无 deadline 的 rootCtx，直接 Run(tabCtx)
// 会让这里的 Tabs() 在 Chrome 假死时永久挂起，调用方的 ctx 形同虚设（历史缺陷 BUG-07）。
// 注意超时只能加在「等待预算」上，见 tabInitBudget 的说明。
func (b *Browser) attachTarget(ctx context.Context, id target.ID, url string) (*Tab, error) {
	// 派生 tab 上下文并完成 attach；首次 Run 与超时预算的细节见 newTabCtx。
	tabCtx, cancel, err := b.newTabCtx(ctx, chromedp.WithTargetID(id))
	if err != nil {
		return nil, err
	}
	tab := page.NewTabHandle(tabCtx, id, cancel, b.opts)
	page.SetTabURL(tab, url)
	// 附着成功意味着浏览器里已有一个真实标签页，可以安全关闭锚点空白页（sync.Once 保证只关一次）。
	b.closeAnchorOnce(ctx)
	return tab, nil
}

// findNewTargetID 对比 before 和 after，找出新出现的 page target ID
// 用于 NewTab 创建后定位新标签页
func findNewTargetID(before, after []targetInfo) target.ID {
	beforeIDs := map[string]bool{}
	for _, info := range before {
		beforeIDs[info.ID] = true
	}
	for _, info := range after {
		if info.Type == "page" && !beforeIDs[info.ID] {
			return target.ID(info.ID)
		}
	}
	return ""
}
