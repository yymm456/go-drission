package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// regURL 是回归用例内部使用的地址解析包装，直接复用同包已有的 mustURL。
func regURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	return mustURL(t, raw)
}

// TestRequestURLValidation 覆盖 URL 校验：相对地址、空地址、非 http 协议
// 都必须在发起请求之前就被拦下，并返回可用 errors.Is 判断的哨兵错误。
func TestRequestURLValidation(t *testing.T) {
	s := New()

	cases := []struct {
		name string
		url  string
		want error
	}{
		{"空地址", "", ErrEmptyURL},
		{"纯空白", "   ", ErrEmptyURL},
		{"相对路径", "/relative/path", ErrInvalidURL},
		{"缺协议头", "example.com/a", ErrInvalidURL},
		{"缺主机名", "http:///path", ErrInvalidURL},
		{"不支持协议", "ftp://example.com/x", ErrUnsupportedProtocol},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Get(context.Background(), c.url)
			if !errors.Is(err, c.want) {
				t.Fatalf("url=%q 期望 %v，实际 %v", c.url, c.want, err)
			}
		})
	}
}

// TestProxyAndInsecureTLSOrderIndependent 守住「选项顺序敏感」这个坑：
// 早期 SetProxy 会 new 一个裸 Transport，把先设置的 InsecureSkipVerify 直接丢掉。
func TestProxyAndInsecureTLSOrderIndependent(t *testing.T) {
	orders := map[string]*Session{
		"先 TLS 后 代理": New(WithInsecureTLS(), WithProxy("http://127.0.0.1:7890")),
		"先 代理 后 TLS": New(WithProxy("http://127.0.0.1:7890"), WithInsecureTLS()),
	}
	for name, s := range orders {
		tr, ok := s.Client().Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: Transport 类型不是 *http.Transport", name)
		}
		if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
			t.Errorf("%s: InsecureSkipVerify 被覆盖丢失", name)
		}
		if tr.Proxy == nil {
			t.Fatalf("%s: 代理未生效", name)
		}
		u, err := tr.Proxy(&http.Request{URL: regURL(t, "http://example.com/")})
		if err != nil || u == nil || u.Host != "127.0.0.1:7890" {
			t.Errorf("%s: 代理地址不符：%v %v", name, u, err)
		}
	}
}

// TestProxyKeepsTransportDefaults 守住「设代理把默认参数一起丢掉」这个坑：
// 零值 http.Transport 的 ForceAttemptHTTP2 为 false、TLSHandshakeTimeout 为 0。
func TestProxyKeepsTransportDefaults(t *testing.T) {
	s := New(WithProxy("http://127.0.0.1:7890"))
	tr := s.Client().Transport.(*http.Transport)

	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 被置为 false，HTTPS 站点会静默退回 HTTP/1.1")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout 为 0，握手阶段可能长时间挂起")
	}
	if tr.DialContext == nil {
		t.Error("DialContext 为空，拨号阶段失去超时保护")
	}
}

// TestSetProxyInvalid 非法代理必须报错，且不改变原有 Transport 配置。
func TestSetProxyInvalid(t *testing.T) {
	s := New(WithInsecureTLS())
	before := s.Client().Transport

	for _, bad := range []string{"://bad", "127.0.0.1:7890/*", "http://[::1"} {
		if err := s.SetProxy(bad); !errors.Is(err, ErrInvalidProxy) {
			t.Errorf("SetProxy(%q) 期望 ErrInvalidProxy，实际 %v", bad, err)
		}
	}
	if s.Client().Transport != before {
		t.Error("非法代理不应改动 Transport")
	}

	// 合法值应当生效；空值应当恢复默认
	if err := s.SetProxy("http://127.0.0.1:7890"); err != nil {
		t.Fatalf("合法代理报错：%v", err)
	}
	if err := s.SetProxy(""); err != nil {
		t.Fatalf("清空代理报错：%v", err)
	}
	if s.Client().Transport != nil {
		t.Error("清空代理后应回到 http.DefaultTransport（Transport 为 nil）")
	}
}

