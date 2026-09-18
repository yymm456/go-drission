package chromium

import (
	"github.com/yymm456/go-drission/chromium/internal/network"
)

// Listen 在 tab 上创建监听器。
//
// pattern 是 URL 子串过滤条件（为空表示全部命中）；创建后需要 Start(ctx) 才生效，
// 传给 Start 的 ctx 必须派生自 tab.Ctx。实现见 internal/network，这里只做转发。
func (t *Tab) Listen(pattern string) *Listener { return network.NewListener(pattern) }
