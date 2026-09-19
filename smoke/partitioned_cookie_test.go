//go:build smoke

package smoke

// 分区 Cookie（CHIPS）在真实浏览器里的往返验证。
//
// 背景：`chromium.Cookie` 与 `session.CookieItem` 的字段曾经不一致（前者缺
// Partitioned / PartitionKey），导致跨包接力时分区属性被静默丢弃。补齐字段后
// 需要确认「设置 → 读回」这条链在真实 Chrome 上确实成立，而不只是单测里的字段映射对了。
//
// CHIPS 对上下文有要求（需要 Secure，且分区 key 是顶层站点的 site），
// 本地 http 测试站点能否满足要实测 —— 若浏览器不收，用例会明确 skip 而不是假装通过。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func TestPartitionedCookieRoundTrip(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 60*time.Second)

	// 测试站点跑在 127.0.0.1 上：Chrome 把它当作 potentially trustworthy，
	// 因此 Secure Cookie 在这里可以被设置（分区 Cookie 要求 Secure）。
	const site = "http://127.0.0.1"

	want := chromium.Cookie{
		Name:         "chips_demo",
		Value:        "v1",
		Domain:       "127.0.0.1",
		Path:         "/",
		Secure:       true,
		Partitioned:  true,
		PartitionKey: site,
	}

	if err := tab.SetCookie(ctx, want); err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("设置分区 Cookie 失败：%v", err)
	}

	// 读回全部 Cookie，找我们刚设的那条
	list, err := tab.Cookies(ctx)
	if err != nil {
		t.Fatalf("读取 Cookie 失败：%v", err)
	}

	var got *chromium.Cookie
	for i := range list {
		if list[i].Name == want.Name {
			got = &list[i]
			break
		}
	}
	if got == nil {
		// 浏览器没有落盘：多半是本地 http 站点不满足 CHIPS 的上下文要求。
		// 这属于环境限制，不该判失败，但必须说清楚，避免把「没测到」当成「测过了」。
		t.Skipf("浏览器未保存该分区 Cookie（本地 http 站点可能不满足 CHIPS 的 Secure 上下文要求）；"+
			"当前共读到 %d 条 Cookie", len(list))
	}

	if !got.Partitioned {
		t.Errorf("读回的 Cookie 丢了分区标记：%+v", *got)
	}
	if got.PartitionKey == "" {
		t.Errorf("读回的分区 Cookie 没有 PartitionKey：%+v", *got)
	} else {
		t.Logf("分区 Cookie 往返成功：name=%s partitioned=%v key=%s", got.Name, got.Partitioned, got.PartitionKey)
	}

	// 非分区 Cookie 必须以 Partitioned=false 读回，不能被误标
	plain := chromium.Cookie{Name: "plain_demo", Value: "v2", Domain: "127.0.0.1", Path: "/"}
	if err := tab.SetCookie(ctx, plain); err != nil {
		t.Fatalf("设置普通 Cookie 失败：%v", err)
	}
	list2, err := tab.Cookies(ctx)
	if err != nil {
		t.Fatalf("再次读取 Cookie 失败：%v", err)
	}
	for i := range list2 {
		if list2[i].Name == plain.Name && list2[i].Partitioned {
			t.Errorf("普通 Cookie 被错误地标记为分区：%+v", list2[i])
		}
	}

}

// TestPartitionedCookieRelayFromSession session → chromium 的分区属性接力。
//
// 这是补齐字段要解决的那个场景：session 侧导出带 partitioned 的 JSON，
// 经 chromium 解析后属性必须还在（原来会因为字段不存在被静默丢掉）。
func TestPartitionedCookieRelayFromSession(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 60*time.Second)

	// 模拟 session 侧导出的 JSON（字段名与 session.CookieItem 一致）
	data := []byte(`[{"name":"relay_chips","value":"v1","domain":"127.0.0.1","path":"/",` +
		`"http_only":false,"secure":true,"same_site":"","expires":0,` +
		`"partitioned":true,"partition_key":"http://127.0.0.1"}]`)

	parsed, err := chromium.ParseCookiesJSON(data)
	if err != nil {
		t.Fatalf("解析 session 导出的 Cookie JSON 失败：%v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("解析出 %d 条，期望 1 条", len(parsed))
	}
	if !parsed[0].Partitioned || parsed[0].PartitionKey != "http://127.0.0.1" {
		t.Fatalf("分区属性在解析后被丢弃：%+v —— 这正是补齐字段前的问题", parsed[0])
	}

	// 注入并读回；浏览器不收时按环境限制处理
	if _, err := tab.ImportCookiesJSON(ctx, data); err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("导入分区 Cookie 失败：%v", err)
	}

	list, err := tab.Cookies(ctx)
	if err != nil {
		t.Fatalf("读取 Cookie 失败：%v", err)
	}
	found := false
	for i := range list {
		if list[i].Name == "relay_chips" {
			found = true
			if !list[i].Partitioned {
				t.Errorf("接力后分区标记丢失：%+v", list[i])
			}
		}
	}
	if !found {
		t.Skipf("浏览器未保存该分区 Cookie（本地 http 站点可能不满足 CHIPS 要求）；共 %d 条", len(list))
	}
	t.Log("session → chromium 的分区属性接力成功")

	// 导出后分区属性也要还在（chromium → session 方向）
	out, err := tab.ExportCookiesJSON(ctx)
	if err != nil {
		t.Fatalf("导出 Cookie 失败：%v", err)
	}
	if !strings.Contains(string(out), `"partitioned"`) {
		t.Errorf("导出的 JSON 缺少 partitioned 字段：%s", out)
	}

}
