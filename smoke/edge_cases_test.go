//go:build smoke

package smoke

// 本文件补三类「资源在对面消失/被占用」的异常场景：
//
//   - Element：查询成功之后，DOM 里的节点被删掉
//   - Frame：查找不到 / 上下文已过期 / 框架被页面移除
//   - Profile：目录被别的实例占着 / Chrome 被强杀后能否恢复
//
// 每个用例回答的是同一组问题（不是凑数量）：
//
//	会不会 panic？死锁？永久等待？错误被吞掉？
//	状态会不会坏掉？后续对象还能不能用？能不能重新拿到？
//
// 重要：全部按**当前实现与 API 的实际语义**断言，不假设"应该"怎样。
// 首次跑起来的观察值用 Logf 记下来；如果某条与预期不符，
// 那是需要讨论的行为差异，不是改测试去迎合实现。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
	"github.com/yymm456/go-drission/chromium/chrome"
)

// edgeTimeout 是异常路径下每个调用的等待上限。
const edgeTimeout = 30 * time.Second

// freshCtx 派生一个**独立的**短超时 ctx。
//
// 异常路径的用例里，每个操作都必须各自持有一个：这些操作在目标不存在时
// 会「等待它出现」，第一个就会把 ctx 的 deadline 耗光；后面几个拿到的
// 都是「ctx 已过期」，于是会把「其实正常的对象」误判成也坏了。
// 这个坑在本文件里已经踩到两次（Element.Count 与 Frame 查找后判定标签页可用）。
func freshCtx(t *testing.T, tab *chromium.Tab, d time.Duration) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(tab.Ctx, d)
	t.Cleanup(cancel)
	return c
}

