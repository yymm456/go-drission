// Package cookie 是 Cookie 的纯逻辑层：数据结构、JSON 解析、注入前校验，
// 以及「Cookie -> CDP 参数」的字段映射。
//
// 本包不感知 Tab / Browser，也不发起任何 CDP 调用——因此可以脱离浏览器单独单测。
// 真正把参数送进某个标签页的动作由 page 包负责。
package cookie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/yymm456/go-drission/chromium/errs"
)

// Cookie 描述一个要注入的 Cookie。
//
// JSON 字段与 session.CookieItem 完全一致（这是两条会话能互相接力的前提，详见门面文档）。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"same_site"`
	Expires  float64 `json:"expires"`

	// Partitioned / PartitionKey 对应 CHIPS（Cookies Having Independent Partitioned State）
	// 分区 Cookie，json tag 与 session.CookieItem 逐字一致 —— 那是两个包接力时的唯一通道，
	// 任何一边少一个字段，导出的 JSON 再导入时该属性就会被静默丢掉（Go 会忽略未知字段），
	// Cookie 退化成普通 Cookie 而调用方毫不知情。
	//
	// PartitionKey 是写入时顶层站点的 site（形如 "https://example.com"），对应 CDP 的
	// CookiePartitionKey.TopLevelSite。
	Partitioned  bool   `json:"partitioned,omitempty"`
	PartitionKey string `json:"partition_key,omitempty"`
}

// CookieSource 是「能导出自己的 Cookie JSON」的类型。
//
// session.Jar 与 *session.Jar 天然满足该接口，因此可把登录态从 HTTP 侧接力给浏览器，
// 而 chromium 包无需反向依赖 session 包（完整示例见门面 chromium.CookieSource 的文档）。
type CookieSource interface {
	ExportJSON() ([]byte, error)
}

// ParseCookiesJSON 解析 Cookie JSON，公开文档见门面 chromium。
// 顶层必须是数组；空内容返回空切片而不是错误。
func ParseCookiesJSON(data []byte) ([]Cookie, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var out []Cookie
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", errs.ErrInvalidCookieJSON, err)
	}
	return out, nil
}

// ValidateCookie 在发起 CDP 调用之前拦住不完整的 Cookie。
//
// CDP 对缺字段只回一句 "Invalid cookie fields"，看不出是哪一条、缺了哪个字段，批量注入时尤其难查。
// 四项检查与 errs 中 ErrInvalidCookie 一一对应：缺 name、缺 domain、
// SameSite=None 却没有 Secure、标记分区却没有 PartitionKey。
func ValidateCookie(c Cookie, idx int) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: 第 %d 条缺少 name", errs.ErrInvalidCookie, idx)
	}
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("%w: Cookie %q 缺少 domain（CDP 需要 domain 或 url 才能落盘）",
			errs.ErrInvalidCookie, c.Name)
	}
	// Chrome 依 RFC 6265bis 直接丢弃「SameSite=None 但没有 Secure」的 Cookie，但 Network.setCookie
	// 本身不报错——若不拦，调用方会拿到「全部注入成功」的假象而登录态实际缺了几条。
	if strings.EqualFold(strings.TrimSpace(c.SameSite), "None") && !c.Secure {
		return fmt.Errorf("%w: Cookie %q 的 SameSite=None 必须带 Secure（浏览器会拒收）",
			errs.ErrInvalidCookie, c.Name)
	}
	// 同理：分区标记了但没给 key，CDP 会当普通 Cookie 收下 —— 语义变了却不报错，必须拦。
	if c.Partitioned && strings.TrimSpace(c.PartitionKey) == "" {
		return fmt.Errorf("%w: Cookie %q 标记为分区（Partitioned）但缺少 PartitionKey（形如 \"https://example.com\"）",
			errs.ErrInvalidCookie, c.Name)
	}
	return nil
}

// cookiePartitionKey 把分区信息转成 CDP 的 CookiePartitionKey；非分区 Cookie 返回 nil。
//
// 必须原样返回 nil：CDP 的语义是「不设 partitionKey 即为普通 Cookie」，
// 给非分区 Cookie 塞一个空结构体会把它错误地变成分区 Cookie。
func cookiePartitionKey(c Cookie) *network.CookiePartitionKey {
	if !c.Partitioned || c.PartitionKey == "" {
		return nil
	}
	return &network.CookiePartitionKey{TopLevelSite: c.PartitionKey}
}

// cookiePath 返回注入用的 Cookie path，缺省为 "/"（与浏览器默认一致）。
func cookiePath(c Cookie) string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}

// cookieExpires 把 Cookie.Expires（Unix 秒）转成 CDP 时间戳；<= 0 表示会话 Cookie，返回 nil。
func cookieExpires(c Cookie) *cdp.TimeSinceEpoch {
	if c.Expires <= 0 {
		return nil
	}
	exp := cdp.TimeSinceEpoch(time.Unix(int64(c.Expires), 0))
	return &exp
}

// SetCookieParams 把 Cookie 转换成 CDP 的 SetCookieParams（单个设置用）。
//
// path / Expires 的归一化与批量路径共用 cookiePath / cookieExpires，避免两条注入路径字段映射漂移。
func SetCookieParams(c Cookie) *network.SetCookieParams {
	p := network.SetCookie(c.Name, c.Value).
		WithDomain(c.Domain).
		WithPath(cookiePath(c))

	if c.HTTPOnly {
		p = p.WithHTTPOnly(true)
	}
	if c.Secure {
		p = p.WithSecure(true)
	}
	if c.SameSite != "" {
		p = p.WithSameSite(network.CookieSameSite(c.SameSite))
	}
	if exp := cookieExpires(c); exp != nil {
		p = p.WithExpires(exp)
	}
	// 该字段没有 WithXxx builder，直接赋值。
	if pk := cookiePartitionKey(c); pk != nil {
		p.PartitionKey = pk
	}
	return p
}

// CookieParam 把 Cookie 转换成 CDP 的 CookieParam（批量设置用）。
//
// path / Expires 与单个设置路径共用 cookiePath / cookieExpires，保证两条注入路径一致。
func CookieParam(c Cookie) *network.CookieParam {
	p := &network.CookieParam{
		Name:     c.Name,
		Value:    c.Value,
		Domain:   c.Domain,
		Path:     cookiePath(c),
		HTTPOnly: c.HTTPOnly,
		Secure:   c.Secure,
		Expires:  cookieExpires(c),
	}

	if c.SameSite != "" {
		p.SameSite = network.CookieSameSite(c.SameSite)
	}
	p.PartitionKey = cookiePartitionKey(c)
	return p
}
