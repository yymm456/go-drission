//go:build smoke

package smoke

// 本文件覆盖 Back / Forward（历史导航）。
//
// 判据沿用本库既定风格：
//   - 到达历史边界**不是静默 no-op**，而是返回 ErrNoHistoryEntry（可 errors.Is），
//     且页面保持原样；
//   - 超时 / 取消语义与 Navigate 一致（走 t.run → safeCtx，调用方 deadline 优先）；
//   - 异常路径不 panic、不永久阻塞、Tab 仍然可用。
//
// 之所以必须真跑浏览器：history 的边界、pushState 造出来的条目、
// 「Navigate 后前进栈被截断」这些都是 Chromium 的行为，单元测试模拟不出来。

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// mustURL 读取当前地址，读不到直接判失败（导航类断言的前置条件）。
func mustURL(t *testing.T, tab *chromium.Tab, d time.Duration) string {
	t.Helper()
	u, err := tab.CurrentURL(freshCtx(t, tab, d))
	if err != nil {
		t.Fatalf("读取当前地址失败：%v", err)
	}
	return u
}

// gotoPage 导航到 /page-x 并等待就绪。
func gotoPage(t *testing.T, tab *chromium.Tab, srv *testServers, page string) {
	t.Helper()
	ctx := freshCtx(t, tab, 30*time.Second)
	if err := tab.Navigate(ctx, srv.main.URL+"/page-"+page); err != nil {
		t.Fatalf("导航到 /page-%s 失败：%v", page, err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待 /page-%s 就绪失败：%v", page, err)
	}
}

// mustTitle 读取 #title 的文本。
func mustTitle(t *testing.T, tab *chromium.Tab, d time.Duration) string {
	t.Helper()
	v, err := tab.Eval(freshCtx(t, tab, d), `document.querySelector('#title').textContent`)
	if err != nil {
		t.Fatalf("读取标题失败：%v", err)
	}
	s, _ := v.(string)
	return s
}

// newCleanHistoryTab 新建一个历史干净的标签页，用于历史边界类断言。
//
// 不能用 setup() 返回的共享标签页：全量跑时它已经被前面几十个用例导航过，
// 历史条目数量不可控，"退 N 次就到头"这种假设必然失效（单独跑能过、全量跑退不到头）。
// 新标签页的历史从 about:blank 开始，条数可预期。
func newCleanHistoryTab(t *testing.T) *chromium.Tab {
	t.Helper()
	b, shared := setup(t)
	tab, err := b.NewTab(freshCtx(t, shared, 30*time.Second))
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	t.Cleanup(func() {
		b.CloseTab(freshCtx(t, shared, 30*time.Second), tab)
	})
	return tab
}

// ---------------------------------------------------------------- 正常路径

func TestBackReturnsToPreviousPage(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	if got := mustTitle(t, tab, 15*time.Second); got != "页面B" {
		t.Fatalf("前置条件不成立：当前标题 = %q", got)
	}

	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Back 失败：%v", err)
	}
	if err := tab.WaitReady(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Back 后等待就绪失败：%v", err)
	}
	if got := mustTitle(t, tab, 15*time.Second); got != "页面A" {
		t.Errorf("Back 后标题 = %q，期望 页面A", got)
	}
}

func TestBackThenForward(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")

	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Back 失败：%v", err)
	}
	tab.WaitReady(freshCtx(t, tab, 30*time.Second))
	if got := mustTitle(t, tab, 15*time.Second); got != "页面A" {
		t.Fatalf("Back 后标题 = %q，期望 页面A", got)
	}

	if err := tab.Forward(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Forward 失败：%v", err)
	}
	tab.WaitReady(freshCtx(t, tab, 30*time.Second))
	if got := mustTitle(t, tab, 15*time.Second); got != "页面B" {
		t.Errorf("Forward 后标题 = %q，期望 页面B", got)
	}
}

func TestBackTwiceThroughThreePages(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	gotoPage(t, tab, srv, "c")

	for i, want := range []string{"页面B", "页面A"} {
		if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
			t.Fatalf("第 %d 次 Back 失败：%v", i+1, err)
		}
		tab.WaitReady(freshCtx(t, tab, 30*time.Second))
		if got := mustTitle(t, tab, 15*time.Second); got != want {
			t.Errorf("第 %d 次 Back 后标题 = %q，期望 %s", i+1, got, want)
		}
	}
}

