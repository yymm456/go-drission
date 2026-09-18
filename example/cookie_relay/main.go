// example/cookie_relay 演示「免登录」：用纯 HTTP 完成登录，再把会话 Cookie
// 交给浏览器，直接打开业务页面——浏览器全程不会看到登录表单。
//
//	go run ./example/cookie_relay
//
// 为了让示例可以直接运行，这里用 net/http 起了一个本地站点当靶子：
//
//	POST /login  校验表单并下发 sid Cookie（纯 HTTP 就能完成）
//	GET  /home   把收到的 Cookie 回显在页面上，用于判断是否已登录
//
// 真实场景把 base 换成目标站点地址即可，其余流程完全一致。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yymm456/go-drission/chromium"
	"github.com/yymm456/go-drission/session"
)

func main() {
	// ---------- 0. 起一个带会话的本地站点当靶子 ----------
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			_ = r.ParseForm()
			if r.PostFormValue("user") != "alice" || r.PostFormValue("pass") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, "bad credentials")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "token-alice", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "role", Value: "admin", Path: "/"})
			fmt.Fprint(w, "login ok")
		case "/home":
			// 把请求头里的 Cookie 原样回显，方便肉眼确认「浏览器真的带上了登录态」
			fmt.Fprintf(w, "<!doctype html><html><body>cookie header: %s</body></html>",
				r.Header.Get("Cookie"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()

	// ---------- 1. 纯 HTTP 登录：快、稳、不依赖浏览器 ----------
	sess := session.New(session.WithTimeout(10 * time.Second))
	resp, err := sess.PostForm(ctx, srv.URL+"/login", url.Values{
		"user": {"alice"},
		"pass": {"secret"},
	})
	if err != nil {
		log.Fatalf("HTTP 登录失败: %v", err)
	}
	if !resp.OK() {
		log.Fatalf("HTTP 登录失败: 状态码 %d, body=%s", resp.StatusCode, resp.Text())
	}
	log.Printf("HTTP 登录成功，拿到 %d 条 Cookie", len(sess.Cookies()))

	// ---------- 2. 只导出目标站点用得上的 Cookie ----------
	items, err := sess.CookiesFor(srv.URL + "/home")
	if err != nil {
		log.Fatalf("按 URL 过滤 Cookie 失败: %v", err)
	}
	for _, c := range items {
		log.Printf("  待交接: %s=%s (domain=%s path=%s httpOnly=%v)", c.Name, c.Value, c.Domain, c.Path, c.HTTPOnly)
	}

	// ---------- 3. 启动浏览器，把登录态交进去 ----------
	browser, tab, err := chromium.OpenPage(ctx, 0,
		chromium.WithHeadless(false),
		chromium.WithDefaultTimeout(20*time.Second),
		chromium.WithConnectTimeout(30*time.Second),
		// 本地 httptest 站点无需代理，避免系统代理干扰
		chromium.WithFlag("no-proxy-server", ""),
	)
	if err != nil {
		log.Fatalf("启动浏览器失败: %v", err)
	}
	defer browser.Close()

	navCtx, cancel := context.WithTimeout(tab.Ctx, 40*time.Second)
	defer cancel()

	// LoginWithCookies = 注入 Cookie + 导航。因为 Cookie 在首次请求前就已就位，
	// 目标页面拿到的第一个响应就是已登录状态。
	if err := tab.LoginWithCookies(navCtx, srv.URL+"/home", sess.Jar()); err != nil {
		log.Fatalf("免登录失败: %v", err)
	}
	log.Printf("[默认上下文] 免登录完成: %s", pageText(navCtx, tab))

	// ---------- 4. 隔离上下文里同样免登录（多账号共用一个浏览器进程） ----------
	bc, err := browser.Context(navCtx, "relay-account-b")
	if err != nil {
		log.Fatalf("创建隔离上下文失败: %v", err)
	}
	tabB, err := bc.NewTab(navCtx)
	if err != nil {
		log.Fatalf("隔离上下文新建标签页失败: %v", err)
	}
	if err := tabB.LoginWithCookies(navCtx, srv.URL+"/home", sess.Jar()); err != nil {
		log.Fatalf("隔离上下文免登录失败: %v", err)
	}
	log.Printf("[隔离上下文] 免登录完成: %s", pageText(navCtx, tabB))

	// ---------- 5. 反方向：把浏览器里的 Cookie 交给 Session ----------
	// 写到临时目录，避免污染仓库（示例不需要产出可提交的产物）
	file := filepath.Join(os.TempDir(), "go-drission-cookies-demo.json")
	if err := tab.ExportCookies(navCtx, file, srv.URL); err != nil {
		log.Printf("导出 Cookie 失败: %v", err)
	} else {
		s2 := session.New()
		if err := s2.LoadCookies(file); err != nil {
			log.Printf("Session 载入浏览器 Cookie 失败: %v", err)
		} else {
			log.Printf("已把浏览器 Cookie 交回 Session，共 %d 条", len(s2.Cookies()))
		}
	}
}

// pageText 读取页面可见文字，用来肉眼确认服务端到底收到了哪些 Cookie。
func pageText(ctx context.Context, tab *chromium.Tab) string {
	text, err := tab.EleCSS("body").Text(ctx)
	if err != nil {
		return fmt.Sprintf("<读取失败: %v>", err)
	}
	return strings.TrimSpace(text)
}
