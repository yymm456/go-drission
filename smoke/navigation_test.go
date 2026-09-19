//go:build smoke

package smoke

// 本文件补强「导航」这一块的真实场景：异常页面的行为、等待超时、
// 以及 Reload 到底有没有真的重新加载。
//
// 为什么单独成文件：导航是最高频的操作，而异常路径（404 / 连不上 / 加载不完）
// 的行为与正常路径完全不同，混在正常用例里容易被「反正都过了」掩盖掉。
//
// 重要：本文件只描述**当前 API 的实际语义**，不为让断言变绿而假设理想行为。
// 若某条断言与实现不符，那是「实际行为 vs 预期」的差异，应报告出来讨论，
// 而不是改产品代码去迎合测试。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// TestNavigateNotFoundPage 访问一个明确返回 404 的地址。
//
// 关键点：HTTP 404 在浏览器层面是「导航成功」——页面加载了 404 的响应体，
// 并不会让 Navigate 报错。这条用例把该语义钉住，同时确认 404 之后标签页照常可用。
func TestNavigateNotFoundPage(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	// 测试服务器对未匹配的路径走 http.NotFound，即标准 404
	notFound := srv.main.URL + "/definitely-not-exist"

	err := tab.Navigate(ctx, notFound)
	if err != nil {
		// 记录而不是直接失败：若实现选择把非 2xx 报成错误，那是另一种合理语义，
		// 但要在报告里说明，不要悄悄改成期望的样子。
		t.Logf("导航到 404 页面返回了错误（记录以观察实际语义）：%v", err)
	}

	// 无论 Navigate 是否报错，页面都应该已经加载完成且标签页可用。
	// 这一条是真正要守的：404 不能把标签页弄成废状态。
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("404 页面等待就绪失败：%v", err)
	}
	if _, err := tab.Eval(ctx, `1+1`); err != nil {
		t.Fatalf("导航到 404 之后标签页不可用：%v", err)
	}

	// 地址应当就是那个 404 地址（没有被悄悄换掉或回退）
	got, err := tab.CurrentURL(ctx)
	if err != nil {
		t.Fatalf("读取当前地址失败：%v", err)
	}
	if got != notFound {
		t.Errorf("当前地址 = %q，期望 %q", got, notFound)
	}
}

