//go:build smoke

// Package smoke 是依赖真实 Chrome 的端到端冒烟测试。
//
// 运行方式：
//
//	go test -tags smoke -timeout 5m ./smoke/...
//
// 之所以用 build tag 隔离：普通单元测试刻意不依赖浏览器，CI 里必须能秒级跑完；
// 而「选择器是否真的能点到元素」「iframe 跨域能不能读」「Session 的 Cookie 会不会回传」
// 这些只有真实浏览器能回答。找不到浏览器时全部用例自动 skip，不会误报失败。
package smoke

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
	"github.com/yymm456/go-drission/session"
)

// 每个用例共享同一个浏览器实例，避免反复冷启动 Chrome（一次启动约 1~2 秒）。
var (
	sharedBrowser *chromium.Browser
	sharedTab     *chromium.Tab
	errShared     error
	sharedOnce    bool
)

// sharedUserDataDir 是共享浏览器使用的用户数据目录，由 TestMain 创建与清理。
//
// 必须显式指定。chromium 在未指定用户数据目录时，会按「本次使用的端口」在
// %TEMP%\go-drission\userData\<port> 下建一个目录；而库本身刻意不删用户数据目录
// （真实用户的登录态就在里面，替用户删是灾难）。测试图省事不指定，代价就是每跑一次
// 冒烟都在系统临时目录里留一份几百 MB 的垃圾——实测本机跑到第 78 次时累积了 1.1 GB。
// 所以测试侧必须自己管：TestMain 建、TestMain 删。
var sharedUserDataDir string

// TestMain 管理共享浏览器与它的用户数据目录。
//
// 顺带补上 sharedBrowser 的关闭：之前所有用例共用它，却没人负责关，
// 排他锁与 Chrome 进程只能等测试进程退出时被动回收。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "go-drission-smoke-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建共享用户数据目录失败：%v\n", err)
		os.Exit(1)
	}
	sharedUserDataDir = dir

	code := m.Run()

	// 先关浏览器再删目录：目录里还有被 Chrome 独占的文件，
	// 顺序反了会报「另一个程序正在使用此文件」，删不干净。
	if sharedBrowser != nil {
		sharedBrowser.Close()
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// setup 返回共享的浏览器与标签页。找不到浏览器时 skip 而不是 fail。
func setup(t *testing.T) (*chromium.Browser, *chromium.Tab) {
	t.Helper()

	if !sharedOnce {
		sharedOnce = true
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		sharedBrowser, sharedTab, errShared = chromium.OpenPage(ctx, 0,
			chromium.WithUserDataDir(sharedUserDataDir),
			chromium.WithHeadless(true),
			chromium.WithDefaultTimeout(15*time.Second),
			// 测试流量全走 127.0.0.1，必须绕开环境里可能存在的系统代理。
			chromium.WithFlag("no-proxy-server", ""),
		)
	}
	if errShared != nil {
		if errors.Is(errShared, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", errShared)
		}
		t.Fatalf("启动浏览器失败：%v", errShared)
	}
	return sharedBrowser, sharedTab
}

// ctxOf 派生一个带超时的操作用 ctx。
func ctxOf(t *testing.T, tab *chromium.Tab, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(tab.Ctx, d)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------- 测试页面

const frameHTML = `<!doctype html><html><head><meta charset="utf-8"><title>框架页</title></head>
<body>
  <h1 class="h">框架标题</h1>
  <input id="user" type="text">
  <input id="pass" type="password">
  <button id="fb" onclick="document.getElementById('fo').textContent='frame-clicked'">提交</button>
  <div id="fo"></div>
  <table><tr><td>甲</td></tr><tr><td>乙</td></tr><tr><td>丙</td></tr></table>
  <div data-x="a">带引号属性一</div><div data-x="a">带引号属性二</div>
</body></html>`

// mainHTML 里的 __FRAME__ / __CROSS__ 占位会被替换成两个测试服务器的地址。
const mainHTML = `<!doctype html><html><head><meta charset="utf-8"><title>主页面</title></head>
<body>
  <h1 id="title">主标题</h1>
  <div class="item"><span>项目一</span></div>
  <div class="item"><span>项目二</span></div>
  <div class="item"><span>项目三</span></div>
  <input id="kw" type="text">
  <button id="btn" onclick="document.getElementById('out').textContent='clicked'">点我</button>
  <div id="out"></div>
  <my-widget></my-widget>
  <iframe id="f1" name="frameA" src="__FRAME__" width="500" height="300"></iframe>
  <iframe id="f2" src="__CROSS__" width="500" height="300"></iframe>
  <script>
    class W extends HTMLElement {
      connectedCallback() {
        const r = this.attachShadow({mode: 'open'});
        r.innerHTML = '<p id="shadow-text">影子文本</p>' +
          '<button id="shadow-btn" onclick="this.getRootNode().getElementById(\'shadow-out\').textContent=\'shadow-clicked\'">影子按钮</button>' +
          '<span id="shadow-out"></span>';
      }
    }
    customElements.define('my-widget', W);
  </script>
</body></html>`

// testServers 起两个 HTTP 服务：主站与「跨域」站点，用于验证跨域 iframe。
// 同时为 session 包提供一组回显 / Cookie 接口。
type testServers struct {
	main  *httptest.Server
	cross *httptest.Server
}

func startServers(t *testing.T) *testServers {
	t.Helper()

	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, frameHTML)
	}))

	// 先声明再赋值：handler 里要引用自身地址拼 iframe 的 src。
	var main *httptest.Server
	main = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/main.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			page := strings.ReplaceAll(mainHTML, "__FRAME__", main.URL+"/frame.html")
			page = strings.ReplaceAll(page, "__CROSS__", cross.URL+"/frame.html")
			fmt.Fprint(w, page)

		case "/frame.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, frameHTML)

		// --- 以下是 session 包用的接口 ---
		case "/api/get":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":true,"ua":%q}`, r.Header.Get("User-Agent"))

		case "/api/json":
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"method":%q,"body":%s}`, r.Method, string(body))

		case "/api/form":
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"a":%q}`, r.FormValue("a"))

		case "/set-cookie":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc123", Path: "/", MaxAge: 3600})
			fmt.Fprint(w, "ok")

		// 一次响应下发**两个** Set-Cookie：这是「Record.SetCookies 为什么必须是切片」
		// 的现场 —— ResponseHeaders 是 map[string]string，同名键装不下两条，
		// 只往 map 里合并的话这里就会只剩最后一条。
		case "/set-two-cookies":
			http.SetCookie(w, &http.Cookie{Name: "a", Value: "1", Path: "/", MaxAge: 3600})
			http.SetCookie(w, &http.Cookie{Name: "b", Value: "2", Path: "/", MaxAge: 3600})
			fmt.Fprint(w, "ok")

		case "/profile":
			c, err := r.Cookie("sid")
			if err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, "no cookie")
				return
			}
			fmt.Fprintf(w, "hello %s", c.Value)

		case "/redirect":
			http.Redirect(w, r, "/api/get", http.StatusFound)

		default:
			http.NotFound(w, r)
		}
	}))

	return &testServers{main: main, cross: cross}
}

