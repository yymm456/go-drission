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
// 不依赖 chromedp context，因此不会触发 invalid context；请求生命周期由 ctx 控制
func (b *Browser) listTargets(ctx context.Context) ([]targetInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json", b.port), nil)
	if err != nil {
		return nil, fmt.Errorf("构造 target 查询请求失败: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 target 列表失败: %w", err)
	}
	defer resp.Body.Close()

	var infos []targetInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return nil, fmt.Errorf("解析 target 列表失败: %w", err)
	}
	return infos, nil
}

// targets.go
func (b *Browser) syncTabsLocked(ctx context.Context) ([]*Tab, error) {
	infos, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}

	aliveIDs := map[target.ID]bool{}
	for _, info := range infos {
		if info.Type == "page" {
			aliveIDs[target.ID(info.ID)] = true
		}
	}

	var kept []*Tab
	for _, t := range b.tabs {
		if !aliveIDs[t.ID] {
			if t.cancel != nil {
				t.cancel()
			}
			continue
		}
		kept = append(kept, t)
	}
	b.tabs = kept

	seen := map[target.ID]*Tab{}
	for _, t := range b.tabs {
		seen[t.ID] = t
	}

	var result []*Tab
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		id := target.ID(info.ID)
		if id == b.rootTargetID {
			continue // rootCtx 自身的锚点 target，不作为普通标签页管理
		}
		if tab, ok := seen[id]; ok {
			tab.URL = info.URL
			result = append(result, tab)
		} else {
			// 外部打开的标签页：从常驻 rootCtx 派生以共享 browser 连接，
			// 并 Run 一次激活附加到该 target，否则后续操作会报 "no browser is open"。
			tabCtx, cancel := chromedp.NewContext(b.rootCtx, chromedp.WithTargetID(id))
			if err := chromedp.Run(tabCtx); err != nil {
				cancel()
				// 该 target 无法附加（可能正在关闭），跳过
				b.opts.logger.Warn("附加外部标签页失败", "id", id, "err", err)
				continue
			}
			tab := &Tab{ID: id, Ctx: tabCtx, cancel: cancel, URL: info.URL}
			b.tabs = append(b.tabs, tab)
			result = append(result, tab)
		}
	}
	return result, nil
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
