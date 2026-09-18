// Package cookie 是 Cookie 的纯逻辑层：数据结构、JSON 解析、注入前校验，
// 以及「Cookie -> CDP 参数」的字段映射。
//
// 本包不感知 Tab / Browser，也不发起任何 CDP 调用——因此可以脱离浏览器单独单测。
// 真正把参数送进某个标签页的动作留在 chromium 侧（S5 之后移到 page）。
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
// JSON 字段与 session.CookieItem 完全一致，因此两侧导出的 cookies.json
// 可以互相导入——这正是「Session 抓包拿 Cookie → 浏览器免登录」的通道。
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"same_site"`
	Expires  float64 `json:"expires"`
}

// CookieSource 是「能导出自己的 Cookie JSON」的类型。
//
// session.Jar 与 *session.Jar 天然满足该接口（ExportJSON() ([]byte, error)），
// 因此可以从浏览器侧这样接力，而 chromium 包无需反向依赖 session 包：
//
//	s := session.New()
//	s.PostForm(ctx, loginURL, form)          // 纯 HTTP 登录，拿到 Cookie
//	tab.LoginWithCookies(ctx, homeURL, s.Jar()) // 把登录态交给浏览器，免登录
type CookieSource interface {
	ExportJSON() ([]byte, error)
}

// ParseCookiesJSON 解析 Cookie JSON（支持 chrome 扩展导出 / 本包与 session 包导出的格式）。
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
// CDP 对缺字段只会回一句 "Invalid cookie fields"，看不出是哪一条、
// 缺了哪个字段；批量注入几十条时尤其难查。这里提前指出来。
//
// 三项检查与 errs 中 ErrInvalidCookie 的注释一一对应：缺 name、缺 domain、
// SameSite=None 却没有 Secure。
func ValidateCookie(c Cookie, idx int) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: 第 %d 条缺少 name", errs.ErrInvalidCookie, idx)
	}
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("%w: Cookie %q 缺少 domain（CDP 需要 domain 或 url 才能落盘）",
			errs.ErrInvalidCookie, c.Name)
	}
	// Chrome 依 RFC 6265bis 直接丢弃「SameSite=None 但没有 Secure」的 Cookie，
	// 但 Network.setCookie 本身不报错——库若照旧返回成功，调用方拿到的是
	// 「全部注入成功」的假象，登录态却少了几条。这里提前拦成显式错误。
	if strings.EqualFold(strings.TrimSpace(c.SameSite), "None") && !c.Secure {
		return fmt.Errorf("%w: Cookie %q 的 SameSite=None 必须带 Secure（浏览器会拒收）",
			errs.ErrInvalidCookie, c.Name)
	}
	return nil
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
// path / Expires 的归一化与批量路径共用 cookiePath / cookieExpires——
// 两条注入路径的字段映射各写一份，迟早会出现「只改了一条」的偏差。
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
	return p
}

// CookieParam 把 Cookie 转换成 CDP 的 CookieParam（批量设置用）。
//
// path / Expires 与单个设置路径共用 cookiePath / cookieExpires，
// 免得「改了单条、忘了批量」造成两条注入路径不一致。
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
	return p
}