// gotoMain 导航到主页面并等待就绪。
func gotoMain(t *testing.T, tab *chromium.Tab, srv *testServers) {
	t.Helper()
	ctx := ctxOf(t, tab, 30*time.Second)
	if err := tab.Navigate(ctx, srv.main.URL+"/main.html"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待页面就绪失败：%v", err)
	}
}

// ---------------------------------------------------------------- 选择器

func TestSelectorCSS(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	got, err := tab.EleCSS("#title").Text(ctx)
	if err != nil || got != "主标题" {
		t.Fatalf("CSS 读文本：got=%q err=%v", got, err)
	}

	n, err := tab.EleCSS(".item").Count(ctx)
	if err != nil || n != 3 {
		t.Fatalf("CSS 计数：got=%d err=%v", n, err)
	}

	attr, err := tab.EleCSS("#f1").Attribute(ctx, "name")
	if err != nil || attr != "frameA" {
		t.Fatalf("CSS 读属性：got=%q err=%v", attr, err)
	}
}

func TestSelectorXPath(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	// 取第二个 .item 下的 span
	got, err := tab.EleXPath("//div[@class='item'][2]/span").Text(ctx)
	if err != nil || got != "项目二" {
		t.Fatalf("XPath 读文本：got=%q err=%v", got, err)
	}

	n, err := tab.EleXPath("//div[@class='item']").Count(ctx)
	if err != nil || n != 3 {
		t.Fatalf("XPath 计数：got=%d err=%v", n, err)
	}
}

func TestSelectorID(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	got, err := tab.EleID("title").Text(ctx)
	if err != nil || got != "主标题" {
		t.Fatalf("ID 读文本：got=%q err=%v", got, err)
	}
}

// TestSelectorJS 验证 JS() 的语义：一段「返回元素的 JS 表达式」，
// 典型用途是穿透 Shadow DOM——CSS / XPath 都够不到影子根内部的节点。
func TestSelectorJS(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	sel := chromium.JS(`document.querySelector('my-widget').shadowRoot.querySelector('#shadow-text')`)
	got, err := tab.Ele(sel).Text(ctx)
	if err != nil || got != "影子文本" {
		t.Fatalf("JS 穿透 Shadow DOM 读文本失败：got=%q err=%v", got, err)
	}
}

// TestSelectorNotFound 验证找不到元素时返回可判断的哨兵错误，
// 而不是裸的 context deadline exceeded。
func TestSelectorNotFound(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 8*time.Second)

	// Count 的语义是「数一数有几个」，没匹配到就是 0，不算错误
	n, err := tab.EleCSS("#no-such-element").Count(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Count 未匹配时应返回 0 且无错误：got=%d err=%v", n, err)
	}

	// 取文本/属性这类「必须有元素」的操作，才返回可判断的哨兵错误
	if _, err := tab.EleCSS("#no-such-element").Text(ctx); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Fatalf("Text 未匹配时期望 ErrElementNotFound，实际：%v", err)
	}
	if _, err := tab.EleCSS("#no-such-element").Attribute(ctx, "id"); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Fatalf("Attribute 未匹配时期望 ErrElementNotFound，实际：%v", err)
	}
	if err := tab.EleXPath("//div[@id='no-such-element']").ClickJS(ctx); !errors.Is(err, chromium.ErrElementNotFound) {
		t.Fatalf("ClickJS 未匹配时期望 ErrElementNotFound，实际：%v", err)
	}
}