// TestJarRejectsPublicSuffixCookie 守住跨站 Cookie 投毒：
// example.com 不得通过 Domain=com 给所有 .com 域写 Cookie。
func TestJarRejectsPublicSuffixCookie(t *testing.T) {
	j := NewJar()
	j.SetCookies(regURL(t, "https://evil.example.com/x"),
		[]*http.Cookie{{Name: "poison", Value: "1", Domain: "com", Path: "/"}})
	j.SetCookies(regURL(t, "https://evil.co.uk/x"),
		[]*http.Cookie{{Name: "poison", Value: "1", Domain: "co.uk", Path: "/"}})
	j.SetCookies(regURL(t, "https://x.github.io/y"),
		[]*http.Cookie{{Name: "poison", Value: "1", Domain: "github.io", Path: "/"}})

	if n := j.Len(); n != 0 {
		t.Fatalf("公共后缀 Cookie 应全部被拒，实际留下 %d 条", n)
	}
	if got := j.Cookies(regURL(t, "https://bank.com/")); len(got) != 0 {
		t.Fatalf("bank.com 不应收到任何 Cookie，实际 %v", got)
	}

	// 正常上级域仍然允许（app.example.com 给 example.com 写 Cookie 是合法的）
	j.SetCookies(regURL(t, "https://app.example.com/x"),
		[]*http.Cookie{{Name: "sid", Value: "v", Domain: "example.com", Path: "/"}})
	if n := j.Len(); n != 1 {
		t.Fatalf("合法域级 Cookie 应被接受，实际 %d 条", n)
	}
	// 但 IP 字面量不能当公共后缀误杀
	j.SetCookies(regURL(t, "http://127.0.0.1:8080/x"),
		[]*http.Cookie{{Name: "ip", Value: "v", Domain: "127.0.0.1", Path: "/"}})
	if n := j.Len(); n != 2 {
		t.Fatalf("IP 域 Cookie 应被接受，实际 %d 条", n)
	}
}

// TestJarCookiesForFiltering 验证按 URL 过滤导出：只给出该地址真正会携带的 Cookie，
// 且域级 Cookie 会补回前导点，保证 Save→Load 往返后子域匹配能力不丢。
func TestJarCookiesForFiltering(t *testing.T) {
	j := NewJar()
	j.SetCookies(regURL(t, "https://app.example.com/a/b"),
		[]*http.Cookie{
			{Name: "root", Value: "1", Path: "/"},
			{Name: "scoped", Value: "2", Path: "/a/b"},
			{Name: "other", Value: "3", Path: "/other"},
			{Name: "secureOnly", Value: "4", Path: "/", Secure: true},
		})

	items, err := j.CookiesFor("https://app.example.com/a/b/c")
	if err != nil {
		t.Fatalf("CookiesFor 失败：%v", err)
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	if !names["root"] || !names["scoped"] || !names["secureOnly"] {
		t.Errorf("应命中的 Cookie 缺失：%v", names)
	}
	if names["other"] {
		t.Error("路径 /other 的 Cookie 不应被 /a/b/c 命中")
	}

	// http 请求不应带上 Secure Cookie
	items, err = j.CookiesFor("http://app.example.com/a/b")
	if err != nil {
		t.Fatalf("CookiesFor 失败：%v", err)
	}
	for _, it := range items {
		if it.Name == "secureOnly" {
			t.Error("http 请求不应携带 Secure Cookie")
		}
	}

	// 无关站点应当为空
	items, err = j.CookiesFor("https://unrelated.com/")
	if err != nil {
		t.Fatalf("CookiesFor 失败：%v", err)
	}
	if len(items) != 0 {
		t.Errorf("无关站点不应命中任何 Cookie，实际 %v", items)
	}

	// 非法地址沿用同一套 URL 校验
	if _, err := j.CookiesFor("/relative"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("期望 ErrInvalidURL，实际 %v", err)
	}
}

// TestJarHostOnlyRoundTrip 验证全量导出→导入后 hostOnly 语义不丢。
func TestJarHostOnlyRoundTrip(t *testing.T) {
	j := NewJar()
	j.SetCookies(regURL(t, "https://app.example.com/"),
		[]*http.Cookie{
			{Name: "hostOnly", Value: "1", Path: "/"},                          // 未指定 Domain → host-only
			{Name: "domainWide", Value: "2", Domain: "example.com", Path: "/"}, // 域级
		})

	data, err := j.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON 失败：%v", err)
	}

	j2 := NewJar()
	if err := j2.ImportJSON(data); err != nil {
		t.Fatalf("ImportJSON 失败：%v", err)
	}
	if n := j2.Load(data2Items(t, data)); n != 2 {
		t.Fatalf("Load 应返回写入条数 2，实际 %d", n)
	}

	// 子域 app.example.com 仍应看到域级 Cookie，且看不到 host-only 的那条
	got := map[string]bool{}
	for _, c := range j2.Cookies(regURL(t, "https://sub.example.com/")) {
		got[c.Name] = true
	}
	if !got["domainWide"] {
		t.Error("往返后域级 Cookie 丢失了子域匹配能力")
	}
	if got["hostOnly"] {
		t.Error("host-only Cookie 不应匹配子域")
	}
}