// TestNavigateUnreachable 访问一个根本连不上的地址（端口无人监听）。
//
// 断言两件事：
//  1. Navigate 必须返回错误 —— 静默成功会把调用方骗得很惨；
//  2. 失败之后标签页**仍然可用** —— 这是最容易写坏的地方：
//     导航失败往往伴随 CDP 层面的异常，若实现因此把会话弄坏，
//     后续所有操作都会失败，而调用方只会看到「莫名其妙全都超时」。
func TestNavigateUnreachable(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	// 先正常访问一次，确保标签页处于可用状态
	gotoMain(t, tab, srv)

	// 用 RFC 2606 保留的 .invalid 顶级域：它保证无法解析，
	// 是「连不上」最干净的表现形式。
	//
	// 不能用 127.0.0.1:1 这类低位端口 —— Chrome 维护了一份「不安全端口」黑名单
	// （1、7、9、11……6000 等），访问它们会被拦成 net::ERR_UNSAFE_PORT，
	// 那是「浏览器主动拒绝」而不是「连不上」，测的就不是这回事了。
	ctx := ctxOf(t, tab, 30*time.Second)
	err := tab.Navigate(ctx, "http://unreachable.invalid/")
	if err == nil {
		t.Errorf("访问不可达地址期望返回错误，实际返回 nil")
	} else {
		t.Logf("访问不可达地址返回：%v", err)
	}

	// 关键断言：导航失败后标签页必须还能用
	if err := tab.Navigate(ctx, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("导航失败后无法再次导航（标签页已废）：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("导航失败后等待就绪失败：%v", err)
	}
	v, err := tab.Eval(ctx, `document.querySelector('#title').textContent`)
	if err != nil || v != "主标题" {
		t.Fatalf("导航失败后标签页不可用：got=%v err=%v", v, err)
	}
}

// TestNavigateSlowPageTimesOut 加载一个「响应体迟迟不返回」的页面，验证不会死等。
//
// 用 /slow：它先 flush 一部分 HTML 让导航开始，然后挂着不结束。
// 这类页面在真实环境里很常见（后端卡住、大文件慢慢吐），
// 如果没有超时保护，调用方会一直挂在里面。
//
// 实测语义：Navigate 本身就会等页面 load，所以对慢页面是 **Navigate 这一层就超时**，
// 而不是等到 WaitReady 才超时。本用例按这个真实行为断言 —— 核心是
// 「必须在 deadline 附近返回，而不是白等到默认 30s 之后」。
func TestNavigateSlowPageTimesOut(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	// deadline 明显小于默认超时：若实现没有超时保护，这里会挂满默认超时
	const budget = 3 * time.Second
	short, cancel := context.WithTimeout(tab.Ctx, budget)
	defer cancel()

	start := time.Now()
	err := tab.Navigate(short, srv.main.URL+"/slow")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("导航到慢页面应当超时返回错误，实际返回 nil（耗时 %v）", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("超时返回的错误不是 DeadlineExceeded（记录实际类型）：%v", err)
	}
	// 关键：必须在 deadline 附近返回，不能白等到默认超时（30s）之后
	if elapsed > budget+8*time.Second {
		t.Errorf("导航耗时 %v，远超 %v 的 deadline —— 超时没有真正生效", elapsed, budget)
	}
	t.Logf("慢页面导航在 %v 后返回：%v", elapsed, err)

	// 超时之后标签页必须还能继续用（超时不等于废掉会话）
	ctx := ctxOf(t, tab, 30*time.Second)
	if err := tab.Navigate(ctx, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("导航超时后无法再次导航（标签页已废）：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("导航超时后等待就绪失败：%v", err)
	}
	v, err := tab.Eval(ctx, `document.querySelector('#title').textContent`)
	if err != nil || v != "主标题" {
		t.Fatalf("导航超时后标签页不可用：got=%v err=%v", v, err)
	}
}

// TestReloadRefreshesPage 验证 Reload 真的重新加载了页面，而不只是"调了个方法没报错"。
//
// 判据选「Reload 前写进输入框的值会丢失」：页面重新加载后 DOM 回到初始状态，
// 输入框必然被清空。这比只断言 err == nil 强得多 ——
// 后者即使 Reload 什么都不做也能通过。
func TestReloadRefreshesPage(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 30*time.Second)

	const marker = "before-reload"

	// 读输入框当前值必须用 JS 取 DOM property。
	// 不能用 Element.Attribute(ctx, "value")：它读的是 **HTML attribute**，
	// 而 SetValue / SendKeys 改的是 **DOM property**；main.html 里的 #kw 没有
	// value 属性，所以 attribute 永远是空字符串 —— 用它会得出错误结论。
	readValue := func() string {
		t.Helper()
		v, err := tab.Eval(ctx, `document.querySelector('#kw').value`)
		if err != nil {
			t.Fatalf("读取输入框失败：%v", err)
		}
		s, _ := v.(string)
		return s
	}

	// 1) 往输入框里写点东西
	if err := tab.EleCSS("#kw").SetValue(ctx, marker); err != nil {
		t.Fatalf("设置输入框失败：%v", err)
	}
	if before := readValue(); before != marker {
		t.Fatalf("设置后读取到的值 = %q，期望 %q（前置条件不成立）", before, marker)
	}

	// 记录 Reload 前的地址
	urlBefore, err := tab.CurrentURL(ctx)
	if err != nil {
		t.Fatalf("读取当前地址失败：%v", err)
	}

	// 2) Reload
	if err := tab.Reload(ctx); err != nil {
		t.Fatalf("Reload 失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("Reload 后等待就绪失败：%v", err)
	}

	// 3) 地址应当不变（Reload 是刷新当前页，不是跳转）
	urlAfter, err := tab.CurrentURL(ctx)
	if err != nil {
		t.Fatalf("Reload 后读取地址失败：%v", err)
	}
	if urlAfter != urlBefore {
		t.Errorf("Reload 后地址变了：%q → %q", urlBefore, urlAfter)
	}

	// 4) 元素必须能重新拿到（页面已重建，旧句柄如果失效也能重新定位）
	el := tab.EleCSS("#kw")
	if n, err := el.Count(ctx); err != nil || n != 1 {
		t.Fatalf("Reload 后重新获取 #kw 失败：count=%d err=%v", n, err)
	}

	// 5) 关键断言：输入框的值必须被清空 —— 证明页面真的重新加载了
	if after := readValue(); after != "" {
		t.Errorf("Reload 后输入框仍保留旧值 %q —— 页面可能没有真正重新加载", after)
	}

	// 6) 页面内容应当是完整的初始状态（标题还在）
	v, err := tab.Eval(ctx, `document.querySelector('#title').textContent`)
	if err != nil || v != "主标题" {
		t.Fatalf("Reload 后页面内容异常：got=%v err=%v", v, err)
	}
}

// TestReloadAfterNavigation 连续 导航 → 刷新 → 再导航，确认状态不会串。
//
// 关注点是「上一次导航的残留」：例如 URL 快照没更新、
// 或者刷新刷到了上一页。用两个内容不同的页面交叉验证。
func TestReloadAfterNavigation(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	// 先去主页面，再跳到框架页，然后对框架页执行 Reload
	if err := tab.Navigate(ctx, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("导航到主页面失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待主页面失败：%v", err)
	}
	if err := tab.Navigate(ctx, srv.main.URL+"/frame.html"); err != nil {
		t.Fatalf("导航到框架页失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待框架页失败：%v", err)
	}

	if err := tab.Reload(ctx); err != nil {
		t.Fatalf("在框架页 Reload 失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("框架页 Reload 后等待就绪失败：%v", err)
	}

	// Reload 必须刷的是「当前页」而不是回到上一页
	got, err := tab.CurrentURL(ctx)
	if err != nil {
		t.Fatalf("读取当前地址失败：%v", err)
	}
	if want := srv.main.URL + "/frame.html"; got != want {
		t.Errorf("Reload 后地址 = %q，期望停留在 %q（刷新串页了）", got, want)
	}

	// 框架页的标志元素应当存在
	if n, err := tab.EleCSS("h1.h").Count(ctx); err != nil || n != 1 {
		t.Fatalf("框架页 Reload 后标志元素异常：count=%d err=%v", n, err)
	}
}

var _ = chromium.ErrClosed