// mustReturnEdge 在独立 goroutine 里跑 f，断言它在 d 内返回。
// panic 会被 recover 成一条错误（否则会掀翻整个测试进程）。
func mustReturnEdge(t *testing.T, d time.Duration, what string, f func() error) error {
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

// ---------------------------------------------------------------- Element 消失

// TestElementRemovedFromDOM 覆盖「查到了元素，但它在操作前被 DOM 删掉」。
//
// 这是 SPA 里最常见的竞态：列表重渲染、路由切换都会让刚查到的节点消失。
// 本用例关注的不是"返回什么错误"，而是：
//   - 操作必须返回（不能卡住）；
//   - 不能 panic；
//   - 标签页不能被这次失败弄成废状态；
//   - 之后重新查询必须能拿到正确结果（元素确实不在了）。
func TestElementRemovedFromDOM(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, edgeTimeout)

	// 1) 前置：元素确实在
	el := tab.EleCSS("#title")
	if n, err := el.Count(ctx); err != nil || n != 1 {
		t.Fatalf("前置条件不成立：#title count=%d err=%v", n, err)
	}

	// 2) 把它从 DOM 里删掉
	if _, err := tab.Eval(ctx, `document.querySelector('#title').remove()`); err != nil {
		t.Fatalf("删除元素失败：%v", err)
	}

	// 每个操作都必须用**独立的**短超时 ctx。
	//
	// 不能五个操作共用一个 ctx：这些操作在元素不存在时会「等待元素出现」，
	// 第一个就会把 ctx 的 deadline 耗光，后面几个拿到的都是「ctx 已过期」，
	// 于是会误判成「Count 也会超时」——实际上是测试自己的问题。
	fresh := func(d time.Duration, f func(context.Context) error) error {
		c, cancel := context.WithTimeout(tab.Ctx, d)
		defer cancel()
		return f(c)
	}
	const opBudget = 6 * time.Second

	// 3) 用**之前拿到的句柄**继续操作：记录实际返回，不假设应该报错
	if err := mustReturnEdge(t, edgeTimeout, "元素消失后的 Click", func() error {
		return fresh(opBudget, func(c context.Context) error { return el.Click(c) })
	}); err != nil {
		t.Logf("元素消失后 Click 返回：%v", err)
	} else {
		t.Logf("元素消失后 Click 返回 nil（记录实际语义）")
	}

	if err := mustReturnEdge(t, edgeTimeout, "元素消失后的 Text", func() error {
		return fresh(opBudget, func(c context.Context) error { _, err := el.Text(c); return err })
	}); err != nil {
		t.Logf("元素消失后 Text 返回：%v", err)
	}

	if err := mustReturnEdge(t, edgeTimeout, "元素消失后的 Attribute", func() error {
		return fresh(opBudget, func(c context.Context) error { _, err := el.Attribute(c, "id"); return err })
	}); err != nil {
		t.Logf("元素消失后 Attribute 返回：%v", err)
	}

	if err := mustReturnEdge(t, edgeTimeout, "元素消失后的 SendKeys", func() error {
		return fresh(opBudget, func(c context.Context) error { return el.SendKeys(c, "x") })
	}); err != nil {
		t.Logf("元素消失后 SendKeys 返回：%v", err)
	}

	// 4) 计数是纯 JS 求值，不等待：应当立刻如实反映「已经不在了」
	// （Count 的文档承诺：0 是正常返回值，不是错误）
	var n int
	if err := mustReturnEdge(t, edgeTimeout, "元素消失后的 Count", func() error {
		return fresh(opBudget, func(c context.Context) error {
			var err error
			n, err = el.Count(c)
			return err
		})
	}); err != nil {
		t.Errorf("元素消失后 Count 报错：%v（Count 应返回 0 而不是错误）", err)
	} else if n != 0 {
		t.Errorf("元素已删除，Count = %d，期望 0", n)
	}

	// 5) 标签页不能被带坏：还能求值、还能导航
	if _, err := tab.Eval(ctx, `1+1`); err != nil {
		t.Fatalf("元素消失后标签页不可用：%v", err)
	}
	if err := tab.Navigate(ctx, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("元素消失后无法再次导航：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("元素消失后等待就绪失败：%v", err)
	}

	// 6) 重新加载后元素应当能重新查到（句柄不缓存节点，所以天然可恢复）
	if n, err := tab.EleCSS("#title").Count(ctx); err != nil || n != 1 {
		t.Fatalf("重新加载后 #title count=%d err=%v，期望 1", n, err)
	}
}

// TestElementRemovedDuringWait 覆盖「等待过程中元素消失」：
// 先发起一个等待元素文本出现的调用，同时在另一个 goroutine 里把节点删掉，
// 断言等待会按 ctx 收尾而不是一直挂着。
func TestElementRemovedDuringWait(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, edgeTimeout)

	// 让 #title 的文本先不等于目标值，这样等待会真的进入轮询
	if _, err := tab.Eval(ctx, `document.querySelector('#title').textContent = 'waiting'`); err != nil {
		t.Fatalf("改写文本失败：%v", err)
	}

	waitCtx, cancelWait := context.WithTimeout(tab.Ctx, 5*time.Second)
	defer cancelWait()

	// 后台：等一会儿再把元素删掉，制造「等待过程中消失」
	go func() {
		time.Sleep(1 * time.Second)
		opCtx, opCancel := context.WithTimeout(tab.Ctx, 10*time.Second)
		defer opCancel()
		_, _ = tab.Eval(opCtx, `document.querySelector('#title').remove()`)
	}()

	start := time.Now()
	err := tab.EleCSS("#title").WaitText(waitCtx, "绝不会出现的文本")
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("等待一个永不出现的文本应当返回错误，实际返回 nil")
	}
	if elapsed > 15*time.Second {
		t.Errorf("等待耗时 %v，远超 5s 的 deadline —— 没有按 ctx 收尾", elapsed)
	}
	t.Logf("元素消失场景下的 WaitText 在 %v 后返回：%v", elapsed, err)

	// 标签页仍然可用
	if _, err := tab.Eval(ctxOf(t, tab, edgeTimeout), `1+1`); err != nil {
		t.Fatalf("等待结束后标签页不可用：%v", err)
	}
}

// ---------------------------------------------------------------- Frame 异常

// TestFrameNotFound 查找不存在的框架：必须报错、快速返回，不能挂住也不能 panic。
func TestFrameNotFound(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	for name, fn := range map[string]func(context.Context) error{
		"FrameByURL(不存在)": func(c context.Context) error {
			_, err := tab.FrameByURL(c, "/no-such-frame")
			return err
		},
		"FrameByName(不存在)": func(c context.Context) error {
			_, err := tab.FrameByName(c, "no-such-name")
			return err
		},
		"Frame(CSS 不存在)": func(c context.Context) error {
			_, err := tab.Frame(c, chromium.CSS("iframe#nope"))
			return err
		},
	} {
		// 每个查找用独立 ctx：按选择器查找在找不到时会等待，
		// 共用 ctx 会让后一个查找拿到「已过期」的结果。
		c := freshCtx(t, tab, 8*time.Second)
		start := time.Now()
		err := mustReturnEdge(t, edgeTimeout, name, func() error { return fn(c) })
		elapsed := time.Since(start)

		if err == nil {
			t.Errorf("%s 期望返回错误，实际返回 nil", name)
		} else {
			t.Logf("%s 返回：%v（耗时 %v）", name, err, elapsed)
		}
		if elapsed > edgeTimeout {
			t.Errorf("%s 耗时 %v 超过上限", name, elapsed)
		}
	}

	// 查找失败不能把标签页弄坏（同样用独立 ctx）
	if _, err := tab.Eval(freshCtx(t, tab, 15*time.Second), `1+1`); err != nil {
		t.Fatalf("多次查找框架失败后标签页不可用：%v", err)
	}
}