// data2Items 解析 JSON 后交给 Load，用于校验 Load 的返回值。
func data2Items(t *testing.T, data []byte) []CookieItem {
	t.Helper()
	j := NewJar()
	if err := j.ImportJSON(data); err != nil {
		t.Fatalf("ImportJSON 失败：%v", err)
	}
	return j.All()
}

// TestResponseJSONEmptyBody 空响应体必须报 ErrEmptyBody，而不是把空串当合法 JSON。
func TestResponseJSONEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	resp, err := New().Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	var out map[string]any
	if err := resp.JSON(&out); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("期望 ErrEmptyBody，实际 %v", err)
	}
	if err := srv.Config.Shutdown(context.Background()); err != nil {
		t.Logf("shutdown: %v", err)
	}
}

// TestResponseInvalidJSON 非法 JSON 必须报 ErrInvalidJSON。
func TestResponseInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	resp, err := New().Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	var out map[string]any
	if err := resp.JSON(&out); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("期望 ErrInvalidJSON，实际 %v", err)
	}
}

// TestIsRetryable 分类应当稳定：调用方问题不重试，传输层问题可重试。
func TestIsRetryable(t *testing.T) {
	if IsRetryable(nil) {
		t.Error("nil 不应视为可重试")
	}
	for _, err := range []error{ErrInvalidURL, ErrEmptyURL, ErrInvalidProxy, ErrEmptyBody, ErrInvalidJSON} {
		if IsRetryable(err) {
			t.Errorf("%v 属于调用方问题，不应重试", err)
		}
	}
	if !IsRetryable(context.DeadlineExceeded) {
		t.Error("超时属于传输层问题，应可重试")
	}
}

// TestSaveFileForCreatesDir 按 URL 导出到不存在的目录时应自动建目录。
func TestSaveFileForCreatesDir(t *testing.T) {
	j := NewJar()
	j.SetCookies(regURL(t, "https://example.com/"),
		[]*http.Cookie{{Name: "sid", Value: "v", Path: "/"}})

	path := t.TempDir() + "/deep/nested/cookies.json"
	if err := j.SaveFileFor("https://example.com/", path); err != nil {
		t.Fatalf("SaveFileFor 失败：%v", err)
	}
	if err := j.LoadFile(path); err != nil {
		t.Fatalf("LoadFile 失败：%v", err)
	}
}

