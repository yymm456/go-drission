//go:build smoke

// lifecycle_test.go 集中覆盖「target / 浏览器上下文生命周期」这一块的真实浏览器回归。
//
// 为什么单独成文件：本轮修复触碰了 chromium 的 target 附着与上下文生命周期
// （历史缺陷 BUG-06 / BUG-07）——
//   - BUG-06：标签页列表改走 CDP Target.getTargets，并按 browserContextId 只收默认上下文；
//   - BUG-07：建 target / attach 的 chromedp.Run 必须留在长生命周期的 tabCtx 上，
//     只用 runAbandonable 限制「等待预算」，绝不把 Run 的 ctx 换成会取消的子上下文。
//
// 这两处写错后的症状是「标签页建出来了、句柄也拿到了，但之后每个操作都卡到超时」，
// 或者「隔离上下文里的页被 Browser 重复托管」。普通单测（不依赖浏览器）覆盖不到，
// 因为只有真实 Chrome 才会触发 attach 时的事件分发绑定。所以集中放在这里，用
// `-race` + 真实浏览器跑：
//
//	go test -tags smoke -race -count=1 -timeout 15m ./smoke/...
package smoke

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yymm456/go-drission/chromium"
)

// tabWorks 断言一个标签页「事件流仍然存活」：能导航、能求值。
//
// 这是 BUG-07 的核心判据。若 attach 误用了会被取消 / 超时的子上下文，chromedp 绑定在
// 它上面的目标页事件分发 goroutine 会退出，此后该标签页的每个 CDP 命令都收不到应答，
// 表现为「一直等到自己的 ctx 超时」。所以这里除了结果，还要卡耗时上限，避免把
// 「白等一个完整超时」误判成通过。
func tabWorks(t *testing.T, tab *chromium.Tab, pageURL string) {
	t.Helper()

	ctx := ctxOf(t, tab, 20*time.Second)
	start := time.Now()
	if err := tab.Navigate(ctx, pageURL); err != nil {
		t.Fatalf("标签页 %s 导航失败（事件流可能已断）：%v", tab.ID, err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("标签页 %s 等待就绪失败：%v", tab.ID, err)
	}
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("标签页 %s 导航耗时 %v，疑似事件分发已断、在等超时", tab.ID, elapsed)
	}

	v, err := tab.Eval(ctx, `document.querySelector('#title').textContent`)
	if err != nil || v != "主标题" {
		t.Fatalf("标签页 %s 求值失败：got=%v err=%v", tab.ID, v, err)
	}
}

// countID 统计列表里某个 target ID 出现几次（用于确认没有重复托管）。
func countID(tabs []*chromium.Tab, id target.ID) int {
	n := 0
	for _, tb := range tabs {
		if tb.ID == id {
			n++
		}
	}
	return n
}

// lifecyclePollIters 是并发交叉测试里「另一个 goroutine 轮询 Tabs()」的次数。
// 这个交叉曾是 BUG-11 的触发条件（Tabs 会拿旧快照把 NewTab 刚建的标签页 cancel 掉），
// 保留它作为回归压力。
const lifecyclePollIters = 20

// ---------------------------------------------------------------- 多标签

