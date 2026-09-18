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

// Session 是一个带 Cookie 管理的 HTTP 会话，用于纯 HTTP 抓包场景。
//
// 与浏览器模式的分工：
//   - 需要渲染 JS、过滑块、点按钮时，用 chromium 包；
//   - 拿到接口与 Cookie 后批量取数据，用本包——没有浏览器开销，快一个数量级。
//
// 两者通过 Cookie 文件打通：浏览器 tab.ExportCookies 导出的文件，
// 可以直接 session.LoadCookies 载入，登录态无缝接力。
//
// Session 是并发安全的，多个 goroutine 共用一个实例即可（底层 http.Client 本就支持复用连接）。
type Session struct {
	client  *http.Client
	jar     *Jar
	headers http.Header

	// maxBodySize 是单次响应体的字节上限（默认 32MB，见 defaultMaxBodySize），
	// <= 0 表示不限。响应体要整体读进内存（Text/JSON 可反复读取的前提），
	// 所以需要一道闸。
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
// 格式非法时 Option 无法返回 error，因此非法值会被静默忽略（保持原 Transport 不变）；
// 需要拿到错误请改用 SetProxy。同时设置 WithProxy 与 WithInsecureTLS 时，
// 两者互不覆盖、与书写顺序无关。
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
// Response 会把响应体整体读进内存，没有上限时一个误配成 Content-Length: 8G
// 的接口足以把进程撑爆，因此默认即设 32MB（见 defaultMaxBodySize）。
// 传 <= 0 表示不限；需要下载大文件请关掉上限并改用 Client() 自己流式处理。
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

// WithInsecureTLS 跳过 TLS 证书校验，用于抓自签证书的内网站点。
//
// 实现上只改 TLSClientConfig，其余 Transport 配置（代理、拨号超时、HTTP/2、
// 连接池）原样保留，因此与 WithProxy 同时使用时无论书写顺序如何都不会互相覆盖。
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
		// 禁止跟随重定向只需返回哨兵错误，两个入参都用不到
		s.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// defaultMaxBodySize 是新会话默认的响应体上限（32MB）。
//
// README 与 CODE_REVIEW 都声明「默认 32MB」，早期实现却漏了赋值：maxBodySize 落到零值 0，
// 而 newResponse 只在 maxBody > 0 时才限流——文档承诺的 OOM 保护实际并不存在。
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

// Client 返回底层 http.Client 的一个浅拷贝，便于接入已有的 HTTP 生态代码。
//
// 刻意返回拷贝：本会话的时间/代理/重定向等配置靠「复制-替换整个 client 指针」
// 来保证并发安全，把内部指针交出去后，调用方一改就绕过了那层保护
// （Headers 同样是返回副本，这里与它保持一致）。
// 因此改拷贝不会影响本会话——需要改请用 SetTimeout / SetProxy 等方法。
// 注意 Jar 与 Transport 仍是共享的底层对象。
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
// 并发安全：不原地修改可能正被请求使用的 client，而是复制一份改好后原子替换指针，
// 避免与进行中的 client.Do 形成数据竞争（复制仅浅拷贝，Jar/Transport 等指针照旧共享）。
func (s *Session) SetTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *s.client
	c.Timeout = d
	s.client = &c
}

// baseTransportLocked 返回一份「可以安全改写」的 *http.Transport。
//
// 直接 new 一个裸 &http.Transport{} 是个隐蔽的坑：零值 Transport 的
// ForceAttemptHTTP2 为 false、TLSHandshakeTimeout 为 0、连接池参数也全是零值，
// 于是设置代理会顺手把 HTTP/2 支持、TLS 握手超时一起关掉，且不会有任何报错。
// 这里改为「就地克隆」：当前已是 *http.Transport 就复制它（保留用户已设的项），
// 否则克隆 http.DefaultTransport（继承标准库的合理默认值）。
//
// 两者都拿不到时（用户通过 WithTransport 装了自定义 RoundTripper）才退化为空 Transport。
// 该方法必须在持有 s.mu 时调用。
func (s *Session) baseTransportLocked() *http.Transport {
	if t, ok := s.client.Transport.(*http.Transport); ok && t != nil {
		return t.Clone()
	}
	if t, ok := http.DefaultTransport.(*http.Transport); ok && t != nil {
		return t.Clone()
	}
	return &http.Transport{}
}

// SetProxy 设置代理（本次调用可拿到错误，便于调用方处理）。
// 传空字符串表示取消代理，恢复为标准库默认 Transport。
//
// 同 SetTimeout：复制-替换整个 client，保证与进行中的请求互不干扰；
// 代理会写入一份克隆后的 Transport，不会丢掉已有的 TLS 配置与默认参数。
func (s *Session) SetProxy(proxy string) error {
	proxy = strings.TrimSpace(proxy)
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *s.client
	if proxy == "" {
		// 置空表示回到 http.DefaultTransport（nil Transport 的标准语义）
		c.Transport = nil
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
// URL 会先做一次完整校验（非空、可解析、带 http/https 协议头、有主机名），
// 不合法时返回 ErrEmptyURL / ErrInvalidURL / ErrUnsupportedProtocol 供 errors.Is 判断，
// 而不是把问题丢给底层让调用方去猜 "unsupported protocol scheme" 是哪里写错了。
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
// 抽成独立函数是为了让 Jar.CookiesFor 之类「只想解析地址、不发请求」的接口复用同一套规则。
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
	// 在同一把读锁内取 client 指针与响应体上限：SetTimeout/SetProxy/SetMaxBodySize
	// 都采用复制-替换或普通赋值，这里拿到的是当时的快照，
	// 随后在锁外发请求，不会与配置变更相互撕扯。
	client := s.client
	maxBody := s.maxBodySize
	s.mu.RUnlock()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return newResponse(resp, maxBody)
}
