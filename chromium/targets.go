package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// targetInfo 是 Chrome /json 端点返回的 target 结构
type targetInfo struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

// listTargets 通过 HTTP /json 端点获取 target 列表
// 不依赖 chromedp context，因此不会触发 invalid context；请求生命周期由 ctx 控制。
//
// 与 launch.go 的端口探测共用 noProxyTransport：/json 与调试端口都在本机回环上，
// 必须直连。虽然 Go 的 ProxyFromEnvironment 对 loopback 地址本来就会跳过代理，
// 但显式复用同一个 client 还能拿到统一的拨号/响应头超时，
// 也避免依赖 http.DefaultClient 这个全局变量被调用方改写。
func (b *Browser) listTargets(ctx context.Context) ([]targetInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json", b.port), nil)
	if err != nil {
		return nil, fmt.Errorf("构造 target 查询请求失败: %w", err)
	}

	resp, err := noProxyClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 target 列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询 target 列表失败: 端口 %d 返回 HTTP %d", b.port, resp.StatusCode)
	}

	var infos []targetInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return nil, fmt.Errorf("解析 target 列表失败: %w", err)
	}
	return infos, nil
}

// syncTabs 把「浏览器里真实存在的 target 列表」同步到托管的标签页集合，
// 并返回按 target 顺序排列的标签页。
//
// infos 必须由调用方在锁外取好（见 guard + listTargets）：查询本机 /json 虽然快，
// 但端口卡顿时会一直等，持锁做这件事会把所有标签页操作一起卡住。
//
// 不接受 ctx：内部唯一的 I/O 是附着外部 target，而 chromedp 的 target 附着
// 由 Browser 自己的生命周期上下文（rootCtx）决定归属；接一个调用方 ctx 进来只会
// 让人误以为「取消它能中断附着」，实际并不能。
//
// 内部按「锁内读状态 → 锁外做 I/O → 锁内写状态」三段式组织，
// 附着外部标签页（一次 chromedp 往返）全部发生在锁外。
func (b *Browser) syncTabs(infos []targetInfo) ([]*Tab, error) {
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
				if t.cancel != nil {
					t.cancel()
				}
				continue
			}
			t.setURL(url)
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
		tab, err := b.attachTarget(id, alive[id])
		if err != nil {
			// 该 target 无法附加（可能正在关闭），跳过而不是让整次同步失败
			b.opts.logger.Warn("附加外部标签页失败", "id", id, "err", err)
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
			if tab.cancel != nil {
				tab.cancel()
			}
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
func (b *Browser) attachTarget(id target.ID, url string) (*Tab, error) {
	tabCtx, cancel := chromedp.NewContext(b.rootCtx, chromedp.WithTargetID(id))
	if err := chromedp.Run(tabCtx); err != nil {
		cancel()
		return nil, err
	}
	tab := &Tab{
		ID:      id,
		Ctx:     tabCtx,
		cancel:  cancel,
		timeout: b.opts.defaultTimeout,
		logger:  b.opts.logger,
	}
	tab.setURL(url)
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
