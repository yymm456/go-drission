package page

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// TestWaitBuilderValidation 覆盖等待条件的参数校验：
// 这类错误必须在发起 CDP 调用之前就报出来，否则用户只会看到含糊的超时。
func TestWaitBuilderValidation(t *testing.T) {
	tab := &Tab{}

	if err := tab.Wait().Do(context.Background()); !errors.Is(err, errs.ErrWaitConditionUnset) {
		t.Errorf("未指定条件时期望 ErrWaitConditionUnset，实际 %v", err)
	}
	// 元素级条件缺少元素必须被拦住（errElem 为 nil 表示「忘了给元素」）
	if err := tab.Wait().Visible().Do(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("缺元素时期望 ErrSelectorRequired，实际 %v", err)
	}
	if err := tab.Wait().Text("x").Do(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("Text 缺元素时期望 ErrSelectorRequired，实际 %v", err)
	}
	if err := tab.Wait().Count(3).Do(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("Count 缺元素时期望 ErrSelectorRequired，实际 %v", err)
	}

	// 有了元素但选择器为空（EleCSS("")）同样要拦
	if err := tab.Wait().Element(tab.EleCSS("")).Visible().Do(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("空 CSS 时期望 ErrSelectorRequired，实际 %v", err)
	}

	// el.Wait() 是元素级等待的主路径，同样走这套校验
	if err := tab.EleCSS("").Wait().Visible().Do(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("el.Wait() 空选择器时期望 ErrSelectorRequired，实际 %v", err)
	}

	// 元素级操作在空选择器下也必须报 ErrSelectorRequired，而不是发出无效 CDP 调用
	if err := tab.EleCSS("").Click(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("空选择器 Click 期望 ErrSelectorRequired，实际 %v", err)
	}
	if _, err := tab.EleID("").Text(context.Background()); !errors.Is(err, errs.ErrSelectorRequired) {
		t.Errorf("空选择器 Text 期望 ErrSelectorRequired，实际 %v", err)
	}
}

// TestElementDoesNotCacheNode 验证 Element 只保存选择器、不缓存 DOM 节点：
// 同一个 Element 在页面内容变化后必须能读到新值（SPA 重渲染场景的基本要求）。
func TestElementDoesNotCacheNode(t *testing.T) {
	el := (&Tab{}).EleCSS("#x")
	if el.Selector().String() != "#x" {
		t.Errorf("选择器未保存：%q", el.Selector().String())
	}
	if el.Tab() == nil {
		t.Error("Element 应能取回所属 Tab")
	}
	if got := (&Tab{}).EleID("a").String(); got != `id="a"` {
		t.Errorf("String() 期望 id=\"a\"，实际 %q", got)
	}
	// 每次查询都应产生独立的 Element（无共享节点状态）。
	// 必须先取到两个变量再比：写成 `t.EleCSS(x) == t.EleCSS(x)` 会踩 staticcheck 的
	// SA4000（左右两侧文本相同），而且那种写法并不能证明「两次查询结果不同」。
	tabForEle := &Tab{}
	first, second := tabForEle.EleCSS("#a"), tabForEle.EleCSS("#a")
	if first == second {
		t.Error("每次查询都应产生独立的 Element")
	}
	if first == tabForEle.EleCSS("#a") {
		t.Error("Element 不应被复用缓存")
	}
}

// TestToFunctionDecl 覆盖 el.Eval 的 JS 片段自动包装规则。
func TestToFunctionDecl(t *testing.T) {
	cases := map[string]string{
		"this.innerText":          "function(){ return (this.innerText); }",
		"function(){ return 1 }":  "function(){ return 1 }",
		"() => this.id":           "() => this.id",
		"":                        "function() {}",
		"return this.dataset.id":  "function(){ return this.dataset.id }",
		"const v = 1;\nreturn v;": "function(){ const v = 1;\nreturn v; }",
		"this.click();":           "function(){ this.click(); }",
	}
	for in, want := range cases {
		if got := toFunctionDecl(in); got != want {
			t.Errorf("toFunctionDecl(%q)\n  期望 %q\n  实际 %q", in, want, got)
		}
	}
}