// TestLifecycleMultiTabs 覆盖默认上下文里的多标签生命周期：
// 连续新建多个标签页 → 各自独立导航 → 全部被 Browser 托管 → 关闭一个后列表不残留，
// 且关闭它不会连带打断其余标签页的事件流。
func TestLifecycleMultiTabs(t *testing.T) {
	b, _ := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, sharedTab, 120*time.Second)

	const n = 3
	tabs := make([]*chromium.Tab, 0, n)
	for i := range n {
		tb, err := b.NewTab(ctx)
		if err != nil {
			t.Fatalf("新建第 %d 个标签页失败：%v", i, err)
		}
		tabs = append(tabs, tb)
		// 每个标签页独立导航：验证多个 target 并存时，各自的会话互不干扰
		tabWorks(t, tb, fmt.Sprintf("%s/main.html?tab=%d", srv.main.URL, i))
	}

	// 默认上下文里的标签页都应被 Browser 托管，且每个 ID 只出现一次
	managed, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("读取 Browser.Tabs 失败：%v", err)
	}
	for i, tb := range tabs {
		switch got := countID(managed, tb.ID); got {
		case 1:
		case 0:
			t.Fatalf("第 %d 个标签页 %s 未出现在 Browser.Tabs 里", i, tb.ID)
		default:
			t.Fatalf("第 %d 个标签页 %s 在 Browser.Tabs 里出现 %d 次（重复托管）", i, tb.ID, got)
		}
	}

	// 关闭中间那个：列表里必须消失，其余两个仍能继续操作
	victim := tabs[1]
	b.CloseTab(ctx, victim)

	after, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("CloseTab 后读取 Browser.Tabs 失败：%v", err)
	}
	if countID(after, victim.ID) != 0 {
		t.Fatalf("CloseTab 后 %s 仍留在 Browser.Tabs 里（陈旧条目）", victim.ID)
	}
	for i, tb := range []*chromium.Tab{tabs[0], tabs[2]} {
		if countID(after, tb.ID) != 1 {
			t.Fatalf("关闭一个标签页后，另一个（第 %d 个）%s 不应从列表消失", i, tb.ID)
		}
		// 事件流不能被「别人的关闭」连带打断
		v, err := tb.Eval(ctxOf(t, tb, 15*time.Second), `document.querySelector('#title').textContent`)
		if err != nil || v != "主标题" {
			t.Fatalf("关闭相邻标签页后，标签页 %s 无法继续求值：got=%v err=%v", tb.ID, v, err)
		}
	}

	// 收尾：关掉本用例创建的标签页，避免污染共享浏览器
	for _, tb := range []*chromium.Tab{tabs[0], tabs[2]} {
		b.CloseTab(ctx, tb)
	}
	final, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("收尾读取 Browser.Tabs 失败：%v", err)
	}
	for _, tb := range tabs {
		if countID(final, tb.ID) != 0 {
			t.Fatalf("收尾后标签页 %s 仍在 Browser.Tabs 里", tb.ID)
		}
	}
}

// TestLifecycleTabOutlivesCallerDeadline 是 BUG-07 的定点回归：
// 用「带短 deadline 的调用方 ctx」建标签页；attach 完成后让那个 ctx 彻底过期，
// 标签页必须照常可用。
//
// 原理：tabInitBudget(ctx) 只作 runAbandonable 的 watchCtx（决定「等多久就放弃等待」），
// chromedp.Run 必须留在长生命周期的 tabCtx 上。若有人把 Run 的 ctx 换回会被取消的子
// 上下文，事件分发会被连带杀死，这里就会卡到超时。
func TestLifecycleTabOutlivesCallerDeadline(t *testing.T) {
	b, _ := setup(t)
	srv := startServers(t)

	caller, cancelCaller := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCaller()

	tab, err := b.NewTab(caller)
	if err != nil {
		t.Fatalf("用带 deadline 的调用方 ctx 新建标签页失败：%v", err)
	}

	// 让调用方 ctx 彻底过期，确保任何挂在它上面的取消传导都已生效
	cancelCaller()
	<-caller.Done()
	time.Sleep(300 * time.Millisecond)

	// 事件流必须仍然活着
	tabWorks(t, tab, srv.main.URL+"/main.html")

	b.CloseTab(ctxOf(t, sharedTab, 20*time.Second), tab)
}

