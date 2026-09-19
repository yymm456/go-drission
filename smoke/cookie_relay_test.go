//go:build smoke

// 本文件覆盖两类容易被忽略的行为：
//  1. Session 与浏览器之间的 Cookie 接力（纯 HTTP 登录 → 浏览器免登录）；
//  2. 之前用真实浏览器复现过、已修复的回归点（静默成功、失败重试的重复副作用、
//     WaitGroup 与 Stop 的竞态）。
package smoke

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
	"github.com/yymm456/go-drission/session"
)

// relayBrowser 为每个用例起一个独立浏览器，避免与共享实例的标签页互相干扰。
func relayBrowser(t *testing.T) *chromium.Browser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	// 显式指定用户数据目录，交给 testing 在用例结束时删除（原因见 smoke_test.go 的
	// sharedUserDataDir 注释）。这里先调 t.TempDir() 再注册 t.Cleanup(b.Close)：
	// t.Cleanup 是后进先出，于是先关浏览器、后删目录，不会「占着目录删不掉」。
	dir := t.TempDir()
	b, _, err := chromium.OpenPage(ctx, nextFreePort(),
		chromium.WithUserDataDir(dir),
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
	return b
}

// loginSite 是一个带会话的最小站点：
//
//	POST /login  校验表单，下发 sid Cookie（纯 HTTP 就能完成）
//	GET  /home   把收到的 Cookie 回显在页面上，用于判断是否「已登录」
func loginSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("user") != "alice" || r.PostFormValue("pass") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "bad credentials")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "token-alice", Path: "/", HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "role", Value: "admin", Path: "/"})
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<!doctype html><html><body><div id='who'>%s</div></body></html>",
			r.Header.Get("Cookie"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestLoginWithCookiesRelay 是本次改动的核心用例：
// 用 session 走纯 HTTP 登录拿到会话 Cookie，再直接交给浏览器实现免登录——
// 浏览器全程没见过登录表单，打开 /home 时就已经是登录态。
func TestLoginWithCookiesRelay(t *testing.T) {
	srv := loginSite(t)
	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) 纯 HTTP 登录
	sess := session.New(session.WithTimeout(10 * time.Second))
	resp, err := sess.PostForm(ctx, srv.URL+"/login", map[string][]string{
		"user": {"alice"}, "pass": {"secret"},
	})
	if err != nil {
		t.Fatalf("HTTP 登录失败：%v", err)
	}
	if !resp.OK() {
		t.Fatalf("HTTP 登录状态码异常：%d", resp.StatusCode)
	}
	if len(sess.Cookies()) == 0 {
		t.Fatal("HTTP 登录后没有拿到 Cookie")
	}

	// 2) Cookie 交给浏览器（默认上下文）
	tab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	if err := tab.LoginWithCookies(ctx, srv.URL+"/home", sess.Jar()); err != nil {
		t.Fatalf("免登录失败：%v", err)
	}
	who, err := tab.EleCSS("#who").Text(ctx)
	if err != nil {
		t.Fatalf("读取页面失败：%v", err)
	}
	if !strings.Contains(who, "sid=token-alice") || !strings.Contains(who, "role=admin") {
		t.Fatalf("浏览器未带上会话 Cookie，页面显示：%q", who)
	}
	t.Logf("默认上下文免登录成功：%s", who)

	// 3) 隔离上下文同样生效，且与默认上下文互不影响
	bc, err := b.Context(ctx, "relayAccount")
	if err != nil {
		t.Fatalf("创建隔离上下文失败：%v", err)
	}
	tab2, err := bc.NewTab(ctx)
	if err != nil {
		t.Fatalf("隔离上下文新建标签页失败：%v", err)
	}
	if err := tab2.LoginWithCookiesJSON(ctx, srv.URL+"/home", mustJSON(t, sess)); err != nil {
		t.Fatalf("隔离上下文免登录失败：%v", err)
	}
	who2, err := tab2.EleCSS("#who").Text(ctx)
	if err != nil {
		t.Fatalf("隔离上下文读取页面失败：%v", err)
	}
	if !strings.Contains(who2, "sid=token-alice") {
		t.Fatalf("隔离上下文未带上会话 Cookie：%q", who2)
	}
	t.Logf("隔离上下文免登录成功：%s", who2)

	// 4) 按目标 URL 过滤导出，不会把无关域的 Cookie 一起带过去
	items, err := sess.CookiesFor(srv.URL + "/home")
	if err != nil {
		t.Fatalf("CookiesFor 失败：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("期望导出 2 条本站 Cookie，实际 %d 条", len(items))
	}
}

func mustJSON(t *testing.T, s *session.Session) []byte {
	t.Helper()
	data, err := s.Jar().ExportJSON()
	if err != nil {
		t.Fatalf("导出 Cookie JSON 失败：%v", err)
	}
	return data
}

// TestLoginWithCookiesRejectsBadInput 免登录接口的入参校验。
func TestLoginWithCookiesRejectsBadInput(t *testing.T) {
	srv := loginSite(t)
	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	tab, _ := b.NewTab(ctx)
	empty := session.New()

	if err := tab.LoginWithCookies(ctx, "/relative", empty.Jar()); !errors.Is(err, chromium.ErrEmptyURL) {
		t.Errorf("相对地址期望 ErrEmptyURL，实际 %v", err)
	}
	data, _ := empty.Jar().ExportJSON()
	if err := tab.LoginWithCookiesJSON(ctx, srv.URL+"/home", data); !errors.Is(err, chromium.ErrInvalidCookie) {
		t.Errorf("空 Cookie 期望 ErrInvalidCookie，实际 %v", err)
	}
	if _, err := tab.ImportCookiesJSON(ctx, []byte("{不是数组}")); !errors.Is(err, chromium.ErrInvalidCookieJSON) {
		t.Errorf("非法 JSON 期望 ErrInvalidCookieJSON，实际 %v", err)
	}
	if err := tab.SetCookies(ctx, []chromium.Cookie{{Name: "nodomain", Value: "1"}}); !errors.Is(err, chromium.ErrInvalidCookie) {
		t.Errorf("缺 domain 期望 ErrInvalidCookie，实际 %v", err)
	}
	if err := tab.SetCookie(ctx, chromium.Cookie{Value: "noname"}); !errors.Is(err, chromium.ErrInvalidCookie) {
		t.Errorf("缺 name 期望 ErrInvalidCookie，实际 %v", err)
	}
}

// TestIsolatedContextCookieScope 隔离上下文的 Cookie 必须真正隔离，
// 且不会漏进默认上下文——这是多账号共用一个浏览器进程的前提。
func TestIsolatedContextCookieScope(t *testing.T) {
	srv := loginSite(t)
	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	host := strings.TrimPrefix(srv.URL, "http://")

	bcA, err := b.Context(ctx, "scopeA")
	if err != nil {
		t.Fatalf("ctxA: %v", err)
	}
	tabA, err := bcA.NewTab(ctx)
	if err != nil {
		t.Fatalf("tabA: %v", err)
	}
	if err := tabA.Navigate(ctx, srv.URL+"/home"); err != nil {
		t.Fatalf("navA: %v", err)
	}
	if err := tabA.SetCookies(ctx, []chromium.Cookie{{Name: "probe", Value: "fromA", Domain: host, Path: "/"}}); err != nil {
		t.Fatalf("A 注入失败：%v", err)
	}

	bcB, err := b.Context(ctx, "scopeB")
	if err != nil {
		t.Fatalf("ctxB: %v", err)
	}
	tabB, err := bcB.NewTab(ctx)
	if err != nil {
		t.Fatalf("tabB: %v", err)
	}
	if err := tabB.Navigate(ctx, srv.URL+"/home"); err != nil {
		t.Fatalf("navB: %v", err)
	}
	whoB, _ := tabB.EleCSS("#who").Text(ctx)
	if strings.Contains(whoB, "probe=fromA") {
		t.Fatalf("隔离上下文 B 看到了 A 的 Cookie：%q", whoB)
	}

	defTab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("默认上下文新建失败：%v", err)
	}
	if err := defTab.Navigate(ctx, srv.URL+"/home"); err != nil {
		t.Fatalf("默认上下文导航失败：%v", err)
	}
	whoDef, _ := defTab.EleCSS("#who").Text(ctx)
	if strings.Contains(whoDef, "probe=fromA") {
		t.Fatalf("默认上下文看到了隔离上下文 A 的 Cookie：%q", whoDef)
	}
}

