package session

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Jar ----------

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestJarSetAndGet(t *testing.T) {
	j := NewJar()
	u := mustURL(t, "https://example.com/path/page")

	j.SetCookies(u, []*http.Cookie{
		// 显式指定 Path，避免按请求路径 /path/page 推导出 /path/ 而影响断言
		{Name: "sid", Value: "abc", Path: "/"},
		{Name: "sub", Value: "yes", Domain: ".example.com", Path: "/"},
	})

	cookies := j.Cookies(u)
	if len(cookies) != 2 {
		t.Fatalf("期望 2 条 cookie，实际 %d", len(cookies))
	}

	// 子域能拿到设置了 Domain 的那条，拿不到 hostOnly 的那条
	sub := j.Cookies(mustURL(t, "https://a.example.com/"))
	if len(sub) != 1 || sub[0].Name != "sub" {
		t.Errorf("子域应只拿到 domain cookie，实际 %+v", sub)
	}
}

func TestJarHostOnlyNotSharedWithSubdomain(t *testing.T) {
	j := NewJar()
	j.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{
		{Name: "host", Value: "1"}, // 未指定 Domain → hostOnly
	})

	if got := j.Cookies(mustURL(t, "https://example.com/")); len(got) != 1 {
		t.Errorf("同域应命中 hostOnly cookie，实际 %d 条", len(got))
	}
	if got := j.Cookies(mustURL(t, "https://x.example.com/")); len(got) != 0 {
		t.Errorf("hostOnly cookie 不应泄漏到子域，实际 %+v", got)
	}
}

func TestJarRejectsForeignDomain(t *testing.T) {
	j := NewJar()
	// 从 evil.com 的响应里给 example.com 下 cookie，必须被拒绝
	j.SetCookies(mustURL(t, "https://evil.com/"), []*http.Cookie{
		{Name: "bad", Value: "1", Domain: "example.com"},
	})
	if j.Len() != 0 {
		t.Errorf("跨站下 cookie 应被拒绝，实际存了 %d 条", j.Len())
	}
}

func TestJarPathMatch(t *testing.T) {
	j := NewJar()
	u := mustURL(t, "https://example.com/admin/list")
	j.SetCookies(u, []*http.Cookie{{Name: "p", Value: "1"}}) // 未指定 path → /admin/

	if got := j.Cookies(mustURL(t, "https://example.com/admin/x")); len(got) != 1 {
		t.Error("/admin/x 应命中 /admin/ 下的 cookie")
	}
	if got := j.Cookies(mustURL(t, "https://example.com/other")); len(got) != 0 {
		t.Error("/other 不应命中 /admin/ 下的 cookie")
	}
}

func TestJarSecureOnlyOverHTTPS(t *testing.T) {
	j := NewJar()
	j.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{
		{Name: "s", Value: "1", Secure: true},
	})
	if got := j.Cookies(mustURL(t, "http://example.com/")); len(got) != 0 {
		t.Error("Secure cookie 不应出现在 http 请求里")
	}
	if got := j.Cookies(mustURL(t, "https://example.com/")); len(got) != 1 {
		t.Error("Secure cookie 应出现在 https 请求里")
	}
}

func TestJarExpiry(t *testing.T) {
	j := NewJar()
	u := mustURL(t, "https://example.com/")

	j.SetCookies(u, []*http.Cookie{{Name: "old", Value: "1", Expires: time.Now().Add(-time.Hour)}})
	if got := j.Cookies(u); len(got) != 0 {
		t.Error("已过期的 cookie 不应被返回")
	}

	j.SetCookies(u, []*http.Cookie{{Name: "session", Value: "1"}}) // 会话 cookie
	if got := j.Cookies(u); len(got) != 1 {
		t.Error("会话 cookie（无 Expires）不应过期")
	}
}

func TestJarDeleteByMaxAge(t *testing.T) {
	j := NewJar()
	u := mustURL(t, "https://example.com/")
	j.SetCookies(u, []*http.Cookie{{Name: "k", Value: "1"}})
	if j.Len() != 1 {
		t.Fatal("前置条件失败：cookie 未写入")
	}

	j.SetCookies(u, []*http.Cookie{{Name: "k", Value: "", MaxAge: -1}})
	if j.Len() != 0 {
		t.Errorf("MaxAge=-1 应删除 cookie，实际还有 %d 条", j.Len())
	}
}