// TestFrameLookupWithExpiredContext 用一个已经过期的 ctx 查框架，
// 断言立刻返回错误，而不是忽略 ctx 继续干。
func TestFrameLookupWithExpiredContext(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	expired, cancel := context.WithTimeout(tab.Ctx, 1*time.Nanosecond)
	defer cancel()
	<-expired.Done()

	err := mustReturnEdge(t, edgeTimeout, "已过期 ctx 的 Frames()", func() error {
		_, err := tab.Frames(expired)
		return err
	})
	if err == nil {
		t.Errorf("用已过期的 ctx 查 Frames 期望返回错误，实际返回 nil")
	} else {
		t.Logf("已过期 ctx 查 Frames 返回：%v", err)
	}

	err = mustReturnEdge(t, edgeTimeout, "已过期 ctx 的 FrameByURL()", func() error {
		_, err := tab.FrameByURL(expired, "frame.html")
		return err
	})
	if err == nil {
		t.Errorf("用已过期的 ctx 查 FrameByURL 期望返回错误，实际返回 nil")
	}
}

// TestFrameRemovedFromPage 覆盖「框架被页面移除之后继续使用」。
//
// 关注 Frame / FrameElement / Tab 三者的生命周期关系：
// 框架没了，Frame 句柄还能不能安全操作？会不会 panic？会不会卡住？
// 页面主体还能不能用？能不能重新找到框架？
func TestFrameRemovedFromPage(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, edgeTimeout)

	// 1) 先定位到同域框架
	f, err := tab.Frame(ctx, chromium.CSS("iframe#f1"))
	if err != nil {
		t.Fatalf("定位同域框架失败：%v", err)
	}
	if _, err := f.Eval(ctx, `1+1`); err != nil {
		t.Fatalf("框架内求值失败（前置条件不成立）：%v", err)
	}

	// 2) 把 iframe 从页面里移除（模拟框架被销毁）
	if _, err := tab.Eval(ctx, `document.querySelector('iframe#f1').remove()`); err != nil {
		t.Fatalf("移除 iframe 失败：%v", err)
	}

	// 3) 继续用原来的 Frame 句柄：记录实际行为（每个操作独立 ctx）
	if err := mustReturnEdge(t, edgeTimeout, "框架被移除后的 Eval", func() error {
		_, err := f.Eval(freshCtx(t, tab, 10*time.Second), `1+1`)
		return err
	}); err != nil {
		t.Logf("框架被移除后 Eval 返回：%v", err)
	} else {
		t.Logf("框架被移除后 Eval 仍然返回 nil（记录实际语义）")
	}

	if err := mustReturnEdge(t, edgeTimeout, "框架被移除后的 URLNow", func() error {
		_, err := f.URLNow(freshCtx(t, tab, 10*time.Second))
		return err
	}); err != nil {
		t.Logf("框架被移除后 URLNow 返回：%v", err)
	}

	// 4) 页面主体必须仍然可用
	if _, err := tab.Eval(freshCtx(t, tab, 15*time.Second), `1+1`); err != nil {
		t.Fatalf("框架被移除后页面主体不可用：%v", err)
	}
	if n, err := tab.EleCSS("#title").Count(freshCtx(t, tab, 15*time.Second)); err != nil || n != 1 {
		t.Fatalf("框架被移除后主页面元素异常：count=%d err=%v", n, err)
	}

	// 5) 重新加载后可以重新找到框架（框架列表应恢复）
	reload := freshCtx(t, tab, 20*time.Second)
	if err := tab.Navigate(reload, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("框架被移除后重新导航失败：%v", err)
	}
	if err := tab.WaitReady(reload); err != nil {
		t.Fatalf("重新导航后等待就绪失败：%v", err)
	}
	if _, err := tab.Frame(reload, chromium.CSS("iframe#f1")); err != nil {
		t.Fatalf("重新加载后无法再次定位框架：%v", err)
	}
}

// ---------------------------------------------------------------- Profile 异常

