package cookie

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yymm456/go-drission/chromium/errs"
)

// ---------- Cookie 校验 ----------

// TestValidateCookieSameSiteNoneWithoutSecure 守住「注释承诺的第三项校验实际不存在」。
//
// errs 里 ErrInvalidCookie 的注释写明包含「SameSite=None 却没有 Secure」，
// 而 ValidateCookie 只检查了 name / domain。浏览器侧确实会拒收这种组合，但
// Network.setCookie 本身不报错——库把这个结果当成成功返回，调用方拿到的是
// 「全部注入成功」的假象，登录态却少了几条。必须在发起 CDP 之前拦成显式错误。
func TestValidateCookieSameSiteNoneWithoutSecure(t *testing.T) {
	bad := []Cookie{
		{Name: "none", Value: "1", Domain: "example.com", SameSite: "None"},
		{Name: "none-lower", Value: "1", Domain: "example.com", SameSite: "none"},
		{Name: "none-padded", Value: "1", Domain: "example.com", SameSite: "  None  "},
	}
	for i, c := range bad {
		if err := ValidateCookie(c, i+1); !errors.Is(err, errs.ErrInvalidCookie) {
			t.Errorf("Cookie %q（SameSite=%q, Secure=false）应报 errs.ErrInvalidCookie，实际 %v",
				c.Name, c.SameSite, err)
		}
	}

	good := []Cookie{
		{Name: "none-secure", Value: "1", Domain: "example.com", SameSite: "None", Secure: true},
		{Name: "none-secure-lower", Value: "1", Domain: "example.com", SameSite: "none", Secure: true},
		{Name: "lax", Value: "1", Domain: "example.com", SameSite: "Lax"},
		{Name: "strict", Value: "1", Domain: "example.com", SameSite: "Strict"},
		{Name: "unset", Value: "1", Domain: "example.com"},
	}
	for i, c := range good {
		if err := ValidateCookie(c, i+1); err != nil {
			t.Errorf("Cookie %q 不应被误拦：%v", c.Name, err)
		}
	}

	// 前两项校验不能被这轮改动破坏
	if err := ValidateCookie(Cookie{Domain: "example.com"}, 1); !errors.Is(err, errs.ErrInvalidCookie) {
		t.Errorf("缺 name 应报 errs.ErrInvalidCookie，实际 %v", err)
	}
	if err := ValidateCookie(Cookie{Name: "a"}, 1); !errors.Is(err, errs.ErrInvalidCookie) {
		t.Errorf("缺 domain 应报 errs.ErrInvalidCookie，实际 %v", err)
	}
}

// ---------- 分区 Cookie（CHIPS）----------

// TestValidateCookiePartitionedRequiresKey 守住第四项校验：
// 标了 Partitioned 却没给 PartitionKey 时必须在注入前拦下。
//
// 不拦的后果与 SameSite=None 缺 Secure 同类：CDP 会把分区标记丢掉、
// 当普通 Cookie 收下，调用方拿到「注入成功」，语义却已经变了。
func TestValidateCookiePartitionedRequiresKey(t *testing.T) {
	bad := []Cookie{
		{Name: "p", Value: "1", Domain: "example.com", Partitioned: true},
		{Name: "p-blank", Value: "1", Domain: "example.com", Partitioned: true, PartitionKey: "   "},
	}
	for i, c := range bad {
		if err := ValidateCookie(c, i+1); !errors.Is(err, errs.ErrInvalidCookie) {
			t.Errorf("Cookie %q 标了 Partitioned 却没给 PartitionKey，应报 errs.ErrInvalidCookie，实际 %v", c.Name, err)
		}
	}

	// 给了 key 就应当放行；没标分区时 key 留空也不该被拦
	good := []Cookie{
		{Name: "ok", Value: "1", Domain: "example.com", Partitioned: true, PartitionKey: "https://example.com"},
		{Name: "plain", Value: "1", Domain: "example.com"},
	}
	for i, c := range good {
		if err := ValidateCookie(c, i+1); err != nil {
			t.Errorf("Cookie %q 不应被误拦：%v", c.Name, err)
		}
	}
}

// TestPartitionKeyMapping 验证两条注入路径都带上了分区字段，
// 且**非分区 Cookie 必须拿到 nil** —— CDP 的语义是「不设 partitionKey 即为普通 Cookie」，
// 塞一个空结构体会把一个普通 Cookie 错误地变成分区 Cookie。
func TestPartitionKeyMapping(t *testing.T) {
	partitioned := Cookie{
		Name: "sid", Value: "v", Domain: "example.com",
		Partitioned: true, PartitionKey: "https://example.com",
	}
	plain := Cookie{Name: "sid", Value: "v", Domain: "example.com"}

	single := SetCookieParams(partitioned)
	if single.PartitionKey == nil {
		t.Fatal("单个注入路径未映射 PartitionKey")
	}
	if single.PartitionKey.TopLevelSite != "https://example.com" {
		t.Errorf("TopLevelSite = %q，期望 https://example.com", single.PartitionKey.TopLevelSite)
	}
	if got := SetCookieParams(plain); got.PartitionKey != nil {
		t.Errorf("普通 Cookie 不应带 PartitionKey，实际 %+v", got.PartitionKey)
	}

	batch := CookieParam(partitioned)
	if batch.PartitionKey == nil {
		t.Fatal("批量注入路径未映射 PartitionKey")
	}
	if batch.PartitionKey.TopLevelSite != "https://example.com" {
		t.Errorf("批量路径 TopLevelSite = %q", batch.PartitionKey.TopLevelSite)
	}
	if got := CookieParam(plain); got.PartitionKey != nil {
		t.Errorf("批量路径：普通 Cookie 不应带 PartitionKey，实际 %+v", got.PartitionKey)
	}
}

// TestPartitionedJSONRoundTrip 钉住「字段名与 session 侧一致」这件契约：
// 分区属性必须能经过一次 JSON 编解码原样往返，否则跨包接力时会静默丢属性。
func TestPartitionedJSONRoundTrip(t *testing.T) {
	in := []Cookie{{
		Name: "sid", Value: "v", Domain: "example.com", Path: "/",
		Partitioned: true, PartitionKey: "https://example.com",
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	// 字段名必须是 session.CookieItem 用的那两个，不能是 Go 的默认导出名
	for _, want := range []string{`"partitioned"`, `"partition_key"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("导出的 JSON 缺少 %s：%s", want, data)
		}
	}

	out, err := ParseCookiesJSON(data)
	if err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if len(out) != 1 || !out[0].Partitioned || out[0].PartitionKey != "https://example.com" {
		t.Errorf("分区属性未能往返：%+v", out)
	}
}