// TestLifecycleConcurrentNewTabs 用 -race 压 NewTab 与 Tabs 的互斥保护。
//
// NewTab 与 Tabs 都持 targetListMu：NewTab 串行「快照 → 建 target → 再快照 → 求差集」，
// Tabs 串行「取快照 → syncTabs」。二者互斥正是 BUG-11 的修复点——否则 Tabs 会拿旧快照
// 把 NewTab 刚建的 target 当成已关闭 cancel 掉（cancel == CloseTarget）。这里另起一个
// goroutine 持续调 Tabs() 与并发 NewTab 交叉，断言：N 个并发新建拿到 N 个互异 ID，
// 都能被 Browser.Tabs 恰好托管一次，且事后每个都仍可用（没被误关）。
func TestLifecycleConcurrentNewTabs(t *testing.T) {
	b, _ := setup(t)
	ctx := ctxOf(t, sharedTab, 120*time.Second)

	const n = 4

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []*chromium.Tab
	)
	for range n {
		wg.Go(func() {
			tb, err := b.NewTab(ctx)
			if err != nil {
				t.Errorf("并发 NewTab 失败：%v", err)
				return
			}
			mu.Lock()
			created = append(created, tb)
			mu.Unlock()
		})
	}

	// 并发读列表：与 NewTab 交叉，验证 targetListMu 互斥确实生效（新建的标签页不会被 Tabs 的 syncTabs 误关）
	stopPolling := make(chan struct{})
	var poller sync.WaitGroup
	poller.Go(func() {
		for range lifecyclePollIters {
			select {
			case <-stopPolling:
				return
			default:
				_, _ = b.Tabs(ctx)
			}
		}
	})

	wg.Wait()
	close(stopPolling)
	poller.Wait()

	if len(created) != n {
		t.Fatalf("期望 %d 个并发新建结果，实际 %d（其中含失败）", n, len(created))
	}

	seen := make(map[target.ID]bool, n)
	for _, tb := range created {
		if seen[tb.ID] {
			t.Fatalf("并发 NewTab 返回了重复的 target ID：%s", tb.ID)
		}
		seen[tb.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("期望 %d 个互异 target，实际 %d", n, len(seen))
	}

	// 更强的断言：每个标签页必须仍然「可用」。
	// syncTabs 对「快照里没有」的标签页会调 cancel，而 chromedp 的 cancel 会真的
	// CloseTarget —— 若被误 cancel，这里求值会直接失败（target 已不存在），
	// 这也是「Tabs() 会销毁并发新建的标签页」最直接的证据。
	for _, tb := range created {
		if _, err := tb.Eval(ctxOf(t, tb, 15*time.Second), `1+1`); err != nil {
			t.Fatalf("并发新建的标签页 %s 已不可用（target 可能被 Tabs() 误关）：%v", tb.ID, err)
		}
	}

	managed, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("读取 Browser.Tabs 失败：%v", err)
	}
	for _, tb := range created {
		if got := countID(managed, tb.ID); got != 1 {
			t.Fatalf("并发新建的标签页 %s 在 Browser.Tabs 里出现 %d 次（期望 1）", tb.ID, got)
		}
	}

	// 收尾
	for _, tb := range created {
		b.CloseTab(ctx, tb)
	}
}

// ---------------------------------------------------------------- 隔离上下文