func TestJarExportImportRoundTrip(t *testing.T) {
	j := NewJar()
	j.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{
		{Name: "a", Value: "1", Domain: ".example.com", Path: "/", HttpOnly: true, Secure: true},
	})

	data, err := j.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}

	j2 := NewJar()
	if err := j2.ImportJSON(data); err != nil {
		t.Fatal(err)
	}
	if j2.Len() != j.Len() {
		t.Fatalf("导入后数量不一致: %d vs %d", j2.Len(), j.Len())
	}

	got := j2.Cookies(mustURL(t, "https://example.com/"))
	if len(got) != 1 || got[0].Name != "a" || got[0].Value != "1" {
		t.Errorf("往返后 cookie 内容不符: %+v", got)
	}
}

// TestJarDomainWideSurvivesRoundTrip 守住一个保真度回归：域级 Cookie（Domain=.example.com）
// 经 Export→Import 往返后，仍必须能匹配子域。曾经 Load 恒设 hostOnly=true，
// 往返后域级 Cookie 退化成 host-only，子域再也拿不到它。
func TestJarDomainWideSurvivesRoundTrip(t *testing.T) {
	j := NewJar()
	j.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{
		{Name: "dom", Value: "1", Domain: ".example.com", Path: "/"},
		{Name: "host", Value: "2", Path: "/"}, // 未指定 Domain → host-only
	})

	data, err := j.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}
	j2 := NewJar()
	if err := j2.ImportJSON(data); err != nil {
		t.Fatal(err)
	}

	// 子域应拿到域级 Cookie，拿不到 host-only Cookie
	sub := j2.Cookies(mustURL(t, "https://a.example.com/"))
	if len(sub) != 1 || sub[0].Name != "dom" {
		t.Errorf("往返后子域应只拿到域级 cookie dom，实际 %+v", sub)
	}
	// 主域两条都应命中
	main := j2.Cookies(mustURL(t, "https://example.com/"))
	if len(main) != 2 {
		t.Errorf("往返后主域应命中 2 条，实际 %d: %+v", len(main), main)
	}
}

func TestJarSaveAndLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "cookies.json")

	j := NewJar()
	j.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{{Name: "sid", Value: "xyz"}})
	if err := j.SaveFile(path); err != nil {
		t.Fatal(err)
	}

	j2 := NewJar()
	if err := j2.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if j2.Len() != 1 {
		t.Fatalf("从文件恢复后应有 1 条，实际 %d", j2.Len())
	}

	// 文件不存在不应报错（首次运行是常态）
	if err := NewJar().LoadFile(filepath.Join(dir, "nope.json")); err != nil {
		t.Errorf("加载不存在的文件不应报错: %v", err)
	}
}

func TestJarConcurrentAccess(t *testing.T) {
	j := NewJar()
	u := mustURL(t, "https://example.com/")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				j.SetCookies(u, []*http.Cookie{{Name: "k", Value: "v"}})
				_ = j.Cookies(u)
				_ = j.All()
			}
		})
	}
	wg.Wait()
}

// ---------- Session ----------

func TestSessionGetAndHeaders(t *testing.T) {
	var gotUA, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCustom = r.Header.Get("X-Token")
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	s := New(WithHeader("X-Token", "t-123"))
	resp, err := s.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if !resp.OK() {
		t.Errorf("期望 2xx，实际 %d", resp.StatusCode)
	}
	if resp.Text() != "hello" {
		t.Errorf("响应体不符: %q", resp.Text())
	}
	if gotCustom != "t-123" {
		t.Errorf("默认请求头未生效: %q", gotCustom)
	}
	if !strings.Contains(gotUA, "Chrome") {
		t.Errorf("默认 UA 未生效: %q", gotUA)
	}
}