// TestSameSiteNoneWithoutSecureRejected 守住「浏览器拒收、本包却收下」的差异：
// RFC 6265bis 规定 SameSite=None 必须带 Secure，浏览器会整条丢弃。
// 本地容器若照单全收，就会出现「Session 里登录态可用、交给浏览器就没了」的怪事。
func TestSameSiteNoneWithoutSecureRejected(t *testing.T) {
	u := regURL(t, "https://example.com/")
	bad := []*http.Cookie{
		{Name: "bad", Value: "v", Path: "/", SameSite: http.SameSiteNoneMode, Secure: false},
	}

	j := NewJar()
	j.SetCookies(u, bad)
	if n := j.Len(); n != 0 {
		t.Fatalf("SameSite=None 且无 Secure 的 Cookie 应被拒收，实际容器内有 %d 条", n)
	}

	// SameSite=None + Secure 是合法的，不能被误杀
	j.SetCookies(u, []*http.Cookie{
		{Name: "good", Value: "v", Path: "/", SameSite: http.SameSiteNoneMode, Secure: true},
	})
	if n := j.Len(); n != 1 {
		t.Fatalf("SameSite=None + Secure 应当被接受，实际 %d 条", n)
	}

	// 显式导入这条规则同样生效（Load 走的是另一条路径）
	j2 := NewJar()
	if n := j2.Load([]CookieItem{{Name: "bad", Domain: "example.com", Path: "/", SameSite: "None"}}); n != 0 {
		t.Fatalf("Load 应跳过 SameSite=None 无 Secure 的项，实际写入 %d 条", n)
	}
	// 大小写不敏感
	if n := j2.Load([]CookieItem{{Name: "bad2", Domain: "example.com", Path: "/", SameSite: "none"}}); n != 0 {
		t.Fatalf("SameSite 取值应大小写不敏感，实际写入 %d 条", n)
	}
}

// TestPartitionedCookieRoundTrip 守住 CHIPS 分区属性：早期 CookieItem 没有这两个
// 字段，导出再导入会把分区属性抹掉——浏览器要么当普通 Cookie 收下（语义变了），
// 要么直接拒收。
func TestPartitionedCookieRoundTrip(t *testing.T) {
	u := regURL(t, "https://example.com/")
	j := NewJar()
	j.SetCookies(u, []*http.Cookie{
		{Name: "chips", Value: "v", Path: "/", Secure: true, Partitioned: true},
	})

	items := j.All()
	if len(items) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条", len(items))
	}
	if !items[0].Partitioned {
		t.Fatal("Partitioned 属性在导出后丢失")
	}
	if want := "https://example.com"; items[0].PartitionKey != want {
		t.Fatalf("PartitionKey = %q，期望 %q", items[0].PartitionKey, want)
	}

	// 走一遍 JSON 往返，模拟「存盘 → 下次启动载入」
	data, err := j.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON 失败：%v", err)
	}
	j2 := NewJar()
	if err := j2.ImportJSON(data); err != nil {
		t.Fatalf("ImportJSON 失败：%v", err)
	}
	back := j2.All()
	if len(back) != 1 || !back[0].Partitioned || back[0].PartitionKey != "https://example.com" {
		t.Fatalf("分区属性未能在 JSON 往返后保留：%+v", back)
	}

	// 非分区 Cookie 不应凭空多出 partition_key 字段
	j3 := NewJar()
	j3.SetCookies(u, []*http.Cookie{{Name: "plain", Value: "v", Path: "/"}})
	if got := j3.All()[0]; got.Partitioned || got.PartitionKey != "" {
		t.Fatalf("普通 Cookie 不应带分区属性：%+v", got)
	}
}