// TestSelectorEmpty 验证空选择器被拦住，而不是静默定位到 <html>。
func TestSelectorEmpty(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 8*time.Second)

	_, err := tab.EleCSS("").Count(ctx)
	if !errors.Is(err, chromium.ErrSelectorRequired) {
		t.Fatalf("期望 ErrSelectorRequired，实际：%v", err)
	}
}

// ---------------------------------------------------------------- 交互

func TestClickAndSetValue(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	// SetValue 改的是 DOM property，用 Eval 读 value 校验（Attribute 读的是 attribute）
	if err := tab.EleID("kw").SetValue(ctx, "golang"); err != nil {
		t.Fatalf("SetValue by ID 失败：%v", err)
	}
	v, err := tab.Eval(ctx, `document.getElementById('kw').value`)
	if err != nil || v != "golang" {
		t.Fatalf("SetValue by ID 未生效：got=%v err=%v", v, err)
	}

	// 换 XPath 再设一次，验证四种选择器行为一致
	if err := tab.EleXPath("//input[@id='kw']").SetValue(ctx, "drission"); err != nil {
		t.Fatalf("SetValue by XPath 失败：%v", err)
	}
	v, err = tab.Eval(ctx, `document.getElementById('kw').value`)
	if err != nil || v != "drission" {
		t.Fatalf("SetValue by XPath 未生效：got=%v err=%v", v, err)
	}

	// 点击（原生 click）
	if err := tab.EleCSS("#btn").Click(ctx); err != nil {
		t.Fatalf("Click by CSS 失败：%v", err)
	}
	out, err := tab.EleID("out").Text(ctx)
	if err != nil || out != "clicked" {
		t.Fatalf("点击未触发：got=%q err=%v", out, err)
	}

	// ClickJS 绕可见性，用 XPath 定位同一个按钮后再点一次（这里改为验证不报错）
	if err := tab.EleXPath("//button[@id='btn']").ClickJS(ctx); err != nil {
		t.Fatalf("ClickJS by XPath 失败：%v", err)
	}

	// Shadow DOM 内的按钮：JS 选择器点击
	if err := tab.EleJS(`document.querySelector('my-widget').shadowRoot.querySelector('#shadow-btn')`).Click(ctx); err != nil {
		t.Fatalf("Shadow DOM 内点击失败：%v", err)
	}
	sout, err := tab.Eval(ctx, `document.querySelector('my-widget').shadowRoot.querySelector('#shadow-out').textContent`)
	if err != nil || sout != "shadow-clicked" {
		t.Fatalf("Shadow DOM 点击未触发：got=%v err=%v", sout, err)
	}
}

func TestSendKeys(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	if err := tab.EleID("kw").SendKeys(ctx, "hello"); err != nil {
		t.Fatalf("SendKeys 失败：%v", err)
	}
	v, err := tab.Eval(ctx, `document.getElementById('kw').value`)
	if err != nil || v != "hello" {
		t.Fatalf("SendKeys 未生效：got=%v err=%v", v, err)
	}
}

func TestWaitBuilder(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 20*time.Second)

	if err := tab.EleCSS(".item").Wait().Count(3).Do(ctx); err != nil {
		t.Fatalf("Wait Count 失败：%v", err)
	}
	if err := tab.EleID("title").Wait().Text("主标题").Do(ctx); err != nil {
		t.Fatalf("Wait Text 失败：%v", err)
	}
	if err := tab.EleXPath("//div[@class='item']").Wait().Visible().Do(ctx); err != nil {
		t.Fatalf("Wait Visible by XPath 失败：%v", err)
	}
	if err := tab.Wait().URL("main.html").Do(ctx); err != nil {
		t.Fatalf("Wait URL 失败：%v", err)
	}
	if err := tab.Wait().Ready().Do(ctx); err != nil {
		t.Fatalf("Wait Ready 失败：%v", err)
	}
}

// ---------------------------------------------------------------- iframe

func TestIframeFrames(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	frames, err := tab.Frames(ctx)
	if err != nil {
		t.Fatalf("Frames 失败：%v", err)
	}
	// Frames 只返回 iframe，主框架不算在内
	if len(frames) < 2 {
		t.Fatalf("期望至少 2 个 iframe，实际 %d", len(frames))
	}
}

