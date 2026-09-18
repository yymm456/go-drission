//go:build smoke

package smoke

// 本文件覆盖异常路径：资源在对面消失之后，本库必须以「返回错误」收场，
// 而不是 panic、死锁或永久阻塞。
//
// 判据统一为三条：
//
//  1. **不 panic** —— 在 goroutine 里 recover，把 panic 转成一条错误报告，
//     否则 panic 会直接掀翻整个测试进程，其它用例的结果就读不到了；
//  2. **不永久阻塞** —— 每个调用都卡一个时间上限，超时即判失败；
//  3. **有错误** —— 该报错的时候必须报错，静默成功也算失败（会把调用方骗过去）。
//
// 为什么单独成文件：正常流程已经被 lifecycle_test.go 覆盖得很扎实，
// 而异常流程的写法与断言形态完全不同（需要超时包裹 + recover），
// 混在一起会让两边都变难读。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// abnormalTimeout 是异常路径下每个调用的等待上限。
//
// 取值要大于「正常的首次 CDP 往返」，又不能大到让用例卡死：
// Chrome 已经不在时，一个健全的实现应当在连接层立刻失败或走内置超时。
const abnormalTimeout = 30 * time.Second

// mustReturn 在独立 goroutine 里跑 f，断言它在 d 内返回。
//
// 返回它的 error；panic 被 recover 成一条错误（这样调用方能统一处理），
// 超时则返回 nil 并记一条错误。
//
// 注意：f 若真的永久阻塞，这个 goroutine 就泄漏了。这是有意的取舍 ——
// 用例此时已经失败，进程即将退出，为了「不泄漏」而引入额外的强杀机制
// 只会让这段测试逻辑比被测代码更难懂。
func mustReturn(t *testing.T, d time.Duration, what string, f func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- f()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Errorf("%s 在 %v 内没有返回（疑似永久阻塞）", what, d)
		return nil
	}
}

// mustError 断言 f 返回非 nil 错误，且在 d 内返回、不 panic。
func mustError(t *testing.T, d time.Duration, what string, f func() error) {
	t.Helper()
	err := mustReturn(t, d, what, f)
	if err == nil {
		t.Errorf("%s 期望返回错误，实际返回 nil", what)
	}
}

// TestAbnormalOpsAfterBrowserClose 浏览器关闭后，所有操作必须报错而不是 panic。
//
// 这条最容易坏在 teardown 路径上：Close 会把 rootCtx 置 nil，
// 而 chromedp.NewContext(nil) / chromedp.Run(nil, ...) 是会 panic 的，
// 所以库侧必须在入口处拦一道。
func TestAbnormalOpsAfterBrowserClose(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	b.Close()

	mustError(t, abnormalTimeout, "关闭后的 Tabs()", func() error {
		_, err := b.Tabs(context.Background())
		return err
	})
	mustError(t, abnormalTimeout, "关闭后的 NewTab()", func() error {
		_, err := b.NewTab(context.Background())
		return err
	})
	mustError(t, abnormalTimeout, "关闭后的 Context()", func() error {
		_, err := b.Context(context.Background(), "after-close")
		return err
	})
	mustError(t, abnormalTimeout, "关闭后的 tab.Eval()", func() error {
		ctx, cancel := context.WithTimeout(tab.Ctx, 10*time.Second)
		defer cancel()
		_, err := tab.Eval(ctx, "1+1")
		return err
	})

	// 重复关闭必须安全（defer 与显式 Close 撞在一起时必然发生）
	if err := mustReturn(t, abnormalTimeout, "重复 Close()", func() error {
		b.Close()
		return nil
	}); err != nil {
		t.Errorf("重复 Close() 出错：%v", err)
	}
}

// TestAbnormalOperateClosedTab 关掉标签页之后再操作它，必须报错而不是 panic。
func TestAbnormalOperateClosedTab(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	newTab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	b.CloseTab(ctx, newTab)

	// 关键点：ctx 必须从**被关掉的那个**标签页派生。
	// 若误用同浏览器里另一个标签页的 ctx，CDP 命令会路由到那个还活着的页面，
	// 于是「关闭后再操作」看起来成功了 —— 那是测试写错，不是库的行为。
	mustError(t, abnormalTimeout, "CloseTab 后的 Eval()", func() error {
		c, cancel := context.WithTimeout(newTab.Ctx, 10*time.Second)
		defer cancel()
		_, err := newTab.Eval(c, "1+1")
		return err
	})
	mustError(t, abnormalTimeout, "CloseTab 后的 Navigate()", func() error {
		c, cancel := context.WithTimeout(newTab.Ctx, 10*time.Second)
		defer cancel()
		return newTab.Navigate(c, "about:blank")
	})
}