// TestMaxBodySize 覆盖响应体上限：Response 会把 body 整体读进内存，
// 没有闸门时对端一个误配的大响应就能把进程撑爆。
func TestMaxBodySize(t *testing.T) {
	const bodySize = 64 * 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, bodySize))
	}))
	defer srv.Close()

	ctx := context.Background()

	// 默认 32MB（见 defaultMaxBodySize）：64KB 的响应远未触顶，应完整读回
	s := New()
	if s.MaxBodySize() != defaultMaxBodySize {
		t.Fatalf("默认上限应为 %d（32MB），实际 %d", defaultMaxBodySize, s.MaxBodySize())
	}
	resp, err := s.Get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("未触顶时不应报错：%v", err)
	}
	if len(resp.Bytes()) != bodySize {
		t.Fatalf("body 长度 = %d，期望 %d", len(resp.Bytes()), bodySize)
	}

	// 超限：返回 ErrBodyTooLarge
	small := New(WithMaxBodySize(1024))
	if _, err := small.Get(ctx, srv.URL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("超限应返回 ErrBodyTooLarge，实际 %v", err)
	}

	// 刚好放得下时不误报
	exact := New(WithMaxBodySize(bodySize))
	if _, err := exact.Get(ctx, srv.URL); err != nil {
		t.Fatalf("恰好等于上限时不应报错：%v", err)
	}

	// 运行期调整同样生效
	small.SetMaxBodySize(0)
	if _, err := small.Get(ctx, srv.URL); err != nil {
		t.Fatalf("SetMaxBodySize(0) 后不应再报错：%v", err)
	}
}

// TestSessionCookiesExcludesExpired 区分「当前有效」与「全量镜像」：
// Cookies 面向「现在这个会话的登录态」，不该把过期项也算进去。
func TestSessionCookiesExcludesExpired(t *testing.T) {
	s := New()
	u := regURL(t, "https://example.com/")
	s.jar.SetCookies(u, []*http.Cookie{
		{Name: "live", Value: "1", Path: "/"},
		{Name: "dead", Value: "2", Path: "/", Expires: time.Now().Add(-time.Hour)},
	})

	if n := len(s.Cookies()); n != 1 {
		t.Fatalf("Cookies 应只返回未过期项，实际 %d 条", n)
	}
	if n := len(s.AllCookies()); n != 2 {
		t.Fatalf("AllCookies 应返回全部（含过期），实际 %d 条", n)
	}
	if got := s.Cookies()[0].Name; got != "live" {
		t.Fatalf("应返回 live，实际 %q", got)
	}
}

// TestClientReturnsCopy 守住可变逃逸口：Client 返回的是拷贝，
// 调用方改它不应影响会话自身的配置（需要改请走 SetTimeout / SetProxy）。
func TestClientReturnsCopy(t *testing.T) {
	s := New(WithTimeout(5 * time.Second))

	c := s.Client()
	c.Timeout = time.Hour // 改拷贝
	if got := s.Client().Timeout; got != 5*time.Second {
		t.Fatalf("改动拷贝污染了会话配置：Timeout = %v", got)
	}
}

// TestDefaultMaxBodySizeMatchesDoc 守住「文档承诺默认 32MB、实现却是 0（不限）」这个缺口。
//
// README 四处与 CODE_REVIEW 都写着默认 32MB，而 session.New 曾漏掉默认值赋值：
// maxBodySize 落到零值 0，而 newResponse 只在 maxBody > 0 时才限流——承诺的 OOM 保护
// 实际并不存在。这里既钉住默认值，也端到端确认默认会话真的会拒绝超限响应。
func TestDefaultMaxBodySizeMatchesDoc(t *testing.T) {
	if got := New().MaxBodySize(); got != defaultMaxBodySize {
		t.Fatalf("默认上限 = %d，期望 %d（32MB）", got, defaultMaxBodySize)
	}

	// 分块写出「略多于默认上限」的正文，避免在测试里额外备一份 32MB 缓冲。
	const chunk = 256 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, chunk)
		for written := int64(0); written <= defaultMaxBodySize; written += chunk {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	if _, err := New().Get(context.Background(), srv.URL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("默认会话读到超过 32MB 的响应应报 ErrBodyTooLarge，实际 %v", err)
	}
}