func TestIframeLocate(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	byCSS, err := tab.Frame(ctx, chromium.CSS("#f1"))
	if err != nil {
		t.Fatalf("Frame by CSS 失败：%v", err)
	}
	if byCSS.Name() != "frameA" {
		t.Fatalf("Frame by CSS name 不符：%q", byCSS.Name())
	}

	byName, err := tab.FrameByName(ctx, "frameA")
	if err != nil {
		t.Fatalf("FrameByName 失败：%v", err)
	}
	if byName.URL() != byCSS.URL() {
		t.Fatalf("按 name 与按 CSS 定位到的不是同一个框架：%q vs %q", byName.URL(), byCSS.URL())
	}

	byURL, err := tab.FrameByURL(ctx, "/frame.html")
	if err != nil {
		t.Fatalf("FrameByURL 失败：%v", err)
	}
	if byURL.URL() == "" {
		t.Fatal("FrameByURL 返回空 URL")
	}

	if _, err := tab.FrameByName(ctx, "no-such-frame"); !errors.Is(err, chromium.ErrFrameNotFound) {
		t.Fatalf("期望 ErrFrameNotFound，实际：%v", err)
	}
}

// TestIframeSameOrigin 验证同域 iframe 内可读写、可点击。
func TestIframeSameOrigin(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 20*time.Second)

	f, err := tab.Frame(ctx, chromium.CSS("#f1"))
	if err != nil {
		t.Fatalf("定位 iframe 失败：%v", err)
	}

	// 中文文本必须正确解码（RemoteObject.Value 是 json.RawMessage，忘了解码会拿到 \uXXXX）
	text, err := f.EleCSS(".h").Text(ctx)
	if err != nil || text != "框架标题" {
		t.Fatalf("iframe 内读中文文本失败：got=%q err=%v", text, err)
	}

	n, err := f.EleCSS("tr").Count(ctx)
	if err != nil || n != 3 {
		t.Fatalf("iframe 内计数失败：got=%d err=%v", n, err)
	}

	// XPath 计数走的是 document.evaluate("count(...)") 分支，必须用 NUMBER_TYPE(1)；
	// 曾经误用 FIRST_ORDERED_NODE_TYPE(9) 会抛 TypeError 使计数恒为 0，这里守住回归。
	nx, errx := f.EleXPath("//tr").Count(ctx)
	if errx != nil || nx != 3 {
		t.Fatalf("iframe 内 XPath 计数失败：got=%d err=%v", nx, errx)
	}

	// XPath 里带双引号的属性值是常态（//div[@data-x="a"]）。这里守住回归：
	// 曾经把 count(...) 裸拼进 JS 的 "..." 字面量，XPath 一含 " 就把字符串提前截断，
	// 整段脚本语法错误——表现出来是「Count 报错」而不是「数错了」，
	// 上面那条 //tr 因为没引号所以一直没暴露。
	nq, errq := f.EleXPath(`//div[@data-x="a"]`).Count(ctx)
	if errq != nil || nq != 2 {
		t.Fatalf("XPath 含双引号时计数失败：got=%d err=%v", nq, errq)
	}

	if err := f.EleID("user").SetValue(ctx, "admin"); err != nil {
		t.Fatalf("iframe 内设值失败：%v", err)
	}
	v, err := f.Eval(ctx, `document.getElementById('user').value`)
	if err != nil || v != "admin" {
		t.Fatalf("iframe 内设值未生效：got=%v err=%v", v, err)
	}

	if err := f.EleCSS("#fb").Click(ctx); err != nil {
		t.Fatalf("iframe 内点击失败：%v", err)
	}
	out, err := f.EleID("fo").Text(ctx)
	if err != nil || out != "frame-clicked" {
		t.Fatalf("iframe 内点击未触发：got=%q err=%v", out, err)
	}

	html, err := f.HTML(ctx)
	if err != nil || !strings.Contains(html, "框架标题") {
		t.Fatalf("iframe HTML 不含预期内容：len=%d err=%v", len(html), err)
	}

	u, err := f.URLNow(ctx)
	if err != nil || !strings.Contains(u, "/frame.html") {
		t.Fatalf("iframe URLNow 失败：got=%q err=%v", u, err)
	}
}

// TestIframeCrossOrigin 验证跨域 iframe 同样能读——这是 isolated world 方案
// 相对「switch_to_frame + 同源策略」的关键优势。
func TestIframeCrossOrigin(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 20*time.Second)

	f, err := tab.Frame(ctx, chromium.CSS("#f2"))
	if err != nil {
		t.Fatalf("定位跨域 iframe 失败：%v", err)
	}

	text, err := f.EleCSS(".h").Text(ctx)
	if err != nil || text != "框架标题" {
		t.Fatalf("跨域 iframe 读文本失败：got=%q err=%v", text, err)
	}

	if err := f.EleID("fb").Click(ctx); err != nil {
		t.Fatalf("跨域 iframe 点击失败：%v", err)
	}
	out, err := f.EleID("fo").Text(ctx)
	if err != nil || out != "frame-clicked" {
		t.Fatalf("跨域 iframe 点击未触发：got=%q err=%v", out, err)
	}
}

// ---------------------------------------------------------------- Session

func TestSessionGet(t *testing.T) {
	srv := startServers(t)
	s := session.New(session.WithTimeout(10 * time.Second))

	resp, err := s.Get(context.Background(), srv.main.URL+"/api/get")
	if err != nil {
		t.Fatalf("Session GET 失败：%v", err)
	}
	if !resp.OK() || resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码异常：%d", resp.StatusCode)
	}
	var out map[string]any
	if err := resp.JSON(&out); err != nil {
		t.Fatalf("解析 JSON 失败：%v", err)
	}
	if out["ok"] != true {
		t.Fatalf("响应内容不符：%v", out)
	}
	// body 已缓存，重复读取必须还能拿到内容
	if !strings.Contains(resp.Text(), "ok") {
		t.Fatalf("body 重复读取失败：%q", resp.Text())
	}
}