func TestSessionPostJSONAndForm(t *testing.T) {
	var body []byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		ctype = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := New()

	var out struct {
		OK bool `json:"ok"`
	}
	resp, err := s.PostJSON(context.Background(), srv.URL, map[string]string{"a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.JSON(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Error("JSON 解析结果不符")
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ctype)
	}
	if string(body) != `{"a":"1"}` {
		t.Errorf("请求体不符: %s", body)
	}

	if _, err := s.PostForm(context.Background(), srv.URL, url.Values{"k": {"v"}}); err != nil {
		t.Fatal(err)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("表单 Content-Type 不符: %q", ctype)
	}
	if string(body) != "k=v" {
		t.Errorf("表单请求体不符: %s", body)
	}
}

func TestSessionResponseJSONEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	resp, err := New().Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := resp.JSON(&v); err == nil {
		t.Error("空响应体解析 JSON 应报错")
	}
}

func TestSessionCookiesPersistAcrossRequests(t *testing.T) {
	var setCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s-1", Path: "/"})
			w.Write([]byte("ok"))
			return
		}
		setCookie = r.Header.Get("Cookie")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	s := New()
	if _, err := s.Get(context.Background(), srv.URL+"/login"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), srv.URL+"/data"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(setCookie, "sid=s-1") {
		t.Errorf("第二个请求未带上 cookie: %q", setCookie)
	}
}

func TestSessionSetAndSaveCookies(t *testing.T) {
	s := New()
	s.SetCookie(CookieItem{Name: "manual", Value: "v", Domain: "example.com", Path: "/"})

	if len(s.Cookies()) != 1 {
		t.Fatalf("SetCookie 未生效: %+v", s.Cookies())
	}

	path := filepath.Join(t.TempDir(), "c.json")
	if err := s.SaveCookies(path); err != nil {
		t.Fatal(err)
	}

	s2 := New()
	if err := s2.LoadCookies(path); err != nil {
		t.Fatal(err)
	}
	if len(s2.Cookies()) != 1 || s2.Cookies()[0].Name != "manual" {
		t.Errorf("从文件恢复 cookie 失败: %+v", s2.Cookies())
	}

	s2.ClearCookies()
	if len(s2.Cookies()) != 0 {
		t.Error("ClearCookies 未清空")
	}
}

// TestSessionCookieFileCompatibleWithBrowser 是与浏览器互通的契约测试：
// session 导出的 JSON 必须能被同样结构解析回来（字段名与 chromium.Cookie 对齐）。
func TestSessionCookieFileCompatibleWithBrowser(t *testing.T) {
	// 这是一段「浏览器导出的」cookies.json（字段与 chromium.Cookie 一致）
	browserJSON := `[{"name":"token","value":"abc","domain":".example.com","path":"/","http_only":true,"secure":true,"same_site":"Lax","expires":0}]`

	path := filepath.Join(t.TempDir(), "cookies.json")
	if err := os.WriteFile(path, []byte(browserJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New()
	if err := s.LoadCookies(path); err != nil {
		t.Fatal(err)
	}
	cookies := s.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("浏览器导出的 cookie 未加载: %+v", cookies)
	}
	if cookies[0].Name != "token" || cookies[0].Value != "abc" || !cookies[0].HTTPOnly {
		t.Errorf("字段映射不符: %+v", cookies[0])
	}
}

func TestSessionSetProxyValidation(t *testing.T) {
	s := New()
	if err := s.SetProxy("127.0.0.1:7890"); err == nil {
		t.Error("缺少协议头的代理地址应报错")
	}
	if err := s.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Errorf("合法代理地址不应报错: %v", err)
	}
	if err := s.SetProxy(""); err != nil {
		t.Errorf("传空串表示取消代理，不应报错: %v", err)
	}
}

func TestSessionContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte("late"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New().Get(ctx, srv.URL); err == nil {
		t.Error("ctx 已取消时请求应返回错误")
	}
}

func TestSessionInvalidURL(t *testing.T) {
	if _, err := New().Get(context.Background(), "://bad"); err == nil {
		t.Error("非法 URL 应报错")
	}
}

func TestSessionResponseSaveFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("file-content"))
	}))
	defer srv.Close()

	resp, err := New().Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out", "a.txt")
	if err := resp.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "file-content" {
		t.Errorf("落盘内容不符: %q", string(data))
	}
}

func TestResponseContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := New().Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ContentType(); got != "application/json" {
		t.Errorf("ContentType 应去掉参数部分，实际 %q", got)
	}
}