// TestAbnormalWaitIdleAfterTabClosed 监听还在等的时候标签页被关掉，
// WaitIdle 必须返回而不是永久阻塞。
//
// WaitIdle 等的是「在途任务计数归零后关闭的 idle 通道」；若后台任务因为
// target 消失而永远不返回，计数就永远不归零，这里会一直卡住。
func TestAbnormalWaitIdleAfterTabClosed(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	newTab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	c, cancel := context.WithTimeout(newTab.Ctx, 10*time.Second)
	listener := newTab.Listen("")
	if err := listener.Start(c); err != nil {
		cancel()
		t.Fatalf("启动监听失败：%v", err)
	}
	cancel()

	b.CloseTab(ctx, newTab)

	if err := mustReturn(t, abnormalTimeout, "标签页关闭后的 WaitIdle()", func() error {
		listener.WaitIdle()
		return nil
	}); err != nil {
		t.Errorf("WaitIdle 出错：%v", err)
	}
	listener.Stop()
}

// TestAbnormalConcurrentCloseWhileNewTab 一边关浏览器、一边建标签页。
//
// 这是 teardown 与新建路径的真实交错：Close 会置 nil 并释放连接，
// NewTab 正在半路。断言不 panic、不死锁 —— 返回值是「成功」还是「已关闭」
// 取决于交错时序，两者都可接受，所以这里不断言具体错误。
func TestAbnormalConcurrentCloseWhileNewTab(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	parent := ctxOf(t, tab, 60*time.Second)

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		// 用独立 ctx，避免被父 ctx 的取消提前带走
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := b.NewTab(c)
		done <- err
	}()

	// 立刻关掉，制造交错
	b.Close()

	select {
	case err := <-done:
		// 成功或「已关闭」都算可接受；panic 已经在 goroutine 里转成了错误
		if err != nil {
			t.Logf("并发关闭期间 NewTab 返回：%v（可接受）", err)
		}
	case <-time.After(abnormalTimeout):
		t.Errorf("并发关闭期间的 NewTab 在 %v 内没有返回（疑似死锁）", abnormalTimeout)
	}
	_ = parent
}

// TestAbnormalCancelledContext 用一个已经取消的 ctx 发起操作，
// 必须立刻返回错误，而不是静默成功或卡住。
func TestAbnormalCancelledContext(t *testing.T) {
	_, tab := newIsolatedBrowser(t)

	cancelled, cancel := context.WithTimeout(tab.Ctx, 1*time.Nanosecond)
	defer cancel()
	<-cancelled.Done()

	mustError(t, abnormalTimeout, "已取消 ctx 的 Navigate()", func() error {
		return tab.Navigate(cancelled, "about:blank")
	})
	mustError(t, abnormalTimeout, "已取消 ctx 的 Eval()", func() error {
		_, err := tab.Eval(cancelled, "1+1")
		return err
	})
}

// TestAbnormalChromeKilledExternally Chrome 被外部杀掉之后：
// 操作必须在有限时间内失败，Close 也不能 panic。
//
// 这是真实运维场景里最常见的异常：用户在任务管理器里结束 Chrome、
// 或者 Chrome 自己崩了。库侧不能假设进程一定还活着。
func TestAbnormalChromeKilledExternally(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	pid := b.PID()
	if pid <= 0 {
		t.Skipf("PID()=%d：本实例接管的是已有 Chrome，不做外部杀进程", pid)
	}

	// 先确认真的可用，否则后面的失败可能只是「本来就没连上」
	if err := mustReturn(t, abnormalTimeout, "杀进程前的 Eval()", func() error {
		c, cancel := context.WithTimeout(tab.Ctx, 15*time.Second)
		defer cancel()
		_, err := tab.Eval(c, "1+1")
		return err
	}); err != nil {
		t.Fatalf("杀进程前就不可用，用例前提不成立：%v", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("找不到进程 %d：%v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("杀掉进程 %d 失败：%v", pid, err)
	}

	mustError(t, abnormalTimeout, "Chrome 被杀后的 Eval()", func() error {
		c, cancel := context.WithTimeout(tab.Ctx, 15*time.Second)
		defer cancel()
		_, err := tab.Eval(c, "1+1")
		return err
	})

	if err := mustReturn(t, abnormalTimeout, "Chrome 被杀后的 Close()", func() error {
		b.Close()
		return nil
	}); err != nil {
		t.Errorf("Chrome 被杀后 Close() 出错：%v", err)
	}
}

// TestAbnormalContextClosedThenUse 上下文关闭之后再取标签页，必须报错。
func TestAbnormalContextClosedThenUse(t *testing.T) {
	b, tab := newIsolatedBrowser(t)
	ctx := ctxOf(t, tab, 60*time.Second)

	bc, err := b.Context(ctx, "abnormal-ctx")
	if err != nil {
		t.Fatalf("创建隔离上下文失败：%v", err)
	}
	bc.Close()

	mustError(t, abnormalTimeout, "上下文关闭后的 NewTab()", func() error {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := bc.NewTab(c)
		return err
	})

	// 关掉之后 BrowserContext 注册表里不应再留着它
	if names := b.Contexts(); containsString(names, "abnormal-ctx") {
		t.Errorf("上下文已关闭，Contexts() 仍包含它：%v", names)
	}
}

// containsString 判断字符串切片是否包含某项（避免为一个断言引入 slices 依赖差异）。
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// 断言本文件用到的 chromium 包仍被引用（编译期保证门面 API 没被改没）。
var _ = chromium.ErrClosed