func TestSessionPost(t *testing.T) {
	srv := startServers(t)
	s := session.New()

	resp, err := s.PostJSON(context.Background(), srv.main.URL+"/api/json", map[string]any{"name": "go"})
	if err != nil {
		t.Fatalf("PostJSON 失败：%v", err)
	}
	if !strings.Contains(resp.Text(), `"name":"go"`) {
		t.Fatalf("PostJSON 回显不符：%s", resp.Text())
	}

	resp, err = s.PostForm(context.Background(), srv.main.URL+"/api/form", url.Values{"a": {"1"}})
	if err != nil {
		t.Fatalf("PostForm 失败：%v", err)
	}
	if !strings.Contains(resp.Text(), `"a":"1"`) {
		t.Fatalf("PostForm 回显不符：%s", resp.Text())
	}
}

func TestSessionHeaders(t *testing.T) {
	srv := startServers(t)
	s := session.New(session.WithUserAgent("go-drission-smoke"))

	resp, err := s.Get(context.Background(), srv.main.URL+"/api/get")
	if err != nil {
		t.Fatalf("GET 失败：%v", err)
	}
	if !strings.Contains(resp.Text(), "go-drission-smoke") {
		t.Fatalf("默认 UA 未生效：%s", resp.Text())
	}
}

func TestSessionRedirect(t *testing.T) {
	srv := startServers(t)

	s := session.New()
	resp, err := s.Get(context.Background(), srv.main.URL+"/redirect")
	if err != nil {
		t.Fatalf("跟随重定向失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望跟随到 200，实际 %d", resp.StatusCode)
	}

	s2 := session.New(session.WithNoRedirect())
	resp2, err := s2.Get(context.Background(), srv.main.URL+"/redirect")
	if err != nil {
		t.Fatalf("禁止重定向时请求失败：%v", err)
	}
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("期望停在 302，实际 %d", resp2.StatusCode)
	}
}

// TestSessionCookieReuse 是 Session 的核心价值：Cookie 自动回传，
// 且可导出成文件后由另一个 Session（或下次启动）接着用。
func TestSessionCookieReuse(t *testing.T) {
	srv := startServers(t)
	s := session.New()

	if _, err := s.Get(context.Background(), srv.main.URL+"/set-cookie"); err != nil {
		t.Fatalf("设置 Cookie 失败：%v", err)
	}

	resp, err := s.Get(context.Background(), srv.main.URL+"/profile")
	if err != nil {
		t.Fatalf("带 Cookie 请求失败：%v", err)
	}
	if resp.Text() != "hello abc123" {
		t.Fatalf("Cookie 未自动回传：%q", resp.Text())
	}

	// 导出到文件，另一个 Session 载入后应直接是登录态
	dir := t.TempDir()
	file := filepath.Join(dir, "cookies.json")
	if err := s.SaveCookies(file); err != nil {
		t.Fatalf("保存 Cookie 失败：%v", err)
	}

	fresh := session.New()
	if _, err := fresh.Get(context.Background(), srv.main.URL+"/profile"); err != nil {
		t.Fatalf("新 Session 首次请求失败：%v", err)
	}
	if err := fresh.LoadCookies(file); err != nil {
		t.Fatalf("载入 Cookie 失败：%v", err)
	}
	resp2, err := fresh.Get(context.Background(), srv.main.URL+"/profile")
	if err != nil {
		t.Fatalf("复用 Cookie 后请求失败：%v", err)
	}
	if resp2.Text() != "hello abc123" {
		t.Fatalf("跨 Session 复用 Cookie 失败：%q", resp2.Text())
	}

	if _, err := os.Stat(file); err != nil {
		t.Fatalf("Cookie 文件不存在：%v", err)
	}
}

func TestSessionManualCookie(t *testing.T) {
	s := session.New()
	s.SetCookie(session.CookieItem{
		Name:   "token",
		Value:  "xyz",
		Domain: "example.com",
		Path:   "/",
	})

	items := s.Cookies()
	found := false
	for _, it := range items {
		if it.Name == "token" && it.Value == "xyz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("手动设置的 Cookie 未出现在 Cookies() 中：%+v", items)
	}

	s.ClearCookies()
	if s.Jar().Len() != 0 {
		t.Fatalf("ClearCookies 后仍剩 %d 条", s.Jar().Len())
	}
}

// TestSessionShareJar 验证多个 Session 共用同一个 Jar 时 Cookie 实时共享。
func TestSessionShareJar(t *testing.T) {
	srv := startServers(t)
	jar := session.NewJar()

	s1 := session.New(session.WithJar(jar))
	if _, err := s1.Get(context.Background(), srv.main.URL+"/set-cookie"); err != nil {
		t.Fatalf("s1 设置 Cookie 失败：%v", err)
	}

	s2 := session.New(session.WithJar(jar))
	resp, err := s2.Get(context.Background(), srv.main.URL+"/profile")
	if err != nil {
		t.Fatalf("s2 请求失败：%v", err)
	}
	if resp.Text() != "hello abc123" {
		t.Fatalf("共享 Jar 未生效：%q", resp.Text())
	}
}