// TestCookieFileRoundTrip 浏览器导出的 Cookie 文件能被 session 读入，反之亦然。
func TestCookieFileRoundTrip(t *testing.T) {
	srv := loginSite(t)
	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	tab, _ := b.NewTab(ctx)
	if err := tab.Navigate(ctx, srv.URL+"/home"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.SetCookies(ctx, []chromium.Cookie{
		{Name: "a", Value: "1", Domain: strings.TrimPrefix(srv.URL, "http://"), Path: "/"},
		{Name: "b", Value: "2", Domain: strings.TrimPrefix(srv.URL, "http://"), Path: "/"},
	}); err != nil {
		t.Fatalf("注入 Cookie 失败：%v", err)
	}

	// 父目录不存在时应当自动创建（旧实现直接报 path not found）
	dir := filepath.Join(t.TempDir(), "deep", "nested")
	file := filepath.Join(dir, "cookies.json")
	if err := tab.ExportCookies(ctx, file, srv.URL); err != nil {
		t.Fatalf("ExportCookies 失败：%v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("导出文件不存在：%v", err)
	}

	// 浏览器 → session
	sess := session.New()
	if err := sess.LoadCookies(file); err != nil {
		t.Fatalf("session 载入浏览器 Cookie 失败：%v", err)
	}
	if len(sess.Cookies()) != 2 {
		t.Fatalf("session 期望载入 2 条 Cookie，实际 %d", len(sess.Cookies()))
	}

	// session → 浏览器（新标签页导入文件）
	tab2, _ := b.NewTab(ctx)
	if err := tab2.ImportCookies(ctx, file); err != nil {
		t.Fatalf("ImportCookies 失败：%v", err)
	}
	cookies, err := tab2.Cookies(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Cookies 失败：%v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("往返后期望 2 条 Cookie，实际 %d", len(cookies))
	}
}

// TestFrameMissingElementReturnsError 守住「静默成功」这个坑：
// iframe 内元素不存在时必须报 ErrElementNotFound，而不是装作点过了。
func TestFrameMissingElementReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inner" {
			fmt.Fprint(w, `<!doctype html><html><body><button id="b">x</button><span id="cnt">0</span></body></html>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><html><body><iframe id="fr" src="/inner"></iframe></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tab, _ := b.NewTab(ctx)
	if err := tab.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.EleCSS("#fr").Wait().Present().Timeout(10 * time.Second).Do(ctx); err != nil {
		t.Fatalf("等待 iframe 失败：%v", err)
	}
	f, err := tab.Frame(ctx, chromium.CSS("#fr"))
	if err != nil {
		t.Fatalf("定位 iframe 失败：%v", err)
	}

	if err := f.EleCSS("#missing").Click(ctx); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Errorf("FrameElement.Click 期望 ErrElementNotFound，实际 %v", err)
	}
	if err := f.EleCSS("#missing").SetValue(ctx, "v"); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Errorf("FrameElement.SetValue 期望 ErrElementNotFound，实际 %v", err)
	}
	if _, err := f.EleCSS("#missing").Text(ctx); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Errorf("FrameElement.Text 期望 ErrElementNotFound，实际 %v", err)
	}
	if n, err := f.EleCSS("#missing").Count(ctx); err != nil || n != 0 {
		t.Errorf("FrameElement.Count 期望 0/nil，实际 %d/%v", n, err)
	}

	// 元素存在时仍然正常工作
	if err := f.EleCSS("#b").Click(ctx); err != nil {
		t.Errorf("FrameElement.Click(存在元素) 不应报错：%v", err)
	}
	if n, err := f.EleCSS("#b").Count(ctx); err != nil || n != 1 {
		t.Errorf("FrameElement.Count 期望 1/nil，实际 %d/%v", n, err)
	}
}

// TestFrameEvalExceptionNotRetried 守住「失败后重建 world 导致副作用重复执行」这个坑：
// 脚本自身抛异常时绝不能重试，否则点击/提交会被做两遍。
func TestFrameEvalExceptionNotRetried(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inner" {
			fmt.Fprint(w, `<!doctype html><html><body><span id="cnt">0</span></body></html>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><html><body><iframe id="fr" src="/inner"></iframe></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tab, _ := b.NewTab(ctx)
	if err := tab.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.EleCSS("#fr").Wait().Present().Timeout(10 * time.Second).Do(ctx); err != nil {
		t.Fatalf("等待 iframe 失败：%v", err)
	}
	f, err := tab.Frame(ctx, chromium.CSS("#fr"))
	if err != nil {
		t.Fatalf("定位 iframe 失败：%v", err)
	}

	if _, err := f.Eval(ctx, `document.getElementById('cnt').textContent='0'`); err != nil {
		t.Fatalf("重置计数失败：%v", err)
	}
	if _, err := f.Eval(ctx, `(function(){ var d=document.getElementById('cnt');
		d.textContent=String((parseInt(d.textContent)||0)+1); throw new Error('boom'); })()`); err == nil {
		t.Fatal("抛异常的脚本应当返回错误")
	}
	got, err := f.Eval(ctx, `document.getElementById('cnt').textContent`)
	if err != nil {
		t.Fatalf("读取计数失败：%v", err)
	}
	if got != "1" {
		t.Fatalf("脚本副作用应只执行 1 次，实际 %v 次（重试把副作用做重了）", got)
	}

	// world 失效（页面重新导航）时必须能自愈重试
	if err := tab.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("二次导航失败：%v", err)
	}
	if err := tab.EleCSS("#fr").Wait().Present().Timeout(10 * time.Second).Do(ctx); err != nil {
		t.Fatalf("等待 iframe 失败：%v", err)
	}
	if _, err := f.Eval(ctx, `1+1`); err != nil {
		t.Fatalf("导航后应当能重建 world 并继续求值：%v", err)
	}
}

// TestListenerStartStopUnderLoad 守住 WaitGroup 与 Stop 的竞态：
// 事件是异步派发的，取消 ctx 之后仍可能有最后一个事件在路上，
// 若在 Wait 期间执行 Add，会触发 "WaitGroup misuse: Add called concurrently with Wait"。
// 该用例建议配合 -race 运行。
func TestListenerStartStopUnderLoad(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><img src="/a.png"><img src="/b.png"></body></html>`)
	})
	mux.HandleFunc("/a.png", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "a") })
	mux.HandleFunc("/b.png", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "b") })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tab, _ := b.NewTab(ctx)
	if err := tab.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("导航失败：%v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 一边持续导航产生事件
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = tab.Navigate(ctx, srv.URL)
		}
	})

	// 一边反复 Start/Stop，逼出「事件正在派发时 Wait」的交错
	l := tab.Listen("")
	listenCtx, listenCancel := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer listenCancel()
	for i := range 6 {
		if err := l.Start(listenCtx); err != nil {
			t.Fatalf("第 %d 次 Start 失败：%v", i, err)
		}
		time.Sleep(120 * time.Millisecond)
		l.Stop()
		_ = l.Records()
	}
	close(stop)
	wg.Wait()

	// 停止后可以重新启动，且记录不丢
	if err := l.Start(listenCtx); err != nil {
		t.Fatalf("重新 Start 失败：%v", err)
	}
	l.Stop()
}

// TestListenerWaitIdleUnderLoad 守住 WaitIdle 与事件派发之间的交错。
//
// WaitIdle 不能用裸的 sync.WaitGroup.Wait 实现：Wait 被 Done 唤醒之后、真正返回之前
// 有一个窗口，此时若又有一批事件到达并执行 wg.Add(1)，标准库在 Wait 的收尾检查里
// 会看到计数重新变为非零，直接
// panic("sync: WaitGroup is reused before previous Wait has returned")。
//
// 用例刻意让页面用 setInterval + fetch 持续制造事件（而不是靠导航产生一阵一阵的突发），
// 再用多个 goroutine 同时压 WaitIdle —— 这样「计数归零」与「新任务入队」的交错才会密集出现。
func TestListenerWaitIdleUnderLoad(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 页面自己持续发请求，形成稳定事件流
		fmt.Fprint(w, `<html><body><script>
setInterval(function(){ fetch('/ping?t=' + Date.now()); }, 4);
</script></body></html>`)
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := relayBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("新建标签页失败：%v", err)
	}
	if err := tab.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("导航失败：%v", err)
	}

	l := tab.Listen("/ping")
	listenCtx, listenCancel := context.WithTimeout(tab.Ctx, 60*time.Second)
	defer listenCancel()
	if err := l.Start(listenCtx); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	defer l.Stop()

	// 多个 goroutine 一起等空闲，把「Wait 刚被唤醒又有人 Add」的窗口压出来
	deadline := time.Now().Add(15 * time.Second)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				l.WaitIdle()
			}
		})
	}
	wg.Wait()

	if l.Len() == 0 {
		t.Fatal("持续请求期间应当捕获到 /ping 记录")
	}
}
