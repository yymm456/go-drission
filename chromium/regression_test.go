package chromium

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium/internal/cdpkit"
)

// ---------- Cookie 注入前校验（CDP 之前必须拦住） ----------
//
// 校验逻辑本身（含「SameSite=None 必须带 Secure」）已随 S3 下沉到 internal/cookie，
// 纯逻辑用例见 internal/cookie/cookie_test.go 的 TestValidateCookieSameSiteNoneWithoutSecure；
// 这里只守一件事：Tab 方法确实在发起 CDP 调用之前就把它拦下来了。

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
	if d := time.Until(dl2); d > cdpkit.DefaultCallTimeout {
		t.Fatalf("兜底超时不应超过 %v，实际 %v", cdpkit.DefaultCallTimeout, d)
	}
}

// TestBudgetDurationClampsToCallerDeadline 直接守住两个入口共用的那条超时算法。
//
// cdpkit.BudgetDuration 是 boundedRootCtx 与 tabInitBudget 的唯一算法来源，抽出来就必须能
// 单独钉住。只测 tabInitBudget 的派生 deadline 是不够的：它的父 ctx 就是入参 ctx，
// 而 context.WithTimeout 会对父 ctx 的 deadline 再取一次 min，因此哪怕
// cdpkit.BudgetDuration 退化成「永远返回 cdpkit.DefaultCallTimeout」，tabInitBudget 的结果也是对的。
// 真正依赖裁剪的是 boundedRootCtx——它的父是（无 deadline 的）rootCtx，只有
// cdpkit.BudgetDuration 自己收紧了才会尊重调用方的 deadline。
func TestBudgetDurationClampsToCallerDeadline(t *testing.T) {
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if d := cdpkit.BudgetDuration(short); d > time.Second {
		t.Fatalf("应采纳调用方更短的 deadline，实际 %v", d)
	}

	if d := cdpkit.BudgetDuration(context.Background()); d != cdpkit.DefaultCallTimeout {
		t.Fatalf("调用方无 deadline 时应返回默认超时 %v，实际 %v", cdpkit.DefaultCallTimeout, d)
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

// TestDisposeBrowserContextNilRootCtxNoPanic 守住 teardown 路径上的空指针。
//
// rootCtx 为 nil 时（并发 Close 刚把它置 nil），disposeBrowserContext 会走进
// cdpkit.WithDefaultTimeout(nil, ...) —— 它的 nil 分支原样返回 nil —— 紧接着
// chromedp.Run(nil, ...) 会在 chromedp.FromContext 里对 nil 接口调 ctx.Value，
// 直接 panic：
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	  chromedp.FromContext                     chromedp.go:227
//	  chromedp.initContextBrowser({0x0, 0x0})  chromedp.go:291
//	  chromedp.Run({0x0, 0x0}, ...)            chromedp.go:326
//	  (*Browser).disposeBrowserContext         context.go
//
// 触发路径是「建号与 teardown 并发」：Context() 已经建好 CDP 上下文、正在 attach 时，
// Close() 把 rootCtx 置 nil，attach 失败分支随即来这里回收。不需要真实浏览器，
// rootCtx==nil 就足以稳定复现，因此这个用例是纯单测。
func TestDisposeBrowserContextNilRootCtxNoPanic(t *testing.T) {
	var b Browser // rootCtx 零值即 nil

	// 空 bcID 走早退分支，本来就安全
	b.disposeBrowserContext("")

	// 非空 bcID 才是有问题的那条：修复前这里会 panic
	b.disposeBrowserContext("fake-browser-context-id")
}

// TestReleaseConnectionResourcesIdempotent 守住连接资源回收的幂等性。
//
// Close 的第 3 步与 Connect 的失败回滚共用 releaseConnectionResources，而 teardown
// 天然会被重复触发（Close 之后又走一次失败回滚、Close 与回滚并发等）。早期是两处
// 各写一遍的裸代码，重复执行会重复 kill 进程树 / 重复释放目录锁。
//
// 两点要求：零值 Browser 上连续调用必须安全；有实际句柄时回收后字段必须清空——
// 尤其 rootCtx 必须为 nil，否则 ensureConnected 会把「连接已死」误判成「仍已连接」。
func TestReleaseConnectionResourcesIdempotent(t *testing.T) {
	// (1) 零值 Browser：全部字段为 nil / false，不能因为「判空没做」而 panic
	var zero Browser
	zero.releaseConnectionResources()
	zero.releaseConnectionResources()

	// (2) 有实际句柄：回收后必须清空，且重复调用不能改变终态
	var b Browser
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	_, cancelAlloc := context.WithCancel(context.Background())
	b.rootCtx, b.rootCancel, b.allocCancel = rootCtx, cancelRoot, cancelAlloc

	b.releaseConnectionResources()
	if b.rootCtx != nil || b.rootCancel != nil || b.allocCancel != nil {
		t.Fatalf("回收后句柄字段应全部置 nil：rootCtx=%v rootCancel=%v allocCancel=%v",
			b.rootCtx, b.rootCancel, b.allocCancel)
	}
	if rootCtx.Err() == nil {
		t.Fatal("rootCancel 应已被调用（rootCtx 应处于已取消状态）")
	}

	b.releaseConnectionResources()
	if b.rootCtx != nil || b.rootCancel != nil || b.allocCancel != nil {
		t.Fatalf("重复回收不应改变终态：rootCtx=%v rootCancel=%v allocCancel=%v",
			b.rootCtx, b.rootCancel, b.allocCancel)
	}
}