// TestProfileDirLockedByOtherProcess 模拟「档案目录已被别的实例占用」。
//
// 用 chrome.AcquireProfileLock 先占住目录，再让 ProfileManager 打开同名档案。
// 关注点：能不能检测到冲突、会不会永久阻塞、返回什么。
func TestProfileDirLockedByOtherProcess(t *testing.T) {
	base := tempProfileDir(t)
	target := filepath.Join(base, "occupied")

	// 模拟另一个进程/实例已经持有该目录的排他锁
	lock, err := chrome.AcquireProfileLock(target)
	if err != nil {
		t.Fatalf("预先占用档案目录失败：%v", err)
	}
	t.Cleanup(lock.Release)

	pm := chromium.NewProfileManager(base, nextProfileBasePort(),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	t.Cleanup(pm.CloseAll)

	openCtx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	start := time.Now()
	b, _, err := pm.Open(openCtx, "occupied")
	elapsed := time.Since(start)

	t.Logf("目录被占用时 Open 的结果：err=%v 耗时=%v", err, elapsed)

	// 不能永久阻塞：必须在调用方 ctx 之内给出结果
	if elapsed > 35*time.Second {
		t.Errorf("Open 耗时 %v，几乎耗尽 40s 的调用方 ctx —— 疑似永久阻塞", elapsed)
	}

	// 若打开成功，说明"锁冲突"没有拦住它：记录下来讨论，但要能正常收尾
	if err == nil {
		t.Logf("注意：目录被占用时 Open 仍然成功了（记录实际语义，供后续讨论）")
		if b != nil {
			b.Close()
		}
	} else if errors.Is(err, chromium.ErrChromeNotFound) {
		t.Skipf("本机没有可用浏览器，跳过：%v", err)
	}

	// 关键：无论成败，管理器本身不能坏 —— 其它档案仍应可用
	otherCtx, otherCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer otherCancel()
	if _, _, err := pm.Open(otherCtx, "unaffected"); err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Errorf("一个档案冲突后，另一个档案也打不开了（管理器状态被带坏）：%v", err)
	}
}

// TestProfileRecoversAfterChromeKilled 模拟「Chrome 异常退出」之后同一个档案能否恢复。
//
// 流程：打开档案 → 强杀它的 Chrome 进程（模拟崩溃/被任务管理器结束）→
// 关闭档案 → 用同名再开一次。
// 关注点：锁不会永久残留、后续能重新使用、不影响其它档案。
func TestProfileRecoversAfterChromeKilled(t *testing.T) {
	pm := chromium.NewProfileManager(tempProfileDir(t), nextProfileBasePort(),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	t.Cleanup(pm.CloseAll)

	const name = "crashed"
	openCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, _, err := pm.Open(openCtx, name)
	if err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("首次打开档案失败：%v", err)
	}

	// 强杀 Chrome：模拟进程异常退出（不是走正常 Close 路径）
	pid := b.PID()
	if pid > 0 {
		proc, ferr := os.FindProcess(pid)
		if ferr == nil {
			if kerr := proc.Kill(); kerr != nil {
				t.Logf("强杀进程 %d 失败（继续验证恢复能力）：%v", pid, kerr)
			}
		}
	} else {
		t.Logf("PID()=%d，跳过强杀步骤（接管已有 Chrome 的情况）", pid)
	}

	// 关闭档案（此时底层 Chrome 已死，Close 必须安全）
	pm.Close(name)

	// 同名重新打开：锁必须已经释放，不能被永久占用
	reCtx, reCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer reCancel()

	start := time.Now()
	b2, _, err := pm.Open(reCtx, name)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chrome 被强杀后同名档案无法重新打开（锁可能残留）：%v", err)
	}
	if elapsed > 45*time.Second {
		t.Errorf("重新打开耗时 %v，疑似被残留锁阻塞", elapsed)
	}
	t.Logf("强杀 Chrome 后同名档案在 %v 内恢复", elapsed)

	// 恢复出来的实例必须真的可用
	if b2 != nil {
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer verifyCancel()
		tabs, err := b2.Tabs(verifyCtx)
		if err != nil {
			t.Errorf("恢复后的档案浏览器不可用：%v", err)
		} else {
			t.Logf("恢复后的档案可用，标签页数=%d", len(tabs))
		}
	}

	// 其它档案不受影响
	if _, _, err := pm.Open(reCtx, "still-fine"); err != nil {
		t.Errorf("一个档案崩溃后，另一个档案也打不开了：%v", err)
	}
}

var _ = chromium.ErrClosed