// ---------------------------------------------------------------- 边界

func TestBackWithoutHistoryReturnsErrNoHistoryEntry(t *testing.T) {
	tab := newCleanHistoryTab(t)
	srv := startServers(t)
	gotoPage(t, tab, srv, "a")

	// 不能用「只访问过一页，所以第一次 Back 就该越界」——
	// 共享标签页的历史里本来就有 chrome://new-tab-page/ 这类初始条目，
	// 第一次 Back 往往会退到它。所以一路退到真的没得退为止。
	var lastErr error
	hit := false
	for i := 0; i < 6; i++ {
		lastErr = tab.Back(freshCtx(t, tab, 30*time.Second))
		if errors.Is(lastErr, chromium.ErrNoHistoryEntry) {
			hit = true
			break
		}
		if lastErr != nil {
			t.Fatalf("第 %d 次 Back 失败：%v", i+1, lastErr)
		}
		tab.WaitReady(freshCtx(t, tab, 30*time.Second))
	}
	if !hit {
		t.Fatalf("连续 Back 6 次都没有遇到 ErrNoHistoryEntry，最后错误：%v", lastErr)
	}

	// 越界之后页面必须保持原样：地址不该再变。
	// 不断言标题 —— 一路退到底可能停在 chrome://new-tab-page/ 这类页面上，
	// 它们没有 #title 元素，读标题只会得到 null。
	before := mustURL(t, tab, 15*time.Second)
	if err := tab.Back(freshCtx(t, tab, 20*time.Second)); !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Errorf("越界后再 Back 应继续返回 ErrNoHistoryEntry，实际：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); got != before {
		t.Errorf("Back 越界后地址变了：%q → %q（应保持不动）", before, got)
	}
}

func TestForwardWithoutHistoryReturnsErrNoHistoryEntry(t *testing.T) {
	tab := newCleanHistoryTab(t)
	srv := startServers(t)
	gotoPage(t, tab, srv, "a") // 没有更晚的一条

	before := mustURL(t, tab, 15*time.Second)
	err := tab.Forward(freshCtx(t, tab, 20*time.Second))
	if !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Fatalf("没有下一页时 Forward 应返回 ErrNoHistoryEntry，实际：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); got != before {
		t.Errorf("Forward 越界后地址变了：%q → %q", before, got)
	}
}

