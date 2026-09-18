package chromium

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------- Cookie 校验 ----------

// TestValidateCookieSameSiteNoneWithoutSecure 守住「注释承诺的第三项校验实际不存在」。
//
// errors.go 里 ErrInvalidCookie 的注释写明包含「SameSite=None 却没有 Secure」，
// 而 validateCookie 只检查了 name / domain。浏览器侧确实会拒收这种组合，但
// Network.setCookie 本身不报错——库把这个结果当成成功返回，调用方拿到的是
// 「全部注入成功」的假象，登录态却少了几条。必须在发起 CDP 之前拦成显式错误。
func TestValidateCookieSameSiteNoneWithoutSecure(t *testing.T) {
	bad := []Cookie{
		{Name: "none", Value: "1", Domain: "example.com", SameSite: "None"},
		{Name: "none-lower", Value: "1", Domain: "example.com", SameSite: "none"},
		{Name: "none-padded", Value: "1", Domain: "example.com", SameSite: "  None  "},
	}
	for i, c := range bad {
		if err := validateCookie(c, i+1); !errors.Is(err, ErrInvalidCookie) {
			t.Errorf("Cookie %q（SameSite=%q, Secure=false）应报 ErrInvalidCookie，实际 %v",
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
		if err := validateCookie(c, i+1); err != nil {
			t.Errorf("Cookie %q 不应被误拦：%v", c.Name, err)
		}
	}

	// 前两项校验不能被这轮改动破坏
	if err := validateCookie(Cookie{Domain: "example.com"}, 1); !errors.Is(err, ErrInvalidCookie) {
		t.Errorf("缺 name 应报 ErrInvalidCookie，实际 %v", err)
	}
	if err := validateCookie(Cookie{Name: "a"}, 1); !errors.Is(err, ErrInvalidCookie) {
		t.Errorf("缺 domain 应报 ErrInvalidCookie，实际 %v", err)
	}
}

// TestSetCookiesValidatesBeforeCDP 确认校验发生在 CDP 调用之前。
//
// 这里刻意传一个「没连接、Ctx 为空」的 Tab：如果校验晚于 t.run，
// 拿到的会是 ErrInvalidContext 之类的底层错误，而不是 ErrInvalidCookie。
func TestSetCookiesValidatesBeforeCDP(t *testing.T) {
	var empty Tab
	ctx := context.Background()

	if err := empty.SetCookie(ctx, Cookie{Name: "x", Value: "1", Domain: "example.com", SameSite: "None"}); !errors.Is(err, ErrInvalidCookie) {
		t.Errorf("SetCookie 应在 CDP 之前拦下并返回 ErrInvalidCookie，实际 %v", err)
	}
	if err := empty.SetCookies(ctx, []Cookie{
		{Name: "ok", Value: "1", Domain: "example.com"},
		{Name: "bad", Value: "1", Domain: "example.com", SameSite: "None"},
	}); !errors.Is(err, ErrInvalidCookie) {
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
		if err := s.validate(); !errors.Is(err, ErrSelectorRequired) {
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

// ---------- 标签页上下文超时（BUG-07 的契约） ----------

// TestTabInitBudgetRespectsCallerDeadline 守住「新建 / 附着标签页」那一步的等待预算。
//
// 早期这些步骤直接 chromedp.Run(tabCtx)，而 tabCtx 继承自无 deadline 的 rootCtx，
// Chrome 假死时整个调用会永久挂起，调用方 ctx 的超时形同虚设。
//
// 注意预算只用于 runAbandonable 的 watchCtx——它绝不能直接当 chromedp.Run 的 ctx
// （见 tabInitBudget 的注释：chromedp 会把 Target 的事件分发 goroutine 绑在 attach
// 传入的 ctx 上，那个 ctx 被取消后标签页就再也不应答了）。
func TestTabInitBudgetRespectsCallerDeadline(t *testing.T) {
	// 调用方给了更短的 deadline：必须取它，而不是套满默认的 30s
	caller, cancelCaller := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCaller()

	ctx, cancel := tabInitBudget(caller)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("派生的预算应带 deadline")
	}
	if d := time.Until(dl); d > time.Second {
		t.Fatalf("应沿用调用方更短的 deadline，实际还剩 %v", d)
	}

	// 调用方没设 deadline：必须套默认兜底
	ctx2, cancel2 := tabInitBudget(context.Background())
	defer cancel2()
	dl2, ok := ctx2.Deadline()
	if !ok {
		t.Fatal("调用方 ctx 无 deadline 时应套上默认超时")
	}
	if d := time.Until(dl2); d > defaultCDPTimeout {
		t.Fatalf("兜底超时不应超过 %v，实际 %v", defaultCDPTimeout, d)
	}
}

// TestBoundedRootCtxNilSafe 守住与 Close 并发时的空上下文。
//
// Browser 尚未 Connect（或刚被 Close）时 rootCtx 为 nil，而对 nil 调
// context.WithTimeout 会直接 panic。这里要求返回一个已结束的上下文。
func TestBoundedRootCtxNilSafe(t *testing.T) {
	var b Browser

	ctx, cancel := b.boundedRootCtx(context.Background())
	defer cancel()
	if ctx == nil {
		t.Fatal("rootCtx 为空时不应返回 nil 上下文")
	}
	if ctx.Err() == nil {
		t.Fatal("rootCtx 为空时应返回一个已结束的上下文")
	}
}

// TestConnectedRootCtxReportsState 确认取快照时会把「未连接 / 已关闭」如实报出来，
// 而不是把 nil 上下文交给调用方去 panic。
func TestConnectedRootCtxReportsState(t *testing.T) {
	var b Browser
	if _, err := b.connectedRootCtx(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("未连接时应返回 ErrNotConnected，实际 %v", err)
	}

	b.closed = true
	if _, err := b.connectedRootCtx(); !errors.Is(err, ErrClosed) {
		t.Fatalf("已关闭时应返回 ErrClosed，实际 %v", err)
	}
}
