package session

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CookieItem 是一条可序列化的 Cookie。
//
// 字段名刻意与 chromium.Cookie 对齐，因此浏览器导出的 cookies.json 可以直接被
// Session 载入，反之 Session 导出的文件也能被浏览器导入——两套会话可以互相接力。
// Expires 是 Unix 时间戳（秒，可为小数）；<= 0 表示会话 Cookie（关闭即失效）。
type CookieItem struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"same_site"`
	Expires  float64 `json:"expires"`

	// Partitioned / PartitionKey 对应 CHIPS（Cookies Having Independent
	// Partitioned State）分区 Cookie。早期版本没有这两个字段，导出再导入会把
	// 分区属性抹掉——浏览器要么当普通 Cookie 收下（语义变了），要么直接拒收。
	// PartitionKey 是写入时顶层站点的 site（形如 "https://example.com"）。
	Partitioned  bool   `json:"partitioned,omitempty"`
	PartitionKey string `json:"partition_key,omitempty"`
}

// 本包只支持这几种 SameSite 取值，其余一律按「未指定」处理。
const (
	sameSiteNone   = "None"
	sameSiteLax    = "Lax"
	sameSiteStrict = "Strict"
)

// entry 是 jar 内部存储的 Cookie，额外记录 hostOnly 标志。
// hostOnly 为 true 时该 Cookie 只精确匹配该域名，不匹配子域（对应「未指定 Domain」的情况）。
type entry struct {
	CookieItem
	hostOnly bool
}

// expired 判断该 Cookie 是否已过期。Expires <= 0 视为会话 Cookie，永不过期。
func (e *entry) expired(now time.Time) bool {
	if e.Expires <= 0 {
		return false
	}
	return now.Unix() >= int64(e.Expires)
}

// Jar 是一个可导出、可复用的 Cookie 容器，实现了 http.CookieJar 接口。
//
// 为什么不直接用标准库 cookiejar：标准库的实现不提供任何导出/导入能力，
// 而爬虫场景最刚需的就是「浏览器登录完把 Cookie 交给 Session 批量抓」以及
// 「把登录态存盘下次接着用」。这里自己实现，把这两件事做成一等公民。
//
// 所有方法都是并发安全的。
type Jar struct {
	mu      sync.RWMutex
	entries map[string]map[string]*entry // domain -> key(name\x00path) -> entry
}

// NewJar 创建一个空的 Cookie 容器。
func NewJar() *Jar {
	return &Jar{entries: map[string]map[string]*entry{}}
}

// canonicalHost 规范化主机名：去掉端口与方括号，统一小写，并剥掉前导点。
//
// 端口必须交给标准库去剥：手写「含冒号就按端口截断」对 IPv6 是错的——
// 裸 "::1" 的冒号在位置 0，会被截成空串（Cookie 静默丢失），
// "[::1]:8080" 不以 "[" 结尾，方括号只剥掉前半个，残留 "::1]:8080"。
// net.SplitHostPort 只在「确实带端口」时才成功，上面两种形态都会返回 error 而原样保留。
func canonicalHost(host string) string {
	h := strings.TrimSpace(host)
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(h, "[]")
	h = strings.TrimPrefix(h, ".")
	return strings.ToLower(h)
}

// validCookieDomain 判断一条 Cookie 的 Domain 是否可以接受。
//
// host 是「写入来源主机」（响应路径下即请求 URL 的 host）。两道检查都源自
// RFC 6265 §5.3 的第 5/6 步：
//
//  1. 归属：域级 Cookie 的 Domain 必须是来源主机本身或其上级域，否则就是
//     「无关站点给别人的域投毒」；
//  2. 公共后缀：com / co.uk / github.io 这类纯后缀一律拒收，否则任意一个
//     .com 站点都能给所有 .com 域写 Cookie（跨站 Cookie 投毒）。
//
// 文件导入路径（Jar.Load）没有来源主机，传空串 host 即跳过第 1 道——
// 但第 2 道与来源无关，任何写入路径都不能放过。
//
// 抽成一个函数是为了让 Jar.SetCookies 与 Jar.Load 共用同一套规则：
// 这两条写入路径各自内联一份时，规则很容易只在其中一条上生效（历史缺陷 BUG-01）。
func validCookieDomain(host, domain string) bool {
	if domain == "" {
		return false
	}
	if host != "" && domain != host && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	return !isPublicSuffix(domain)
}

// SetCookies 实现 http.CookieJar：把响应带回的 Cookie 存入容器。
//
// 语义与浏览器一致：
//   - 未指定 Domain 的 Cookie 记为 hostOnly，只精确匹配来源域名；
//   - MaxAge < 0 或 Expires 已过，表示删除同名 Cookie；
//   - Path 缺省按来源路径的目录部分处理，兜底为 "/"。
func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if u == nil || len(cookies) == 0 {
		return
	}
	host := canonicalHost(u.Host)
	if host == "" {
		return
	}
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()

	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}

		// 浏览器拒收「SameSite=None 但没有 Secure」的 Cookie（RFC 6265bis）。
		// 照单全收会制造「本地能过、浏览器不行」的差异——同一条登录态在
		// Session 里可用、交给浏览器却消失，排查起来极难。这里与浏览器保持一致。
		if c.SameSite == http.SameSiteNoneMode && !c.Secure {
			continue
		}

		domain := host
		hostOnly := true
		if c.Domain != "" {
			domain = canonicalHost(c.Domain)
			hostOnly = false
			// 归属检查（须是来源域名本身或其上级域）与公共后缀检查都收敛在
			// validCookieDomain 内，Load 走同一个函数，避免两条写入路径的规则漂移。
			if !validCookieDomain(host, domain) {
				continue
			}
		}

		path := c.Path
		if path == "" || !strings.HasPrefix(path, "/") {
			path = defaultPath(u.Path)
		}

		key := c.Name + "\x00" + path

		// 删除语义：MaxAge<0，或 MaxAge==0 且 Expires 已过
		expires := float64(0)
		switch {
		case c.MaxAge < 0:
			delete(j.entries[domain], key)
			continue
		case c.MaxAge > 0:
			expires = float64(now.Add(time.Duration(c.MaxAge) * time.Second).Unix())
		case !c.Expires.IsZero():
			expires = float64(c.Expires.Unix())
		}

		if j.entries[domain] == nil {
			j.entries[domain] = map[string]*entry{}
		}
		j.entries[domain][key] = &entry{
			CookieItem: CookieItem{
				Name:         c.Name,
				Value:        c.Value,
				Domain:       domain,
				Path:         path,
				HTTPOnly:     c.HttpOnly,
				Secure:       c.Secure,
				SameSite:     sameSiteName(c.SameSite),
				Expires:      expires,
				Partitioned:  c.Partitioned,
				PartitionKey: partitionedKey(c.Partitioned, u),
			},
			hostOnly: hostOnly,
		}
	}
}

// partitionedKey 返回分区 Cookie（CHIPS）的 partition key，即「写入时顶层站点的 site」。
// 对响应写 Cookie 的场景，发请求的那个站点就是顶层站点，因此由请求 URL 推导。
// 非分区 Cookie 返回空串，让 JSON 里干脆不出现该字段。
func partitionedKey(partitioned bool, u *url.URL) string {
	if !partitioned || u == nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + canonicalHost(u.Host)
}

// Cookies 实现 http.CookieJar：返回该 URL 应当携带的 Cookie。
//
// 匹配规则：域名（hostOnly 精确匹配 / 否则匹配自身及子域）、路径前缀、
// 未过期、Secure 仅用于 https。过期项在返回前顺手清理。
func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	if u == nil {
		return nil
	}
	host := canonicalHost(u.Host)
	if host == "" {
		return nil
	}
	reqPath := u.Path
	if reqPath == "" {
		reqPath = "/"
	}
	https := strings.EqualFold(u.Scheme, "https")
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()

	var out []*http.Cookie
	for domain, sub := range j.entries {
		if !domainMatch(host, domain) {
			continue
		}
		for key, e := range sub {
			if e.hostOnly && domain != host {
				continue
			}
			if e.expired(now) {
				delete(sub, key) // 顺手清掉过期项，避免容器无限增长
				continue
			}
			if !pathMatch(reqPath, e.Path) {
				continue
			}
			if e.Secure && !https {
				continue
			}
			//nolint:gosec // G124：这里是把服务端下发的属性原样回放给 net/http，
			// 凭空补 Secure/HttpOnly/SameSite 会改变 Cookie 语义，是相反的错误。
			out = append(out, &http.Cookie{
				Name:        e.Name,
				Value:       e.Value,
				Path:        e.Path,
				Domain:      e.Domain,
				Expires:     expiresTime(e.Expires),
				Secure:      e.Secure,
				HttpOnly:    e.HTTPOnly,
				SameSite:    parseSameSite(e.SameSite),
				Partitioned: e.Partitioned,
			})
		}
	}

	// 稳定排序：路径长的优先，其次按名称，保证每次请求头一致，便于排查与断言
	sort.Slice(out, func(i, k int) bool {
		if len(out[i].Path) != len(out[k].Path) {
			return len(out[i].Path) > len(out[k].Path)
		}
		return out[i].Name < out[k].Name
	})
	return out
}

// All 返回容器内全部 Cookie（含已过期项），用于持久化。
//
// 域级 Cookie（hostOnly=false）导出时会在 Domain 前补回前导点（如 ".example.com"）。
// 前导点是 Netscape/Chrome 通用的「域级 Cookie」标记，也是唯一能跨 JSON 往返
// 携带 hostOnly 语义的载体（CookieItem 无独立的 hostOnly 字段）。不补点的话，
// Save→Load 后域级 Cookie 会退化成 host-only，丢失子域匹配能力。
//
// 存盘请用本方法（过期项要留着，否则会话 Cookie 的「失效时间」信息会在
// 一次存盘往返后丢失）；只是想知道「当前有哪些 Cookie」请用 Valid。
func (j *Jar) All() []CookieItem { return j.snapshot(false) }

// Valid 返回容器内当前仍然有效的 Cookie（已剔除过期项），即「此刻真正会发出去的」集合。
//
// 与 All 的区别只在于过期项：All 面向持久化，Valid 面向「现在这个会话的登录态是什么」。
// 调用方遍历它以构造请求头、做站点间搬运算时应当用本方法。
func (j *Jar) Valid() []CookieItem { return j.snapshot(true) }

func (j *Jar) snapshot(validOnly bool) []CookieItem {
	j.mu.RLock()
	defer j.mu.RUnlock()

	now := time.Now()
	var out []CookieItem
	for _, sub := range j.entries {
		for _, e := range sub {
			if validOnly && e.expired(now) {
				continue
			}
			out = append(out, exportItem(e))
		}
	}
	sortItems(out)
	return out
}

// CookiesFor 返回访问 rawURL 时实际会携带的 Cookie 快照（已按域名、路径、Secure、过期过滤）。
//
// 这是「把登录态交给浏览器」时该用的接口：全量导出会把站点 A 的 Cookie 也塞进站点的
// 浏览器里，而按目标 URL 过滤后只留下真正用得上的那几条，语义与一次真实请求完全一致。
// rawURL 非法时返回 ErrEmptyURL / ErrInvalidURL / ErrUnsupportedProtocol。
func (j *Jar) CookiesFor(rawURL string) ([]CookieItem, error) {
	u, err := parseHTTPURL(rawURL)
	if err != nil {
		return nil, err
	}
	host := canonicalHost(u.Host)
	if host == "" {
		return nil, fmt.Errorf("%w: %q 缺少主机名", ErrInvalidURL, rawURL)
	}
	reqPath := u.Path
	if reqPath == "" {
		reqPath = "/"
	}
	https := strings.EqualFold(u.Scheme, "https")
	now := time.Now()

	j.mu.RLock()
	defer j.mu.RUnlock()

	var out []CookieItem
	for domain, sub := range j.entries {
		if !domainMatch(host, domain) {
			continue
		}
		for _, e := range sub {
			if e.hostOnly && domain != host {
				continue
			}
			if e.expired(now) {
				continue
			}
			if !pathMatch(reqPath, e.Path) {
				continue
			}
			if e.Secure && !https {
				continue
			}
			out = append(out, exportItem(e))
		}
	}
	sortItems(out)
	return out, nil
}

// exportItem 把内部 entry 转成可序列化的 CookieItem。
// 域级 Cookie 会在 Domain 前补上前导点，以便跨 JSON 往返保住 hostOnly 语义。
func exportItem(e *entry) CookieItem {
	item := e.CookieItem
	if !e.hostOnly && !strings.HasPrefix(item.Domain, ".") {
		item.Domain = "." + item.Domain
	}
	return item
}

// sortItems 按域名、路径、名字排序，保证导出结果稳定可 diff。
func sortItems(items []CookieItem) {
	sort.Slice(items, func(i, k int) bool {
		if items[i].Domain != items[k].Domain {
			return items[i].Domain < items[k].Domain
		}
		if items[i].Path != items[k].Path {
			return items[i].Path < items[k].Path
		}
		return items[i].Name < items[k].Name
	})
}

// Load 批量写入 Cookie（覆盖同名同路径项），返回实际写入的条数。用于从文件恢复登录态。
//
// hostOnly 由 Domain 是否带前导点推导（与 All 互补，与浏览器导出格式一致）：
//   - ".example.com" → 域级 Cookie，匹配自身及子域（hostOnly=false）；
//   - "example.com"  → host-only Cookie，只精确匹配该主机（hostOnly=true）。
func (j *Jar) Load(items []CookieItem) int {
	j.mu.Lock()
	defer j.mu.Unlock()

	n := 0
	for _, it := range items {
		if it.Name == "" {
			continue
		}
		// 与 SetCookies 同一条规则：SameSite=None 必须带 Secure，否则浏览器会拒收。
		if strings.EqualFold(it.SameSite, sameSiteNone) && !it.Secure {
			continue
		}
		hostOnly := !strings.HasPrefix(strings.TrimSpace(it.Domain), ".")
		domain := canonicalHost(it.Domain)
		// 文件导入没有「来源主机」，传空 host 即跳过归属检查；公共后缀检查与来源
		// 无关，任何路径都不放过——否则一份来路不明的 cookies.json 里放一条
		// Domain=".com"，之后所有 .com 域请求都会带上它。
		if !validCookieDomain("", domain) {
			continue
		}
		// CHIPS 分区 Cookie 必须带 Secure（README「分区 Cookie」一节声明的组合为
		// Secure + SameSite=None + Partitioned）。缺 Secure 的条目浏览器会拒收，
		// 这里拦下，免得出现「导入报成功、登录态其实不完整」。
		if it.Partitioned && !it.Secure {
			continue
		}
		path := it.Path
		if path == "" {
			path = "/"
		}
		// 内部统一存规范化（无前导点）的域名，map key 与匹配都基于它；
		// hostOnly 单独记录，All 导出时再据此补回前导点。
		it.Domain = domain
		// SameSite 也做一次规范化，这样手写的 "none" / "strict" 与导出值
		// 在文件里长得一样，save→load→save 不会来回变。
		it.SameSite = sameSiteName(parseSameSite(it.SameSite))
		if j.entries[domain] == nil {
			j.entries[domain] = map[string]*entry{}
		}
		j.entries[domain][it.Name+"\x00"+path] = &entry{CookieItem: it, hostOnly: hostOnly}
		n++
	}
	return n
}

// Clear 清空容器。
func (j *Jar) Clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = map[string]map[string]*entry{}
}

// Len 返回当前保存的 Cookie 条数（含过期项）。
func (j *Jar) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()

	n := 0
	for _, sub := range j.entries {
		n += len(sub)
	}
	return n
}

// ---------- 辅助 ----------

// domainMatch 判断请求域名 host 是否落在 Cookie 的 domain 范围内。
func domainMatch(host, domain string) bool {
	if host == domain {
		return true
	}
	return strings.HasSuffix(host, "."+domain)
}

// pathMatch 按 RFC 6265 判断请求路径是否命中 Cookie 的 path。
func pathMatch(reqPath, cookiePath string) bool {
	if reqPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	if strings.HasSuffix(cookiePath, "/") {
		return true
	}
	// 必须正好落在目录边界上，/foo 不应匹配 /foobar
	return reqPath[len(cookiePath)] == '/'
}

// defaultPath 按请求路径推导 Cookie 的默认 path（取目录部分）。
func defaultPath(reqPath string) string {
	if reqPath == "" || reqPath[0] != '/' {
		return "/"
	}
	if i := strings.LastIndex(reqPath, "/"); i >= 0 {
		return reqPath[:i+1]
	}
	return "/"
}

// expiresTime 把 Unix 秒转成 time.Time；<=0（会话 Cookie）返回零值。
func expiresTime(sec float64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0)
}

func sameSiteName(s http.SameSite) string {
	switch s {
	case http.SameSiteLaxMode:
		return "Lax"
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return ""
	}
}

func parseSameSite(name string) http.SameSite {
	switch strings.ToLower(name) {
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteDefaultMode
	}
}