func TestBackUntilBoundaryThenForwardUntilBoundary(t *testing.T) {
	tab := newCleanHistoryTab(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	gotoPage(t, tab, srv, "c")

	// 连续 Back 直到越界。
	// 不假设「退 3 次正好到 a」——共享标签页的历史里还有 chrome://new-tab-page/
	// 这类初始条目，实际条数比看上去的多。
	backToStart := func() error {
		for i := 0; i < 8; i++ {
			err := tab.Back(freshCtx(t, tab, 30*time.Second))
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := backToStart(); !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Fatalf("一路 Back 应最终返回 ErrNoHistoryEntry，实际：%v", err)
	}

	// 连续 Forward 到头（同样不假设次数）
	fwdToEnd := func() error {
		for i := 0; i < 8; i++ {
			if err := tab.Forward(freshCtx(t, tab, 30*time.Second)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := fwdToEnd(); !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Errorf("一路 Forward 应最终返回 ErrNoHistoryEntry，实际：%v", err)
	}
}

// TestNavigateClearsForwardHistory 验证「Back 之后再 Navigate，前进栈会被截断」。
//
// 这是浏览器的标准行为（新导航会替换掉当前位置之后的所有条目），
// 不是我们能控制的，但必须确认本库的 Back/Forward 如实反映了它 ——
// 否则调用方会以为还能 Forward 回原来那一页。
func TestNavigateClearsForwardHistory(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	// 后退到 a，此时「前进」应能回到 b
	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Back 失败：%v", err)
	}
	tab.WaitReady(freshCtx(t, tab, 30*time.Second))
	if err := tab.Forward(freshCtx(t, tab, 20*time.Second)); !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Logf("后退后 Forward 仍可用（预期内）：%v", err)
	}

	// 重新构造：a → b → Back → 再导航到 c，然后 Forward 应当无路可走
	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("第二次 Back 失败：%v", err)
	}
	tab.WaitReady(freshCtx(t, tab, 30*time.Second))

	gotoPage(t, tab, srv, "c") // 新导航，前进栈被截断
	if err := tab.Forward(freshCtx(t, tab, 20*time.Second)); !errors.Is(err, chromium.ErrNoHistoryEntry) {
		t.Errorf("Navigate 之后 Forward 应返回 ErrNoHistoryEntry（前进栈已被清除），实际：%v", err)
	}
}

func TestBackThenReload(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Back 失败：%v", err)
	}
	reloadCtx := freshCtx(t, tab, 30*time.Second)
	if err := tab.WaitReady(reloadCtx); err != nil {
		t.Fatalf("Back 后等待就绪失败：%v", err)
	}
	urlAfterBack := mustURL(t, tab, 15*time.Second)

	// Reload 只刷新当前页，不应把历史一起改掉
	if err := tab.Reload(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Reload 失败：%v", err)
	}
	if err := tab.WaitReady(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("Reload 后等待就绪失败：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); got != urlAfterBack {
		t.Errorf("Back 后 Reload 地址变了：%q → %q", urlAfterBack, got)
	}
	if got := mustTitle(t, tab, 15*time.Second); got != "页面A" {
		t.Errorf("Back 后 Reload 标题 = %q，期望 页面A", got)
	}
}

// ---------------------------------------------------------------- SPA history

// TestBackForwardUsesChromiumHistory 用 pushState 造历史条目，
// 验证 Back/Forward 走的是 Chromium 的 history，而不是「按 URL 比对」之类的简化实现。
//
// pushState 只改地址栏与历史，不发请求、不换文档 —— 如果实现是拿 URL 列表自己算，
// 这类条目就会被漏掉。
func TestBackForwardUsesChromiumHistory(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	ctx := freshCtx(t, tab, 30*time.Second)
	if err := tab.Navigate(ctx, srv.main.URL+"/spa"); err != nil {
		t.Fatalf("导航到 SPA 失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待 SPA 就绪失败：%v", err)
	}

	// 用 pushState 造两条历史（顺便改写 DOM 以便断言）
	push := func(step string) {
		t.Helper()
		c := freshCtx(t, tab, 15*time.Second)
		js := `history.pushState(null, '', '/spa?step=` + step + `');` +
			`document.querySelector('#title').textContent = 'SPA-` + step + `';`
		if _, err := tab.Eval(c, js); err != nil {
			t.Fatalf("pushState(%s) 失败：%v", step, err)
		}
	}
	push("B")
	push("C")

	if got := mustTitle(t, tab, 15*time.Second); got != "SPA-C" {
		t.Fatalf("前置条件不成立：当前标题 = %q", got)
	}

	// 第一次 Back：应回到 B
	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("SPA Back 失败：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); !containsSuffix(got, "/spa?step=B") {
		t.Errorf("SPA 第一次 Back 后地址 = %q，期望以 /spa?step=B 结尾", got)
	}

	// 第二次 Back：应回到最初的 /spa
	if err := tab.Back(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("SPA 第二次 Back 失败：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); !containsSuffix(got, "/spa") {
		t.Errorf("SPA 第二次 Back 后地址 = %q，期望回到 /spa", got)
	}

	// Forward 应能再回到 B
	if err := tab.Forward(freshCtx(t, tab, 30*time.Second)); err != nil {
		t.Fatalf("SPA Forward 失败：%v", err)
	}
	if got := mustURL(t, tab, 15*time.Second); !containsSuffix(got, "/spa?step=B") {
		t.Errorf("SPA Forward 后地址 = %q，期望 /spa?step=B", got)
	}
}

// ---------------------------------------------------------------- 稳定性

// TestBackForwardRepeatedly 连续来回翻历史，确认不会累积错误状态。
//
// 轮次同样由 GO_DRISSION_STABILITY_ROUNDS 控制（默认 5，这里再乘 2 因为
// 一轮含 Back 与 Forward 各一次）。每次 Back/Forward 都是毫秒级，
// 所以 100 轮也就一两秒 —— 但足以暴露「历史指针算错」「越界后状态坏掉」这类问题。
func TestBackForwardRepeatedly(t *testing.T) {
	tab := newCleanHistoryTab(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")
	gotoPage(t, tab, srv, "c")

	rounds := stabilityRounds() * 2
	for i := 0; i < rounds; i++ {
		// 一路退到底（必然遇到 ErrNoHistoryEntry）
		var err error
		for j := 0; j < 8; j++ {
			err = tab.Back(freshCtx(t, tab, 20*time.Second))
			if err != nil {
				break
			}
		}
		if !errors.Is(err, chromium.ErrNoHistoryEntry) {
			t.Fatalf("第 %d 轮：一路 Back 未如期遇到 ErrNoHistoryEntry，实际：%v", i+1, err)
		}

		// 再一路进到底
		for j := 0; j < 8; j++ {
			err = tab.Forward(freshCtx(t, tab, 20*time.Second))
			if err != nil {
				break
			}
		}
		if !errors.Is(err, chromium.ErrNoHistoryEntry) {
			t.Fatalf("第 %d 轮：一路 Forward 未如期遇到 ErrNoHistoryEntry，实际：%v", i+1, err)
		}
	}

	// 折腾完之后页面必须还是可用的正常状态
	if _, err := tab.Eval(freshCtx(t, tab, 15*time.Second), `1+1`); err != nil {
		t.Fatalf("连续 %d 轮历史导航后标签页不可用：%v", rounds, err)
	}
	if got := mustTitle(t, tab, 15*time.Second); got != "页面C" {
		t.Errorf("结束时标题 = %q，期望停在 页面C（历史末端）", got)
	}
	t.Logf("连续 %d 轮 Back/Forward 完成", rounds)
}

// TestBackForwardMultipleTabs 多个标签页各自翻历史，互不干扰。
func TestBackForwardMultipleTabs(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	const n = 3
	tabs := make([]*chromium.Tab, 0, n)
	for i := 0; i < n; i++ {
		tb, err := b.NewTab(freshCtx(t, shared, 30*time.Second))
		if err != nil {
			t.Fatalf("新建第 %d 个标签页失败：%v", i+1, err)
		}
		t.Cleanup(func() { b.CloseTab(freshCtx(t, shared, 30*time.Second), tb) })
		gotoPage(t, tb, srv, "a")
		gotoPage(t, tb, srv, "b")
		tabs = append(tabs, tb)
	}

	// 每个标签页各自 Back 一次，都应回到 a
	for i, tb := range tabs {
		if err := tb.Back(freshCtx(t, tb, 30*time.Second)); err != nil {
			t.Fatalf("第 %d 个标签页 Back 失败：%v", i+1, err)
		}
		if got := mustTitle(t, tb, 15*time.Second); got != "页面A" {
			t.Errorf("第 %d 个标签页 Back 后标题 = %q，期望 页面A", i+1, got)
		}
	}

	// 其中一个再 Forward，不应影响其它标签页
	if err := tabs[0].Forward(freshCtx(t, tabs[0], 30*time.Second)); err != nil {
		t.Fatalf("第 1 个标签页 Forward 失败：%v", err)
	}
	if got := mustTitle(t, tabs[0], 15*time.Second); got != "页面B" {
		t.Errorf("第 1 个标签页 Forward 后标题 = %q，期望 页面B", got)
	}
	for i := 1; i < n; i++ {
		if got := mustTitle(t, tabs[i], 15*time.Second); got != "页面A" {
			t.Errorf("第 %d 个标签页被别人的 Forward 影响了：标题 = %q", i+1, got)
		}
	}
}

// containsSuffix 判断地址是否以给定串结尾（忽略协议与端口差异）。
func containsSuffix(u, suffix string) bool {
	return len(u) >= len(suffix) && u[len(u)-len(suffix):] == suffix
}

// ---------------------------------------------------------------- 异常路径

// TestBackWithCancelledContext 用已取消的 ctx 调 Back，必须立刻返回错误。
func TestBackWithCancelledContext(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")

	cancelled, cancel := context.WithTimeout(tab.Ctx, 1*time.Nanosecond)
	defer cancel()
	<-cancelled.Done()

	if err := mustReturnEdge(t, edgeTimeout, "已取消 ctx 的 Back", func() error {
		return tab.Back(cancelled)
	}); err == nil {
		t.Errorf("已取消 ctx 的 Back 期望返回错误，实际返回 nil")
	}

	// 标签页必须仍然可用
	if _, err := tab.Eval(freshCtx(t, tab, 15*time.Second), `1+1`); err != nil {
		t.Fatalf("Back 被取消后标签页不可用：%v", err)
	}
}

// TestBackAfterTabClosed 标签页关闭之后 Back 必须报错，不能 panic。
//
// 必须关**自己新建的**标签页：setup() 返回的是所有用例共享的那个，
// 关掉它会让后续每个用 setup() 的用例都拿到已取消的会话（表现为莫名其妙的
// context canceled），而且失败位置离这里很远，极难查。
func TestBackAfterTabClosed(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	victim, err := b.NewTab(freshCtx(t, shared, 30*time.Second))
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	gotoPage(t, victim, srv, "a")
	gotoPage(t, victim, srv, "b")

	b.CloseTab(freshCtx(t, victim, 30*time.Second), victim)

	if err := mustReturnEdge(t, edgeTimeout, "标签页关闭后的 Back", func() error {
		return victim.Back(freshCtx(t, victim, 10*time.Second))
	}); err == nil {
		t.Errorf("标签页关闭后 Back 期望返回错误，实际返回 nil")
	}
}

// TestBackAfterBrowserClosed 浏览器关闭之后 Back 必须报错，不能 panic。
func TestBackAfterBrowserClosed(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	srv := startServers(t)
	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")

	b.Close()

	if err := mustReturnEdge(t, edgeTimeout, "浏览器关闭后的 Back", func() error {
		return tab.Back(context.Background())
	}); err == nil {
		t.Errorf("浏览器关闭后 Back 期望返回错误，实际返回 nil")
	}
}

// TestBackAfterChromeKilled Chrome 被强杀之后 Back 必须报错、不永久阻塞。
func TestBackAfterChromeKilled(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	srv := startServers(t)
	gotoPage(t, tab, srv, "a")
	gotoPage(t, tab, srv, "b")

	pid := b.PID()
	if pid <= 0 {
		t.Skipf("PID()=%d，跳过强杀步骤", pid)
	}
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Kill(); err != nil {
			t.Fatalf("强杀进程 %d 失败：%v", pid, err)
		}
	}

	start := time.Now()
	err := mustReturnEdge(t, edgeTimeout, "Chrome 被杀后的 Back", func() error {
		return tab.Back(freshCtx(t, tab, 15*time.Second))
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("Chrome 被杀后 Back 期望返回错误，实际返回 nil")
	}
	if elapsed > 25*time.Second {
		t.Errorf("Back 耗时 %v，疑似永久阻塞", elapsed)
	}
	t.Logf("Chrome 被杀后 Back 在 %v 返回：%v", elapsed, err)
}

// TestBackToSlowPageSettlesByURL 记录 Back 的「完成」判据是地址而不是 load。
//
// 这里刻意**不**断言「Back 到慢页面会超时」——那与实现不符，硬凑只会得到一个脆弱用例。
// 真实语义是：
//   - Navigate 等 load，所以慢页面会按 ctx 超时；
//   - Back 等「地址变成目标条目」，因为历史导航不会再触发一次 load
//     （等它会一路卡到超时，实测正常历史导航都会被拖满 30s）。
//
// 于是退到一个响应体迟迟不返回的页面时，Back 在地址确认后即返回，
// 页面是否真的加载完由调用方按需 WaitReady —— 这一点与 Reload 同类。
//
// 这条用例守的是：Back 不会卡死、地址确实切过去了、之后标签页仍可用。
func TestBackToSlowPageSettlesByURL(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)

	gotoPage(t, tab, srv, "a")
	// 导航到慢页面：Navigate 等 load，这里预期会超时，属于正常现象
	if err := tab.Navigate(freshCtx(t, tab, 20*time.Second), srv.main.URL+"/slow"); err != nil {
		t.Logf("导航到慢页面返回（Navigate 等 load，超时属预期）：%v", err)
	}
	gotoPage(t, tab, srv, "b")

	start := time.Now()
	err := tab.Back(freshCtx(t, tab, 15*time.Second))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Back 失败：%v", err)
	}
	// 关键：不能卡死到 ctx 耗尽
	if elapsed > 12*time.Second {
		t.Errorf("Back 耗时 %v，疑似卡在等待上（历史导航不该等 load）", elapsed)
	}
	t.Logf("Back 到慢页面在 %v 后返回（地址判据，不等 load）", elapsed)

	if got := mustURL(t, tab, 15*time.Second); !containsSuffix(got, "/slow") {
		t.Errorf("Back 后地址 = %q，期望回到 /slow", got)
	}

	// 之后标签页必须还能继续操作
	if _, err := tab.Eval(freshCtx(t, tab, 15*time.Second), `1+1`); err != nil {
		t.Fatalf("Back 之后标签页不可用：%v", err)
	}
}
