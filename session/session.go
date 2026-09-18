package session

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Session 是带 Cookie 管理的 HTTP 会话，用于纯 HTTP 抓取。
//
// 需渲染 JS、过验证码时用 chromium 包；拿到接口与 Cookie 后批量取数据用本包。二者通过
// Cookie 文件打通：tab.ExportCookies 导出的文件可由 session.LoadCookies 直接载入。
//
// Session 并发安全，多个 goroutine 可共用一个实例。
type Session struct {
	client  *http.Client
	jar     *Jar
	headers http.Header

	// maxBodySize 是单次响应体的字节上限（默认 32MB），<= 0 表示不限。
	// 响应体整体读入内存，需此上限防止 OOM。
	maxBodySize int64

	mu sync.RWMutex
}

// Option 是 Session 的函数式配置项。
type Option func(*Session)

// WithTimeout 设置请求超时（默认 30s）。
func WithTimeout(d time.Duration) Option {
	return func(s *Session) {
		s.client.Timeout = d
	}
}

// WithProxy 设置 HTTP 代理，例如 "http://127.0.0.1:7890"。
//
// 格式非法时静默忽略（Option 无法返回 error）；需拿到错误请改用 SetProxy。
func WithProxy(proxy string) Option {
	return func(s *Session) {
		_ = s.SetProxy(proxy)
	}
}

// WithHeader 设置一个默认请求头，对该会话的每次请求生效。
func WithHeader(key, value string) Option {
	return func(s *Session) {
		s.SetHeader(key, value)
	}
}

// WithUserAgent 设置默认 User-Agent，默认模拟 Chrome。
func WithUserAgent(ua string) Option {
	return func(s *Session) {
		s.SetHeader("User-Agent", ua)
	}
}

// WithJar 复用一个已有的 Cookie 容器（例如从文件恢复出来的登录态）。
func WithJar(j *Jar) Option {
	return func(s *Session) {
		if j != nil {
			s.jar = j
			s.client.Jar = j
		}
	}
}

// WithMaxBodySize 限制单次响应体的字节上限，超限返回 ErrBodyTooLarge。
//
// 默认 32MB（见 defaultMaxBodySize）防止超大响应撑爆内存。传 <= 0 表示不限；
// 下载大文件请关掉上限并改用 Client() 流式处理。
func WithMaxBodySize(n int64) Option {
	return func(s *Session) {
		s.maxBodySize = n
	}
}

// WithTransport 完全自定义底层 Transport（代理、TLS、拨号超时等一并接管）。
func WithTransport(t http.RoundTripper) Option {
	return func(s *Session) {
		if t != nil {
			s.client.Transport = t
		}
	}
}

// WithInsecureTLS 跳过 TLS 证书校验，用于自签证书的内网站点。
//
// 只改 TLSClientConfig，其余 Transport 配置原样保留，故与 WithProxy 互不覆盖。
func WithInsecureTLS() Option {
	return func(s *Session) {
		s.mu.Lock()
		defer s.mu.Unlock()
		c := *s.client
		t := s.baseTransportLocked()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式要求
		c.Transport = t
		s.client = &c
	}
}

// WithNoRedirect 禁止自动跟随重定向，便于观察 302 的 Location。
func WithNoRedirect() Option {
	return func(s *Session) {
		s.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// defaultMaxBodySize 是新会话默认的响应体上限（32MB）。
const defaultMaxBodySize = 32 << 20

// New 创建一个会话。默认自带 Cookie 容器、30s 超时、32MB 响应体上限与 Chrome UA。
func New(opts ...Option) *Session {
	jar := NewJar()
	s := &Session{
		client: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
		jar:         jar,
		headers:     http.Header{},
		maxBodySize: defaultMaxBodySize,
	}
	s.SetHeader("User-Agent", defaultUserAgent)
	s.SetHeader("Accept", "*/*")

	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Jar 返回该会话的 Cookie 容器，可直接做导出/导入操作。
func (s *Session) Jar() *Jar { return s.jar }

// Client 返回底层 http.Client 的浅拷贝，便于接入已有 HTTP 生态代码。
//
// 本会话靠「复制整个 client 后替换指针」保证并发安全，因此只能交出拷贝而不能交出内部指针。
// 改拷贝不影响本会话（要修改请用 SetTimeout / SetProxy 等方法）；Jar 与 Transport 仍共享底层对象。
func (s *Session) Client() *http.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := *s.client
	return &c
}

// MaxBodySize 返回当前生效的响应体上限（字节），0 表示不限。
func (s *Session) MaxBodySize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxBodySize
}

// SetMaxBodySize 调整响应体上限，超限返回 ErrBodyTooLarge。传 <= 0 表示不限。
func (s *Session) SetMaxBodySize(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxBodySize = n
}

// SetHeader 设置一个默认请求头（对该会话后续所有请求生效）。
func (s *Session) SetHeader(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers.Set(key, value)
}

// SetHeaders 批量设置默认请求头。
func (s *Session) SetHeaders(h map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range h {
		s.headers.Set(k, v)
	}
}

// DelHeader 删除一个默认请求头。
func (s *Session) DelHeader(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers.Del(key)
}

// Headers 返回当前默认请求头的副本。
func (s *Session) Headers() http.Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headers.Clone()
}

