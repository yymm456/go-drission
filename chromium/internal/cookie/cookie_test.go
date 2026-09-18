package cookie

import (
	"errors"
	"testing"

	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// ---------- Cookie 校验 ----------

// TestValidateCookieSameSiteNoneWithoutSecure 守住「注释承诺的第三项校验实际不存在」。
//
// internal/errs 里 ErrInvalidCookie 的注释写明包含「SameSite=None 却没有 Secure」，
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
