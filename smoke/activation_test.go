//go:build smoke

package smoke

// Activate 与 SetWindowState 的行为验证。
//
// 这两个都依赖「窗口」概念，但实测在 headless 下也能调用（Chrome 会给 headless 实例
// 一个虚拟窗口：WindowID 返回非 0，Activate / SetWindowState 都不报错）。所以这里用
// smoke 默认的 headless 实例即可，不必起带窗口的浏览器 —— 那会让 CI 变得不稳定。
//
// 因此本文件守的是「调用契约」；而 visibilityState / hasFocus 这类只在真实窗口上
// 才看得到的效果，不在 smoke 里断言（它们的实测数据写在方法注释与 README 里）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func TestTabActivate(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	// Activate 内部要先拿 windowID（两者都是 browser 级命令）
	wid, err := tab.WindowID(freshCtx(t, tab, 20*time.Second))
	if err != nil {
		t.Fatalf("WindowID 失败：%v", err)
	}
	if wid == 0 {
		t.Errorf("WindowID 应非 0（headless 也有虚拟窗口）")
	}

	if err := tab.Activate(freshCtx(t, tab, 20*time.Second)); err != nil {
		t.Errorf("Activate 失败：%v", err)
	}
}

// TestSetWindowState 窗口状态切换。
//
// 顺序很关键：Chrome **不允许**从 minimized 直接切到 maximized / fullscreen，
// 实测报错 "To maximize a minimized or fullscreen window, restore it to normal
// state first."。用例按 normal 打底的顺序走，并在最后恢复 normal ——
// 免得把浏览器的窗口状态改坏，影响后面用同一实例的用例。
func TestSetWindowState(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	t.Cleanup(func() {
		_ = tab.SetWindowState(freshCtx(t, tab, 20*time.Second), "normal")
	})

	seq := []string{
		"normal",
		"minimized",
		"normal", // 必须先恢复，否则下一个 maximized 会被 Chrome 拒
		"maximized",
		"normal",
		"fullscreen",
		"normal",
	}
	for _, s := range seq {
		if err := tab.SetWindowState(freshCtx(t, tab, 20*time.Second), s); err != nil {
			t.Errorf("SetWindowState(%q) 失败：%v", s, err)
		}
	}
}

// TestSetWindowStateInvalid 非法值必须在发起 CDP 之前就被拦成哨兵错误。
//
// 不拦的话 Chrome 只回一句 "Invalid window bounds"，看不出是哪个值错了
// （与 Cookie 校验同一个道理）。
func TestSetWindowStateInvalid(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	for _, bad := range []string{"", "bogus", "NORMAL"} {
		err := tab.SetWindowState(freshCtx(t, tab, 20*time.Second), bad)
		if !errors.Is(err, chromium.ErrInvalidWindowState) {
			t.Errorf("SetWindowState(%q) 应报 ErrInvalidWindowState，实际：%v", bad, err)
		}
	}
}

// TestOpenPageWithWindowState 启动选项：窗口状态在 OpenPage 返回前就应用好，
// 不必调用方拿到 tab 后再自己调 SetWindowState。
//
// headless 下 Chrome 会给实例一个虚拟窗口，setWindowBounds 依然返回成功，
// 所以这里用 smoke 默认的 headless 实例即可（与 Tab.SetWindowState 的用例同理）。
func TestOpenPageWithWindowState(t *testing.T) {
	for _, s := range []string{"maximized", "minimized", "normal"} {
		b, tab, err := chromium.OpenPage(context.Background(), nextFreePort(),
			chromium.WithUserDataDir(t.TempDir()),
			chromium.WithHeadless(true),
			chromium.WithWindowState(s),
			chromium.WithConnectTimeout(30*time.Second),
			chromium.WithFlag("no-proxy-server", ""),
		)
		if err != nil {
			if errors.Is(err, chromium.ErrChromeNotFound) {
				t.Skipf("本机没有可用浏览器，跳过：%v", err)
			}
			t.Fatalf("WithWindowState(%q) 打开浏览器失败：%v", s, err)
		}
		if tab == nil {
			t.Errorf("WithWindowState(%q) 返回的标签页为 nil", s)
		}
		b.Close()
	}
}

// TestOpenPageWithInvalidWindowState 取值非法必须在 OpenPage 阶段就报错，
// 而不是静默忽略 —— 否则调用方会以为设了却没生效。
func TestOpenPageWithInvalidWindowState(t *testing.T) {
	b, _, err := chromium.OpenPage(context.Background(), nextFreePort(),
		chromium.WithUserDataDir(t.TempDir()),
		chromium.WithHeadless(true),
		chromium.WithWindowState("bogus"),
		chromium.WithConnectTimeout(30*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	if !errors.Is(err, chromium.ErrInvalidWindowState) {
		t.Errorf("非法窗口状态应报 ErrInvalidWindowState，实际：%v", err)
	}
	if b != nil {
		b.Close()
	}
}
