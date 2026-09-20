//go:build smoke

package smoke

// Tab.Close 的行为验证。
//
// 这里守的是它在整个生命周期里的位置：关闭要同时做到「Chrome 里的 target 消失」、
//「本标签页的 chromedp 会话被释放」、以及「Browser 台账里不再留它」。第三条依赖
// browser/targets.go 里 syncTabs 的惰性兜底，所以用例全部通过公开的 Tabs() 来观察，
// 而不是去翻内部字段 —— 那样测的才是调用方真正看得到的行为。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// newOwnTab 新建一个归本用例所有的标签页。
//
// 刻意不动 setup() 返回的共享标签页：关掉它会让后续每个用 setup() 的用例
// 都拿到已取消的会话（表现为莫名其妙的 context canceled，失败位置离得很远）。
// 注册的清理是兜底的，Close 幂等所以与用例自己的关闭不冲突。
func newOwnTab(t *testing.T, b *chromium.Browser, via *chromium.Tab) *chromium.Tab {
	t.Helper()
	tab, err := b.NewTab(freshCtx(t, via, 30*time.Second))
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	t.Cleanup(tab.Close)
	return tab
}

// hasTab 报告 Tabs() 的结果里是否还有指定 ID。
func hasTab(t *testing.T, b *chromium.Browser, via *chromium.Tab, id any) bool {
	t.Helper()
	tabs, err := b.Tabs(freshCtx(t, via, 20*time.Second))
	if err != nil {
		t.Fatalf("读取标签页列表失败：%v", err)
	}
	for _, x := range tabs {
		if any(x.ID) == id {
			return true
		}
	}
	return false
}

// TestTabCloseRemovesFromBrowserAndDisablesTab 关闭后：Browser 不再列出它，它自己也不能再用。
func TestTabCloseRemovesFromBrowserAndDisablesTab(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	tab := newOwnTab(t, b, shared)
	gotoPage(t, tab, srv, "a")

	id := any(tab.ID)
	if !hasTab(t, b, shared, id) {
		t.Fatal("前置条件不成立：新建的标签页不在 Tabs() 里")
	}

	tab.Close()

	// 台账里不该再有它（syncTabs 会把已消失的 target 摘掉）
	if hasTab(t, b, shared, id) {
		t.Errorf("Close 之后 Tabs() 仍返回该标签页")
	}

	// 它自己也不该还能操作
	if _, err := tab.Eval(freshCtx(t, tab, 5*time.Second), `1+1`); err == nil {
		t.Errorf("关闭之后 Eval 期望返回错误，实际返回 nil")
	}
}

// TestTabCloseIsIdempotent 重复关闭必须安全（不 panic、不卡住），且不影响 Browser 继续用。
func TestTabCloseIsIdempotent(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	tab := newOwnTab(t, b, shared)
	gotoPage(t, tab, srv, "a")

	err := mustReturnEdge(t, edgeTimeout, "连续三次 Close", func() error {
		tab.Close()
		tab.Close()
		tab.Close()
		return nil
	})
	if err != nil {
		t.Fatalf("重复 Close 出错：%v", err)
	}

	// Browser 必须仍然可用
	other := newOwnTab(t, b, shared)
	gotoPage(t, other, srv, "b")
}

// TestTabCloseDoesNotAffectOtherTabs 关掉一个不能影响别的标签页。
func TestTabCloseDoesNotAffectOtherTabs(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	closing := newOwnTab(t, b, shared)
	keeper := newOwnTab(t, b, shared)
	gotoPage(t, closing, srv, "a")
	gotoPage(t, keeper, srv, "b")

	closing.Close()

	// 另一个标签页必须照常工作
	if got := mustTitle(t, keeper, 15*time.Second); got != "页面B" {
		t.Errorf("关闭其它标签页后，本标签页标题 = %q，期望 页面B", got)
	}
	if err := keeper.Navigate(freshCtx(t, keeper, 20*time.Second), srv.main.URL+"/page-c"); err != nil {
		t.Errorf("关闭其它标签页后，本标签页无法导航：%v", err)
	}
	if !hasTab(t, b, shared, any(keeper.ID)) {
		t.Errorf("存活的那个标签页从 Tabs() 里消失了")
	}
}