// TestJarLoadRejectsPublicSuffixCookie 守住跨站 Cookie 投毒的第二条入口。
//
// 公共后缀校验早期只内联在 Jar.SetCookies（响应路径）里，Jar.Load（文件导入路径）
// 完全没有：一份来路不明的 cookies.json 里放一条 Domain=".com"，之后所有 .com 域
// 的请求都会带上它。这里把两条写入路径一起钉住。
func TestJarLoadRejectsPublicSuffixCookie(t *testing.T) {
	poison := []CookieItem{
		{Name: "p1", Value: "1", Domain: ".com", Path: "/"},
		{Name: "p2", Value: "1", Domain: ".org", Path: "/"},
		{Name: "p3", Value: "1", Domain: ".co.uk", Path: "/"},
		{Name: "p4", Value: "1", Domain: "github.io", Path: "/"},
		{Name: "p5", Value: "1", Domain: "com", Path: "/"},
	}

	j := NewJar()
	if n := j.Load(poison); n != 0 {
		t.Fatalf("Load 应拒收全部公共后缀 Cookie，实际写入 %d 条", n)
	}
	if n := j.Len(); n != 0 {
		t.Fatalf("容器内应为空，实际 %d 条", n)
	}
	if got := j.Cookies(regURL(t, "https://bank.com/")); len(got) != 0 {
		t.Fatalf("bank.com 不应收到被投毒的 Cookie，实际 %v", got)
	}

	// 同一批数据经 JSON 导入同样要被拦下
	data, err := json.Marshal(poison)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	j2 := NewJar()
	if err := j2.ImportJSON(data); err != nil {
		t.Fatalf("ImportJSON 失败：%v", err)
	}
	if n := j2.Len(); n != 0 {
		t.Fatalf("ImportJSON 应拒收公共后缀 Cookie，实际 %d 条", n)
	}

	// 文件入口（Session.LoadCookies 走的就是它）
	path := t.TempDir() + "/poison.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写临时 Cookie 文件失败：%v", err)
	}
	j3 := NewJar()
	if err := j3.LoadFile(path); err != nil {
		t.Fatalf("LoadFile 失败：%v", err)
	}
	if n := j3.Len(); n != 0 {
		t.Fatalf("LoadFile 应拒收公共后缀 Cookie，实际 %d 条", n)
	}

	// Session 级便捷入口
	s := New()
	s.SetCookie(CookieItem{Name: "p", Value: "1", Domain: ".com", Path: "/"})
	if n := s.SetCookies(poison); n != 0 {
		t.Fatalf("Session.SetCookies 应拒收公共后缀 Cookie，实际写入 %d 条", n)
	}
	if n := len(s.AllCookies()); n != 0 {
		t.Fatalf("会话内应为空，实际 %d 条", n)
	}

	// 合法的上级域不能被误杀
	if n := NewJar().Load([]CookieItem{
		{Name: "sid", Value: "v", Domain: ".example.com", Path: "/"},
	}); n != 1 {
		t.Fatalf("合法域级 Cookie 应被接受，实际 %d 条", n)
	}
	// IP 字面量不能被公共后缀表误杀
	if n := NewJar().Load([]CookieItem{
		{Name: "ip", Value: "v", Domain: "127.0.0.1", Path: "/"},
	}); n != 1 {
		t.Fatalf("IP 域 Cookie 应被接受，实际 %d 条", n)
	}

	// 对照：响应路径仍然拒绝（确认既有行为没被改坏）
	j4 := NewJar()
	j4.SetCookies(regURL(t, "https://evil.example.com/x"),
		[]*http.Cookie{{Name: "p", Value: "1", Domain: "com", Path: "/"}})
	if n := j4.Len(); n != 0 {
		t.Fatalf("对照失效：SetCookies 应拒收 Domain=com，实际 %d 条", n)
	}
}

// TestJarLoadRejectsPartitionedWithoutSecure 守住 CHIPS 分区 Cookie 的导入校验。
//
// README 写明分区 Cookie 的组合是 Secure + SameSite=None + Partitioned，而 Load
// 此前完全不看 Partitioned：缺 Secure 的条目浏览器会拒收，于是出现「导入报成功、
// 登录态其实不完整」。
func TestJarLoadRejectsPartitionedWithoutSecure(t *testing.T) {
	j := NewJar()
	if n := j.Load([]CookieItem{
		{Name: "chips", Value: "v", Domain: ".example.com", Path: "/", Partitioned: true},
	}); n != 0 {
		t.Fatalf("Partitioned 且缺 Secure 应被拒收，实际写入 %d 条", n)
	}
	// Secure + Partitioned 是合法组合，不能被误杀
	if n := j.Load([]CookieItem{
		{Name: "chips", Value: "v", Domain: ".example.com", Path: "/", Secure: true, Partitioned: true},
	}); n != 1 {
		t.Fatalf("Secure + Partitioned 应被接受，实际写入 %d 条", n)
	}
}