func TestSessionSaveFile(t *testing.T) {
	srv := startServers(t)
	s := session.New()

	resp, err := s.Get(context.Background(), srv.main.URL+"/api/get")
	if err != nil {
		t.Fatalf("GET 失败：%v", err)
	}
	path := filepath.Join(t.TempDir(), "sub", "out.json")
	if err := resp.SaveFile(path); err != nil {
		t.Fatalf("SaveFile 失败：%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落盘文件失败：%v", err)
	}
	if !strings.Contains(string(data), "ok") {
		t.Fatalf("落盘内容不符：%s", data)
	}
}

// ---------------------------------------------------------------- 回归

func TestAntiDetect(t *testing.T) {
	srv := startServers(t)

	// 反检测**默认关闭**，所以这里必须显式 WithAntiDetect(true)，
	// 并且要起独立实例：setup() 的共享浏览器是按默认配置建的，
	// 拿它来断言反检测必然失败（它的 navigator.webdriver 就是原生的 true）。
	dir := t.TempDir()
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBoot()

	b, tab, err := chromium.OpenPage(bootCtx, 0,
		chromium.WithUserDataDir(dir),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
		chromium.WithAntiDetect(true),
	)
	if err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("启动（开启反检测的）浏览器失败：%v", err)
	}
	t.Cleanup(b.Close)

	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	wd, err := tab.Eval(ctx, `navigator.webdriver`)
	if err != nil {
		t.Fatalf("读取 navigator.webdriver 失败：%v", err)
	}
	// 反检测开启时该属性应为 null（JS 里 undefined 经 JSON 变成 null）
	if wd != nil {
		t.Fatalf("navigator.webdriver 未被抹除，实际：%v", wd)
	}

	hasChrome, err := tab.Eval(ctx, `typeof window.chrome`)
	if err != nil || hasChrome != "object" {
		t.Fatalf("window.chrome 缺失：got=%v err=%v", hasChrome, err)
	}
}

// TestAntiDetectDisabledByDefault 钉住「反检测默认关闭」这条默认值。
//
// 默认值是最容易在重构里被静默翻转的东西：改错了不会有编译错误，
// 也不会影响任何显式传参的用例。这里用共享实例（它就是按默认配置建的）
// 反向确认 navigator.webdriver 保持原生的 true。
func TestAntiDetectDisabledByDefault(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	wd, err := tab.Eval(ctx, `navigator.webdriver`)
	if err != nil {
		t.Fatalf("读取 navigator.webdriver 失败：%v", err)
	}
	// 判据只能是「有没有被抹成 undefined」，不能对具体布尔值下断言：
	// 反检测开启时该属性被删成 undefined（经 JSON 变 null）；未开启时保持原生值，
	// 而原生值随 Chrome 版本与 headless 模式而变 —— 本机实测新版 headless 下是 false，
	// 不是旧文献里常写的 true。
	if wd == nil {
		t.Fatalf("反检测默认应为关闭，但 navigator.webdriver 已被抹成 undefined")
	}
	t.Logf("默认配置下 navigator.webdriver = %v（原生值，未被抹除）", wd)
}

func TestTitleAndURL(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	title, err := tab.Title(ctx)
	if err != nil || title != "主页面" {
		t.Fatalf("读标题失败：got=%q err=%v", title, err)
	}
	u, err := tab.CurrentURL(ctx)
	if err != nil || !strings.Contains(u, "/main.html") {
		t.Fatalf("读地址失败：got=%q err=%v", u, err)
	}
}