// TestLifecycleIsolatedContexts 覆盖隔离上下文的全生命周期：
// Cookie 隔离 → 不被 Browser 重复托管 → CloseTab 后不留陈旧条目 → 关闭一个上下文
// 不影响另一个 → Contexts() 注册表随之增删 → 同名可重建。
func TestLifecycleIsolatedContexts(t *testing.T) {
	b, _ := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, sharedTab, 150*time.Second)

	suffix := time.Now().UnixNano()
	nameA := fmt.Sprintf("smoke_life_a_%d", suffix)
	nameB := fmt.Sprintf("smoke_life_b_%d", suffix)

	bcA, err := b.Context(ctx, nameA)
	if err != nil {
		t.Fatalf("创建隔离上下文 A 失败：%v", err)
	}
	bcB, err := b.Context(ctx, nameB)
	if err != nil {
		bcA.Close()
		t.Fatalf("创建隔离上下文 B 失败：%v", err)
	}
	defer bcB.Close()

	// A 里开两个标签页：首个是 newWindow，第二个作为标签页落入同一窗口
	tabA1, err := bcA.NewTab(ctx)
	if err != nil {
		t.Fatalf("A 内新建首个标签页失败：%v", err)
	}
	tabA2, err := bcA.NewTab(ctx)
	if err != nil {
		t.Fatalf("A 内新建第二个标签页失败：%v", err)
	}
	if got := len(bcA.Tabs()); got != 2 {
		t.Fatalf("A 内应有 2 个标签页，实际 %d", got)
	}

	// Cookie 隔离：A 访问过 /set-cookie，所以 /profile 能识别；B 从未访问，必须拿不到
	if err := tabA1.Navigate(ctxOf(t, tabA1, 30*time.Second), srv.main.URL+"/set-cookie"); err != nil {
		t.Fatalf("A 导航到 set-cookie 失败：%v", err)
	}
	ctxA1 := ctxOf(t, tabA1, 30*time.Second)
	if err := tabA1.WaitReady(ctxA1); err != nil {
		t.Fatalf("A 等待就绪失败：%v", err)
	}
	if err := tabA1.Navigate(ctxA1, srv.main.URL+"/profile"); err != nil {
		t.Fatalf("A 导航到 profile 失败：%v", err)
	}
	bodyA, err := tabA1.HTML(ctxA1)
	if err != nil {
		t.Fatalf("A 读取 profile 内容失败：%v", err)
	}
	if !strings.Contains(bodyA, "hello abc123") {
		t.Fatalf("A 上下文内 Cookie 未生效：%q", bodyA)
	}

	tabB1, err := bcB.NewTab(ctx)
	if err != nil {
		t.Fatalf("B 内新建标签页失败：%v", err)
	}
	ctxB1 := ctxOf(t, tabB1, 30*time.Second)
	if err := tabB1.Navigate(ctxB1, srv.main.URL+"/profile"); err != nil {
		t.Fatalf("B 导航到 profile 失败：%v", err)
	}
	bodyB, err := tabB1.HTML(ctxB1)
	if err != nil {
		t.Fatalf("B 读取 profile 内容失败：%v", err)
	}
	if strings.Contains(bodyB, "hello abc123") {
		t.Fatalf("隔离上下文未隔离 Cookie：B 的 /profile 也拿到了 A 的登录态：%q", bodyB)
	}
	if !strings.Contains(bodyB, "no cookie") {
		t.Fatalf("B 的 /profile 预期应为未登录（no cookie），实际：%q", bodyB)
	}

	// 隔离上下文里的标签页绝不能被 Browser 重复托管（BUG-06）
	managed, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("读取 Browser.Tabs 失败：%v", err)
	}
	for _, tb := range []*chromium.Tab{tabA1, tabA2, tabB1} {
		if countID(managed, tb.ID) != 0 {
			t.Fatalf("隔离上下文标签页 %s 不应出现在 Browser.Tabs 里", tb.ID)
		}
	}

	// CloseTab 之后 BrowserContext.Tabs() 不能留下陈旧条目
	// （关第二个而不是首个：首个是独立窗口，关它可能连窗口一起关掉，不属于本用例要验证的点）
	bcA.CloseTab(ctx, tabA2)
	survivors := bcA.Tabs()
	if countID(survivors, tabA2.ID) != 0 {
		t.Fatalf("CloseTab 后 %s 仍留在 BrowserContext.Tabs() 里（陈旧条目）", tabA2.ID)
	}
	if countID(survivors, tabA1.ID) != 1 {
		t.Fatalf("CloseTab 误伤了另一个标签页 %s", tabA1.ID)
	}

	// 关掉一个标签页后，同一上下文仍可继续新增
	tabA3, err := bcA.NewTab(ctx)
	if err != nil {
		t.Fatalf("关闭 A 内一个标签页后无法继续新建：%v", err)
	}
	if got := len(bcA.Tabs()); got != 2 {
		t.Fatalf("继续新建后 A 内应有 2 个标签页，实际 %d", got)
	}
	// 新旧标签页都还能用
	tabWorks(t, tabA1, srv.main.URL+"/main.html")
	tabWorks(t, tabA3, srv.main.URL+"/main.html")

	// 关闭上下文 A：注册表随之清理，B 不受影响
	bcA.Close()
	if names := b.Contexts(); slices.Contains(names, nameA) {
		t.Fatalf("A 已关闭，Contexts() 仍包含 %q：%v", nameA, names)
	}
	if names := b.Contexts(); !slices.Contains(names, nameB) {
		t.Fatalf("B 仍存活，Contexts() 却缺少 %q：%v", nameB, names)
	}
	tabWorks(t, tabB1, srv.main.URL+"/main.html")

	// 同名可重建（关闭后注册表应已清理）
	bcA2, err := b.Context(ctx, nameA)
	if err != nil {
		t.Fatalf("A 关闭后同名重建失败：%v", err)
	}
	defer bcA2.Close()
	if bcA2.Name() != nameA {
		t.Fatalf("重建的上下文名不符：%q", bcA2.Name())
	}
	tabA4, err := bcA2.NewTab(ctx)
	if err != nil {
		t.Fatalf("重建的 A 内新建标签页失败：%v", err)
	}
	tabWorks(t, tabA4, srv.main.URL+"/main.html")

	// 重建的 A 仍然与 B 隔离：B 的登录态不该出现在新 A 里
	ctxA4 := ctxOf(t, tabA4, 30*time.Second)
	if err := tabA4.Navigate(ctxA4, srv.main.URL+"/profile"); err != nil {
		t.Fatalf("重建的 A 导航到 profile 失败：%v", err)
	}
	bodyA4, err := tabA4.HTML(ctxA4)
	if err != nil {
		t.Fatalf("重建的 A 读取 profile 内容失败：%v", err)
	}
	if strings.Contains(bodyA4, "hello abc123") {
		t.Fatalf("重建的 A 不应继承任何登录态，实际：%q", bodyA4)
	}
}