// TestCanonicalHostHandlesIPv6 守住 IPv6 主机的 Cookie 域名解析。
//
// 早期 canonicalHost 手写「含冒号就按端口截断」：裸 "::1" 被截成空串（Cookie 被
// 静默丢弃），"[::1]:8080" 只剥掉前半个方括号、残留 "::1]:8080"。
// 改用 net.SplitHostPort 后两种形态都要正确，IPv4 / 域名带端口的旧行为也不能变。
func TestCanonicalHostHandlesIPv6(t *testing.T) {
	cases := map[string]string{
		"[::1]:8080":       "::1",
		"::1":              "::1",
		"[2001:db8::1]":    "2001:db8::1",
		"example.com:8080": "example.com",
		"Example.COM":      "example.com",
		".example.com":     "example.com",
		"127.0.0.1:8080":   "127.0.0.1",
	}
	for in, want := range cases {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q，期望 %q", in, got, want)
		}
	}

	// 端到端：IPv6 主机写入的 Cookie，Domain 不该带端口或残留方括号
	j := NewJar()
	j.SetCookies(regURL(t, "http://[::1]:8080/path"),
		[]*http.Cookie{{Name: "sid", Value: "1", Path: "/"}})
	items := j.All()
	if len(items) != 1 {
		t.Fatalf("IPv6 主机应写入 1 条，实际 %d 条", len(items))
	}
	if items[0].Domain != "::1" {
		t.Fatalf("Cookie Domain = %q，期望 ::1（不含端口与方括号）", items[0].Domain)
	}

	// 导入裸 IPv6 域名不能再被静默丢弃，且要能被 IPv6 回环命中
	j2 := NewJar()
	if n := j2.Load([]CookieItem{{Name: "sid", Value: "1", Domain: "::1", Path: "/"}}); n != 1 {
		t.Fatalf("Load(Domain=\"::1\") 应写入 1 条，实际 %d 条", n)
	}
	if got := j2.Cookies(regURL(t, "http://[::1]:8080/")); len(got) != 1 {
		t.Fatalf("IPv6 回环应命中该 Cookie，实际 %d 条", len(got))
	}
}

// TestMaxBodySizeMaxInt64NoOverflow 守住「上限写成 math.MaxInt64 时正文被整个吞掉」。
//
// 早期用 io.LimitReader(body, maxBody+1) 判超限：maxBody 为 MaxInt64 时 +1 回绕成
// 负数，LimitReader 遇负数立即返回 EOF——正文读成空，且因为 len(body) 不大于 maxBody，
// 连错误都不报。修复后改成「读满上限再单独探一个字节」，这里连带把边界钉住。
func TestMaxBodySizeMaxInt64NoOverflow(t *testing.T) {
	const body = "hello-body"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	resp, err := New(WithMaxBodySize(math.MaxInt64)).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("MaxInt64 上限下请求失败：%v", err)
	}
	if got := resp.Text(); got != body {
		t.Fatalf("正文 = %q，期望 %q（读成空说明 maxBody+1 溢出了）", got, body)
	}

	// 边界：恰好等于上限通过，多 1 字节即报 ErrBodyTooLarge
	ten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
	}))
	defer ten.Close()

	if _, err := New(WithMaxBodySize(10)).Get(context.Background(), ten.URL); err != nil {
		t.Fatalf("恰好等于上限时不应报错：%v", err)
	}
	if _, err := New(WithMaxBodySize(9)).Get(context.Background(), ten.URL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("超出上限 1 字节应报 ErrBodyTooLarge，实际 %v", err)
	}
}