// TestListener 验证网络监听能抓到真实请求，且 Records() 返回的是深拷贝（不 panic）。
func TestListener(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	listenCtx, stop := context.WithCancel(tab.Ctx)
	defer stop()

	listener := tab.Listen("/api/get")
	if err := listener.Start(listenCtx); err != nil {
		t.Fatalf("启动监听失败：%v", err)
	}
	defer listener.Stop()

	if err := tab.Navigate(ctx, srv.main.URL+"/api/get"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	listener.WaitIdle()

	recs := listener.Records()
	if len(recs) == 0 {
		t.Fatal("监听未抓到任何记录")
	}
	hit := false
	for _, r := range recs {
		if strings.Contains(r.URL, "/api/get") {
			hit = true
			if r.Status != 200 {
				t.Fatalf("抓到的响应状态码异常：%d", r.Status)
			}
		}
	}
	if !hit {
		t.Fatalf("未抓到目标请求，记录数 %d", len(recs))
	}
}

// TestListenerSetCookiesFromExtraInfo 守住「响应头必须带 Set-Cookie」。
//
// CDP 把 Set-Cookie 藏在 responseReceivedExtraInfo 事件里，只听 responseReceived
// 拿不到 —— 这条用例就是那个缺口的现场。选 /set-two-cookies（一次下发两条）是为了
// 同时验证「多值不会被 ResponseHeaders 的 map 覆盖掉只剩一条」。
func TestListenerSetCookiesFromExtraInfo(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, tab, 60*time.Second)

	l := tab.Listen("/set-two-cookies")
	if err := l.Start(ctx); err != nil {
		t.Fatalf("启动监听失败：%v", err)
	}
	t.Cleanup(l.Stop)

	if err := tab.Navigate(ctx, srv.main.URL+"/set-two-cookies"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待就绪失败：%v", err)
	}
	l.WaitIdle()

	var hit *chromium.Record
	for _, r := range l.Records() {
		if strings.Contains(r.URL, "/set-two-cookies") {
			hit = r
			break
		}
	}
	if hit == nil {
		t.Fatalf("没有抓到 /set-two-cookies 的记录（共 %d 条）：可能页面没真正发出请求", l.Len())
	}

	if len(hit.SetCookies) < 2 {
		t.Fatalf("期望至少 2 条 Set-Cookie（a=1 / b=2），实际 %v —— "+
			"若为空，说明 ExtraInfo 事件没有合并进来", hit.SetCookies)
	}
	joined := strings.Join(hit.SetCookies, "\n")
	if !strings.Contains(joined, "a=1") || !strings.Contains(joined, "b=2") {
		t.Fatalf("Set-Cookie 内容不符，期望含 a=1 与 b=2，实际：%v", hit.SetCookies)
	}
	if hit.ResponseHeaders["Set-Cookie"] == "" && !hasSetCookieKey(hit.ResponseHeaders) {
		t.Errorf("ResponseHeaders 也应带上 Set-Cookie，实际：%v", hit.ResponseHeaders)
	}
}

// hasSetCookieKey 大小写不敏感地找 Set-Cookie 键（HTTP 头名不区分大小写）。
func hasSetCookieKey(h map[string]string) bool {
	for k := range h {
		if strings.EqualFold(k, "set-cookie") {
			return true
		}
	}
	return false
}