// TestWaitBuilderIndependentConditionsPassValidation 验证 URL / Ready 这类
// 不需要选择器的条件不会被误拦。
//
// 这里刻意传入「已取消」的 ctx：没有真实浏览器时，任何 CDP 调用都会失败，
// 而 WaitURL 是轮询循环——若不限制它会一直重试到天荒地老。
func TestWaitBuilderIndependentConditionsPassValidation(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	errURL := (&Tab{}).Wait().URL("x").Do(expired)
	if errors.Is(errURL, errs.ErrSelectorRequired) {
		t.Error("URL 条件不应要求选择器")
	}
	if errURL == nil {
		t.Error("URL 条件在 ctx 已取消时应返回错误")
	}

	errReady := (&Tab{}).Wait().Ready().Do(expired)
	if errors.Is(errReady, errs.ErrSelectorRequired) {
		t.Error("Ready 条件不应要求选择器")
	}
	if errReady == nil {
		t.Error("Ready 条件在 ctx 已取消时应返回错误")
	}
}

// TestTabAppliesDefaultTimeout 验证「调用方没设超时」时内置超时生效。
func TestTabAppliesDefaultTimeout(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 50 * time.Millisecond}

	got, cancel := tab.safeCtx(context.Background())
	defer cancel()
	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("内置默认超时未生效：返回的 ctx 没有 deadline")
	}
	if d := time.Until(dl); d > 50*time.Millisecond || d <= 0 {
		t.Errorf("内置超时应约为 50ms，实际 %v", d)
	}
}

// TestTabRespectsCallerDeadline 验证调用方自己设的超时不会被内置超时覆盖——
// 超时决策权始终归调用方，这是本库的核心约定。
func TestTabRespectsCallerDeadline(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 10 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	got, cancelGot := tab.safeCtx(ctx)
	defer cancelGot()
	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("调用方设置的 deadline 丢失")
	}
	if d := time.Until(dl); d > 200*time.Millisecond {
		t.Errorf("调用方的 100ms deadline 被内置 10s 超时覆盖了，实际剩余 %v", d)
	}
}

// TestTabTimeoutDisabled 验证 WithDefaultTimeout(0) 可以彻底关掉内置超时。
func TestTabTimeoutDisabled(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 0}

	got, cancel := tab.safeCtx(context.Background())
	defer cancel()
	if _, ok := got.Deadline(); ok {
		t.Error("timeout=0 时不应自动套用超时")
	}
}

// TestTabBareCtxEarlyCancelPropagates 验证「裸 ctx 带 deadline」这条回退路径上，
// 调用方提前取消能传导到派生上下文。
//
// 该分支里派生上下文的父节点必须换成 t.Ctx（chromedp 路由信息只挂在那条链上），
// 于是它和调用方 ctx 之间没有天然的父子关系，得靠 context.AfterFunc 补边。
// 这条断言就是钉住那条边：少了它，调用方取消后命令会一直跑到 deadline 才结束。
func TestTabBareCtxEarlyCancelPropagates(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: time.Minute}

	caller, cancelCaller := context.WithTimeout(context.Background(), time.Hour)
	defer cancelCaller()

	got, cancel := tab.safeCtx(caller)
	defer cancel()

	cancelCaller()
	select {
	case <-got.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("调用方提前取消未传导到派生上下文")
	}
}

// TestTabSafeCtxCancelReleases 验证返回的 cancel 能立刻回收派生上下文，
// 而不是拖到 deadline 才释放。
//
// 这正是 run 里那句 defer cancel() 的意义所在：派生上下文的超时定时器
// 靠它提前停掉，否则每次调用都要多挂一个定时器到超时为止。
func TestTabSafeCtxCancelReleases(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: time.Hour}

	got, cancel := tab.safeCtx(context.Background())
	cancel()
	select {
	case <-got.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel 之后派生上下文仍未结束")
	}
}