// TestTabCloseViaDefer 复现最常见的用法：NewTab 之后 defer tab.Close()。
//
// 这正是这个 API 存在的理由 —— 让「谁创建、谁关闭」能在同一个作用域里写清楚。
func TestTabCloseViaDefer(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	var id any
	func() {
		tab := newOwnTab(t, b, shared)
		defer tab.Close() // 用户要写的那一行
		id = any(tab.ID)

		if err := tab.Navigate(freshCtx(t, tab, 30*time.Second), srv.main.URL+"/page-a"); err != nil {
			t.Fatalf("导航失败：%v", err)
		}
		if err := tab.WaitReady(freshCtx(t, tab, 30*time.Second)); err != nil {
			t.Fatalf("等待就绪失败：%v", err)
		}
	}()

	if hasTab(t, b, shared, id) {
		t.Errorf("defer tab.Close() 出作用域后，标签页仍在 Tabs() 里")
	}
	// Browser 不受影响
	after := newOwnTab(t, b, shared)
	gotoPage(t, after, srv, "b")
}

// TestTabCloseLastTab 关掉浏览器里最后一个标签页：不能 panic，Browser 对象也要有个确定的状态。
//
// 用独立实例，避免污染共享浏览器。这个场景的行为由 Chrome 决定（可能连带关闭窗口），
// 所以断言只要求「不崩、有确定结果」，并把实际行为记录下来。
func TestTabCloseLastTab(t *testing.T) {
	b, tab, err := chromium.OpenPage(context.Background(),
		nextFreePort(),
		chromium.WithUserDataDir(t.TempDir()),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	if err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("启动浏览器失败：%v", err)
	}
	t.Cleanup(b.Close)

	if err := mustReturnEdge(t, edgeTimeout, "关闭最后一个标签页", func() error {
		tab.Close()
		return nil
	}); err != nil {
		t.Fatalf("关闭最后一个标签页出错：%v", err)
	}

	// Browser 对象本身必须仍然可调用（哪怕内部 target 已经没了）。
	//
	// 这里刻意传裸 ctx：Tabs() 内部走 boundedRootCtx，父是 Browser 自己的 rootCtx
	// （自带路由），所以不需要调用方提供标签页 ctx —— 这一点在标签页全关掉之后很重要，
	// 因为那时调用方已经拿不到任何可用的 tab.Ctx 了。
	// 反过来说，**别传一个已经取消的 ctx**：它的取消会通过 AfterFunc 传导进去，
	// 表现为 "查询 target 列表失败: context canceled"。
	list, err := b.Tabs(context.Background())
	if err != nil {
		t.Errorf("关闭最后一个标签页后 Tabs() 报错：%v", err)
	} else {
		t.Logf("关闭最后一个标签页后，Tabs() 返回 %d 个（Chrome 可能已连带关闭窗口）", len(list))
	}

	// 再关它的 Browser 必须安全（幂等收尾）
	if err := mustReturnEdge(t, edgeTimeout, "关闭后的 b.Close()", func() error {
		b.Close()
		return nil
	}); err != nil {
		t.Fatalf("b.Close() 出错：%v", err)
	}
}

// TestTabCloseConcurrent 多个标签页被并发关闭，且与另一个标签页上的读写交织。
//
// 关注的是「关一个不影响别人」在并发下是否依然成立 —— 每条关闭路径都不碰
// Browser 的台账锁，所以预期不会互相拖住。
func TestTabCloseConcurrent(t *testing.T) {
	b, shared := setup(t)
	srv := startServers(t)

	const n = 4
	tabs := make([]*chromium.Tab, 0, n)
	for i := 0; i < n; i++ {
		tb := newOwnTab(t, b, shared)
		gotoPage(t, tb, srv, "a")
		tabs = append(tabs, tb)
	}
	// 一个不参与关闭的旁观者，用来验证别人的关闭动作没把它带坏
	watcher := newOwnTab(t, b, shared)
	gotoPage(t, watcher, srv, "b")

	var wg sync.WaitGroup
	for _, tb := range tabs {
		wg.Add(1)
		go func(tb *chromium.Tab) {
			defer wg.Done()
			tb.Close()
		}(tb)
	}
	// 关闭进行中，旁观者照常读写
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_, _ = watcher.Eval(freshCtx(t, watcher, 10*time.Second), `1+1`)
		}
	}()
	wg.Wait()

	for i, tb := range tabs {
		if hasTab(t, b, shared, any(tb.ID)) {
			t.Errorf("第 %d 个标签页已关闭，但仍在 Tabs() 里", i+1)
		}
	}
	if got := mustTitle(t, watcher, 15*time.Second); got != "页面B" {
		t.Errorf("并发关闭期间旁观标签页被影响：标题 = %q", got)
	}
}