// TestListenerResponseBodyBackfill 守住「后台补取响应体」这条异步路径。
//
// 这是 Listener 里唯一会异步往 Record 里写字的地方（handleLoadingFinished →
// fetchBodyAsync → GetResponseBody → 回填 ResponseBody）。它此前没有任何断言覆盖：
// 把整个回填删掉，其余 listener 用例照样全绿——所以这里用响应体里的固定标记钉住它。
//
// WaitIdle 必须在 Records 之前：body 是事件派发之后异步补的，不等就会读到空串。
func TestListenerResponseBodyBackfill(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	ctx := ctxOf(t, tab, 30*time.Second)

	listenCtx, stop := context.WithCancel(tab.Ctx)
	defer stop()

	listener := tab.Listen("/api/get")
	if err := listener.Start(listenCtx); err != nil {
		t.Fatalf("启动监听失败：%v", err)
	}
	defer listener.Stop()

	if err := tab.Navigate(ctx, srv.main.URL+"/api/get"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	listener.WaitIdle()

	recs := listener.Records()
	for _, r := range recs {
		if !strings.Contains(r.URL, "/api/get") {
			continue
		}
		if !strings.Contains(r.ResponseBody, `"ok":true`) {
			t.Fatalf("响应体未被回填（fetchBodyAsync 失效？）：%q", r.ResponseBody)
		}
		return
	}
	t.Fatalf("未抓到 /api/get 记录，记录数 %d", len(recs))
}

// TestIsolatedContext 验证单浏览器内多上下文的 Cookie 隔离。
func TestIsolatedContext(t *testing.T) {
	browser, _ := setup(t)
	srv := startServers(t)

	root := ctxOf(t, sharedTab, 60*time.Second)
	name := fmt.Sprintf("smoke_%d", time.Now().UnixNano())

	bc, err := browser.Context(root, name)
	if err != nil {
		t.Fatalf("创建隔离上下文失败：%v", err)
	}
	defer bc.Close()

	tab, err := bc.NewTab(root)
	if err != nil {
		t.Fatalf("上下文内新建标签页失败：%v", err)
	}
	ctx := ctxOf(t, tab, 30*time.Second)
	if err := tab.Navigate(ctx, srv.main.URL+"/set-cookie"); err != nil {
		t.Fatalf("导航失败：%v", err)
	}
	if err := tab.WaitReady(ctx); err != nil {
		t.Fatalf("等待就绪失败：%v", err)
	}

	// 同一上下文内 Cookie 应可见
	v, err := tab.Eval(ctx, `document.cookie`)
	if err != nil {
		t.Fatalf("读 Cookie 失败：%v", err)
	}
	if !strings.Contains(fmt.Sprint(v), "sid") {
		t.Fatalf("上下文内 Cookie 未生效：%v", v)
	}
}

// TestScreenshot 验证截图能落盘。
func TestScreenshot(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)

	path := filepath.Join(t.TempDir(), "shot.png")
	if err := tab.Screenshot(ctxOf(t, tab, 20*time.Second), path); err != nil {
		t.Fatalf("截图失败：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("截图文件不存在：%v", err)
	}
	if info.Size() == 0 {
		t.Fatal("截图文件为空")
	}
}

// ------------------------------------------------- 本轮回归（缺陷报告 BUG-04 / BUG-06）

// TestElementCountUnmatchedAcrossModes 守住 Element.Count 的「未命中」语义统一。
//
// Count 的文档写明「Count 为 0 是正常返回值（不是错误）」，但早期实现里只有 CSS
// 分支走 JS 求值，XPath / ID 走 chromedp.Nodes——而 chromedp 的节点查询在命中前
// 会一直重试到 ctx 到期，于是未命中变成「白等一个完整超时再报错」。
// 这里同时钉住结果（0/nil）与耗时（不能又在等超时）。
func TestElementCountUnmatchedAcrossModes(t *testing.T) {
	_, tab := setup(t)
	srv := startServers(t)
	gotoMain(t, tab, srv)
	ctx := ctxOf(t, tab, 15*time.Second)

	// 命中：四种定位方式都要数得对
	hits := []struct {
		name string
		el   *chromium.Element
		want int
	}{
		{"css", tab.EleCSS(".item"), 3},
		{"xpath", tab.EleXPath("//div[@class='item']"), 3},
		{"id", tab.EleID("title"), 1},
		{"jspath", tab.EleJS("document.querySelector('.item')"), 1},
	}
	for _, h := range hits {
		n, err := h.el.Count(ctx)
		if err != nil || n != h.want {
			t.Errorf("%s 命中 Count = %d, %v；期望 %d, nil", h.name, n, err, h.want)
		}
	}

	// 未命中：必须是 0/nil，且不能白等一个完整超时
	misses := []struct {
		name string
		el   *chromium.Element
	}{
		{"css", tab.EleCSS("#no-such-element")},
		{"xpath", tab.EleXPath("//div[@id='no-such-element']")},
		{"id", tab.EleID("no-such-element")},
		{"jspath", tab.EleJS("null")},
	}
	for _, m := range misses {
		start := time.Now()
		// 只给 3s 上限：修复前 XPath / ID 会一直重试到它到期
		n, err := m.el.Count(ctxOf(t, tab, 3*time.Second))
		elapsed := time.Since(start)
		if err != nil || n != 0 {
			t.Errorf("%s 未命中 Count = %d, %v；期望 0, nil", m.name, n, err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("%s 未命中的 Count 耗时 %v，说明又在等 ctx 超时", m.name, elapsed)
		}
	}
}

// TestIsolatedContextTabNotManagedByBrowser 守住「隔离上下文内的标签页被 Browser 重复托管」。
//
// 早期 syncTabs 走 HTTP /json 取 target 列表，而该端点不返回 browserContextId：
// 隔离上下文里的页面也被附着成 Browser 级 *Tab，同一个 target 被两套管理器各持一份，
// GetTab / LatestTab 会返回隔离上下文里的页面。现在改走 CDP Target.getTargets
// 并只收默认上下文的 page，隔离上下文的标签页归 BrowserContext 独管。
func TestIsolatedContextTabNotManagedByBrowser(t *testing.T) {
	b, _ := setup(t)
	ctx := ctxOf(t, sharedTab, 60*time.Second)

	bc, err := b.Context(ctx, fmt.Sprintf("smoke_probe_%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("创建隔离上下文失败：%v", err)
	}
	defer bc.Close()

	tabInCtx, err := bc.NewTab(ctx)
	if err != nil {
		t.Fatalf("隔离上下文内新建标签页失败：%v", err)
	}

	assertNotManaged := func(where string) {
		t.Helper()
		tabs, tabsErr := b.Tabs(ctx)
		if tabsErr != nil {
			t.Fatalf("%s 读取 Browser.Tabs 失败：%v", where, tabsErr)
		}
		for _, tl := range tabs {
			if tl.ID == tabInCtx.ID {
				t.Fatalf("%s：隔离上下文内的标签页 %s 不应出现在 Browser.Tabs 里", where, tabInCtx.ID)
			}
		}
	}
	assertNotManaged("新建隔离标签页后")

	// LatestTab 也不能返回隔离上下文里的页面
	if latest, latestErr := b.LatestTab(ctx); latestErr == nil && latest.ID == tabInCtx.ID {
		t.Fatal("Browser.LatestTab 返回了隔离上下文里的标签页")
	}

	// 默认上下文里新建的标签页仍要被正常托管（别把正常路径一起修坏）
	defTab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatalf("默认上下文新建标签页失败：%v", err)
	}
	defer b.CloseTab(ctx, defTab)

	found := false
	tabs, err := b.Tabs(ctx)
	if err != nil {
		t.Fatalf("读取 Browser.Tabs 失败：%v", err)
	}
	for _, tl := range tabs {
		switch tl.ID {
		case tabInCtx.ID:
			t.Fatalf("隔离上下文标签页不应出现在 Browser.Tabs 里：%s", tabInCtx.ID)
		case defTab.ID:
			found = true
		}
	}
	if !found {
		t.Fatalf("默认上下文新建的标签页 %s 应被 Browser 托管", defTab.ID)
	}
}