// SetTimeout 调整请求超时。
//
// 并发安全：改副本后整体替换 client 指针，避免与进行中的 client.Do 形成数据竞争（见 Client）。
func (s *Session) SetTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *s.client
	c.Timeout = d
	s.client = &c
}

// baseTransportLocked 返回一份可安全改写的 *http.Transport。
//
// 不直接 new 裸 &http.Transport{}：零值 Transport 会关掉 HTTP/2、TLS 握手超时与连接池参数。
// 改为就地克隆：当前是 *http.Transport 则复制它（保留已设项），否则克隆 http.DefaultTransport。
// 两者都拿不到时（用户通过 WithTransport 装了自定义 RoundTripper）才退化为空 Transport。
// 必须在持有 s.mu 时调用。
func (s *Session) baseTransportLocked() *http.Transport {
	if t, ok := s.client.Transport.(*http.Transport); ok && t != nil {
		return t.Clone()
	}
	if t, ok := http.DefaultTransport.(*http.Transport); ok && t != nil {
		return t.Clone()
	}
	return &http.Transport{}
}

// SetProxy 设置代理（本次调用可拿到错误）。传空字符串表示取消代理，恢复为标准库默认 Transport。
//
// 代理写入克隆后的 Transport，不丢失已有 TLS 配置与默认参数。
func (s *Session) SetProxy(proxy string) error {
	proxy = strings.TrimSpace(proxy)
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *s.client
	if proxy == "" {
		c.Transport = nil // 置空回到 http.DefaultTransport
		s.client = &c
		return nil
	}

	u, err := url.Parse(proxy)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInvalidProxy, proxy, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%w: %q 缺少协议头或主机（应形如 http://127.0.0.1:7890）", ErrInvalidProxy, proxy)
	}

	t := s.baseTransportLocked()
	t.Proxy = http.ProxyURL(u)
	c.Transport = t
	s.client = &c
	return nil
}

// ---------- 请求方法 ----------

// Get 发起 GET 请求。
func (s *Session) Get(ctx context.Context, rawURL string) (*Response, error) {
	return s.Request(ctx, http.MethodGet, rawURL, nil)
}

// Post 发起 POST 请求，contentType 为空时按表单处理。
func (s *Session) Post(ctx context.Context, rawURL, contentType string, body io.Reader) (*Response, error) {
	if contentType == "" {
		contentType = "application/x-www-form-urlencoded"
	}
	return s.Request(ctx, http.MethodPost, rawURL, body, contentType)
}

// PostForm 提交表单（application/x-www-form-urlencoded）。
func (s *Session) PostForm(ctx context.Context, rawURL string, data url.Values) (*Response, error) {
	return s.Request(ctx, http.MethodPost, rawURL,
		strings.NewReader(data.Encode()),
		"application/x-www-form-urlencoded")
}

// PostJSON 提交 JSON，v 会被序列化后发送，并自动带上 Content-Type。
func (s *Session) PostJSON(ctx context.Context, rawURL string, v any) (*Response, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化 JSON 失败: %w", err)
	}
	return s.Request(ctx, http.MethodPost, rawURL, bytes.NewReader(data), "application/json")
}

// Request 是统一的请求入口。body 为 nil 时不带请求体，contentType 为空则不设置该头。
//
// URL 先做完整校验（非空、可解析、带 http/https 协议头、有主机名），不合法时返回
// ErrEmptyURL / ErrInvalidURL / ErrUnsupportedProtocol 供 errors.Is 判断。
func (s *Session) Request(ctx context.Context, method, rawURL string, body io.Reader, contentType ...string) (*Response, error) {
	if _, err := parseHTTPURL(rawURL); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidURL, rawURL, err)
	}
	if len(contentType) > 0 && contentType[0] != "" {
		req.Header.Set("Content-Type", contentType[0])
	}
	return s.Do(req)
}

// parseHTTPURL 校验并解析一个 HTTP(S) 地址。
// 抽成独立函数以供 Jar.CookiesFor 等「只解析地址、不发请求」的接口复用同一套规则。
func parseHTTPURL(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, ErrEmptyURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrInvalidURL, rawURL, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("%w: %q 缺少协议头（应形如 https://host/path）", ErrInvalidURL, rawURL)
	default:
		return nil, fmt.Errorf("%w: %q（本会话只支持 http / https）", ErrUnsupportedProtocol, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q 缺少主机名", ErrInvalidURL, rawURL)
	}
	return u, nil
}

// Do 执行一个已经构造好的 *http.Request，并套用会话的默认请求头。
// 想在请求级别覆盖默认头，先在 req.Header 里设好——默认头不会覆盖已有值。
func (s *Session) Do(req *http.Request) (*Response, error) {
	if req == nil {
		return nil, ErrNilRequest
	}

	s.mu.RLock()
	for k, vals := range s.headers {
		if req.Header.Get(k) == "" {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}
	}
	// 在同一把读锁内取 client 指针与响应体上限：随后在锁外发请求，不会与配置变更相互撞车。
	client := s.client
	maxBody := s.maxBodySize
	s.mu.RUnlock()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return newResponse(resp, maxBody)
}
