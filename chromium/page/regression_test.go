package page

import (
	"context"
	"errors"
	"testing"

	"github.com/yymm456/go-drission/chromium/cookie"
	"github.com/yymm456/go-drission/chromium/errs"
)

// ---------- Cookie 注入前校验（CDP 之前必须拦住） ----------
//
// 校验逻辑本身（含「SameSite=None 必须带 Secure」）已随 S3 下沉到 cookie，
// 纯逻辑用例见 cookie/cookie_test.go 的 TestValidateCookieSameSiteNoneWithoutSecure；
// 这里只守一件事：Tab 方法确实在发起 CDP 调用之前就把它拦下来了。

// TestSetCookiesValidatesBeforeCDP 确认校验发生在 CDP 调用之前。
//
// 这里刻意传一个「没连接、Ctx 为空」的 Tab：如果校验晚于 t.run，
// 拿到的会是 ErrInvalidContext 之类的底层错误，而不是 ErrInvalidCookie。
func TestSetCookiesValidatesBeforeCDP(t *testing.T) {
	var empty Tab
	ctx := context.Background()

	if err := empty.SetCookie(ctx, cookie.Cookie{Name: "x", Value: "1", Domain: "example.com", SameSite: "None"}); !errors.Is(err, errs.ErrInvalidCookie) {
		t.Errorf("SetCookie 应在 CDP 之前拦下并返回 ErrInvalidCookie，实际 %v", err)
	}
	if err := empty.SetCookies(ctx, []cookie.Cookie{
		{Name: "ok", Value: "1", Domain: "example.com"},
		{Name: "bad", Value: "1", Domain: "example.com", SameSite: "None"},
	}); !errors.Is(err, errs.ErrInvalidCookie) {
		t.Errorf("SetCookies 应在 CDP 之前拦下并返回 ErrInvalidCookie，实际 %v", err)
	}
}

// ---------- 选择器判空 ----------

// TestSelectorWhitespaceIsEmpty 守住纯空白选择器漏过校验这一格。
//
// Empty 只比空串时，`CSS(prefix + suffix)` 在变量为空时会拼出 "   " 并直达浏览器，
// 报出「'   ' is not a valid selector」这类底层语法错误；Text 路径更会把
// 「选择器非法」包装成 ErrElementNotFound「等待元素超时」，误导调用方去改选择器。
func TestSelectorWhitespaceIsEmpty(t *testing.T) {
	blanks := []Selector{CSS("   "), XPath("\t"), ID("\n"), JS("  ")}
	for _, s := range blanks {
		if !s.Empty() {
			t.Errorf("%s=%q 应判定为空", s.Mode(), s.String())
		}
		if err := s.validate(); !errors.Is(err, errs.ErrSelectorRequired) {
			t.Errorf("%s=%q 期望 ErrSelectorRequired，实际 %v", s.Mode(), s.String(), err)
		}
	}

	// 判定不应改写原表达式：JS 模式的表达式理论上可含前导空白
	js := JS("  document.body  ")
	if js.Empty() {
		t.Error("含有实际内容的选择器不应判定为空")
	}
	if js.String() != "  document.body  " {
		t.Errorf("Empty 不应改写原表达式，实际 %q", js.String())
	}
}