// TestTabSafeCtxNoopCancelSafe 覆盖「未派生新上下文」时返回的空操作 cancel。
// run 会无条件 defer cancel()，所以这条路径返回的 cancel 必须能安全调用。
func TestTabSafeCtxNoopCancelSafe(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 0}

	got, cancel := tab.safeCtx(context.Background())
	if got == nil {
		t.Fatal("返回的 ctx 不应为 nil")
	}
	cancel()
}

// TestWaitFatalErr 覆盖「轮询等待中哪些错误该立即中止」的判定。
//
// 名单之外的错误一律继续重试——尤其是元素还没出现（ErrElementNotFound）
// 和导航期间的 CDP 报错，那些正是等待本身要熬过去的状态。
func TestWaitFatalErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"空选择器（入参错，重试无意义）", fmt.Errorf("%w: 选择器为空（css）", errs.ErrSelectorRequired), true},
		{"连接已关闭", errs.ErrClosed, true},
		{"尚未连接", errs.ErrNotConnected, true},
		{"隔离上下文已关闭", errs.ErrContextClosed, true},
		{"元素还没出现（等待的目标，不能中止）", errs.ErrElementNotFound, false},
		{"ctx 超时不算致命", fmt.Errorf("查询失败: %w", context.DeadlineExceeded), false},
		{"导航期间的 CDP 报错不能中止", errors.New("Cannot find context with specified id"), false},
	}
	for _, c := range cases {
		if got := waitFatalErr(c.err); got != c.want {
			t.Errorf("%s: waitFatalErr = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestWaitTextFailsFastOnEmptySelector 验证空选择器让等待立即失败，而不是空转到超时。
//
// 原来每轮都把这个错误丢掉，最终只报「等待超时」，把「你根本没给选择器」这件事
// 伪装成了「等不到」。
func TestWaitTextFailsFastOnEmptySelector(t *testing.T) {
	el := (&Tab{Ctx: context.Background(), timeout: 0}).EleCSS("")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := el.WaitText(ctx, "任意")
	if !errors.Is(err, errs.ErrSelectorRequired) {
		t.Fatalf("期望 ErrSelectorRequired，实际 %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("应在首次失败时就返回，实际耗时 %v（说明退化成轮询到超时了）", d)
	}
}

// TestWaitURLSurfacesUnderlyingError 验证等待超时时会把期间最后一次底层错误带出来。
//
// Tab.Ctx 指向一个不含 chromedp 路由信息的裸 context，于是每轮 CDP 调用都会稳定失败
// （chromedp.ErrInvalidContext 是字符串类型，与本库同名哨兵不是同一个值），
// 且错误与 ctx 无关——正是「一直报同一个错，最后只说等待超时」的场景。
func TestWaitURLSurfacesUnderlyingError(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 0}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := tab.WaitURL(ctx, "永远不会匹配")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	// 超时语义必须保留：调用方靠 errors.Is(err, context.DeadlineExceeded) 判断
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("超时语义丢失：%v", err)
	}
	// 底层错误必须留在错误链里。这里直接断言到 chromedp 的那个哨兵——
	// 比「链里有个不是超时的东西」精确，也不用自己遍历错误树。
	// 只剩 ctx 错误，就说明又被吞了。
	if !errors.Is(err, chromedp.ErrInvalidContext) {
		t.Errorf("底层错误被吞掉了：%v", err)
	}
}

// TestSetTimeout 覆盖标签页级别的超时调整。
func TestSetTimeout(t *testing.T) {
	tab := &Tab{Ctx: context.Background(), timeout: 30 * time.Second}
	tab.SetTimeout(5 * time.Second)
	if tab.timeout != 5*time.Second {
		t.Errorf("期望 5s，实际 %v", tab.timeout)
	}
	tab.SetTimeout(-1)
	if tab.timeout != 0 {
		t.Errorf("负数应归零（关闭内置超时），实际 %v", tab.timeout)
	}
}
