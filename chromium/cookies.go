package chromium

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
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
		return nil, fmt.Errorf("%w: %w", ErrInvalidCookieJSON, err)
	}
	return out, nil
}

// validateCookie 在发起 CDP 调用之前拦住不完整的 Cookie。
//
// CDP 对缺字段只会回一句 "Invalid cookie fields"，看不出是哪一条、
// 缺了哪个字段；批量注入几十条时尤其难查。这里提前指出来。
func validateCookie(c Cookie, idx int) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("%w: 第 %d 条缺少 name", ErrInvalidCookie, idx)
	}
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("%w: Cookie %q 缺少 domain（CDP 需要 domain 或 url 才能落盘）",
			ErrInvalidCookie, c.Name)
	}
	return nil
}

// buildCookieParam 把 Cookie 转换成 CDP 的 SetCookieParams（单个设置用）
func buildCookieParam(c Cookie) *network.SetCookieParams {
	path := c.Path
	if path == "" {
		path = "/"
	}

	p := network.SetCookie(c.Name, c.Value).
		WithDomain(c.Domain).
		WithPath(path)

	if c.HTTPOnly {
		p = p.WithHTTPOnly(true)
	}
	if c.Secure {
		p = p.WithSecure(true)
	}
	if c.SameSite != "" {
		p = p.WithSameSite(network.CookieSameSite(c.SameSite))
	}
	if c.Expires > 0 {
		exp := cdp.TimeSinceEpoch(time.Unix(int64(c.Expires), 0))
		p = p.WithExpires(&exp)
	}
	return p
}

// buildCookieParamBulk 把 Cookie 转换成 CDP 的 CookieParam（批量设置用）
func buildCookieParamBulk(c Cookie) *network.CookieParam {
	path := c.Path
	if path == "" {
		path = "/"
	}

	p := &network.CookieParam{
		Name:     c.Name,
		Value:    c.Value,
		Domain:   c.Domain,
		Path:     path,
		HTTPOnly: c.HTTPOnly,
		Secure:   c.Secure,
	}

	if c.SameSite != "" {
		p.SameSite = network.CookieSameSite(c.SameSite)
	}
	if c.Expires > 0 {
		exp := cdp.TimeSinceEpoch(time.Unix(int64(c.Expires), 0))
		p.Expires = &exp
	}
	return p
}

// SetCookie 注入单个 Cookie。
// 注意：Cookie 必须带 Domain（或改用 SetCookieByURL），否则返回 ErrInvalidCookie。
func (t *Tab) SetCookie(ctx context.Context, c Cookie) error {
	if err := validateCookie(c, 1); err != nil {
		return err
	}
	return t.run(ctx, buildCookieParam(c))
}

// SetCookies 批量注入 Cookie。
//
// 作用域说明：Cookie 会写进当前标签页所属的隔离上下文（BrowserContext），
// 不会泄漏到其它上下文或默认上下文——已由 smoke 用例验证。
// 注入时机上，先 SetCookies 再 Navigate 是可行的（Cookie 会在首次请求前就位），
// 因此免登录不需要「先打开页面 → 注入 → 刷新」这套多余步骤。
func (t *Tab) SetCookies(ctx context.Context, cookies []Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	params := make([]*network.CookieParam, 0, len(cookies))
	for i, c := range cookies {
		if err := validateCookie(c, i+1); err != nil {
			return err
		}
		params = append(params, buildCookieParamBulk(c))
	}
	return t.run(ctx, network.SetCookies(params))
}

// ImportCookiesSource 从任意 CookieSource 导入 Cookie，返回导入条数。
//
// 典型用法（浏览器免登录）：
//
//	tab.LoginWithCookies(ctx, "https://site.com/home", sess.Jar())
func (t *Tab) ImportCookiesSource(ctx context.Context, src CookieSource) (int, error) {
	if src == nil {
		return 0, fmt.Errorf("%w: CookieSource 为 nil", ErrInvalidCookie)
	}
	data, err := src.ExportJSON()
	if err != nil {
		return 0, fmt.Errorf("导出 Cookie 失败: %w", err)
	}
	return t.ImportCookiesJSON(ctx, data)
}

// ImportCookiesJSON 从 JSON 字节导入 Cookie，返回导入条数。
func (t *Tab) ImportCookiesJSON(ctx context.Context, data []byte) (int, error) {
	cookies, err := ParseCookiesJSON(data)
	if err != nil {
		return 0, err
	}
	if len(cookies) == 0 {
		return 0, nil
	}
	if err := t.SetCookies(ctx, cookies); err != nil {
		return 0, err
	}
	return len(cookies), nil
}

// LoginWithCookies 免登录：把已有登录态（Cookie）注入标签页，然后直接打开目标页面。
//
// 顺序是刻意的——先注入再导航：Cookie 在首次请求发出前就已就位，
// 因此目标页面第一个响应就是已登录状态，不需要「先打开登录页 → 注入 → 再刷新」。
// 已通过真实浏览器验证：默认上下文与隔离上下文均生效，且隔离上下文的 Cookie 不会外溢。
//
// ctx 只控制「注入 + 导航」这一阶段，不是标签页的生命周期。
// 导航失败时返回错误；登录是否真的生效由调用方按业务判断（例如检查跳转后的 URL
// 是否离开了登录页），本方法不做站点相关的臆断。
func (t *Tab) LoginWithCookies(ctx context.Context, rawURL string, src CookieSource) error {
	data, err := src.ExportJSON()
	if err != nil {
		return fmt.Errorf("导出 Cookie 失败: %w", err)
	}
	return t.LoginWithCookiesJSON(ctx, rawURL, data)
}

// LoginWithCookiesJSON 与 LoginWithCookies 相同，但直接接受 Cookie JSON 字节，
// 适合「从文件/网络读到的登录态」这种没有 CookieSource 对象的场景。
func (t *Tab) LoginWithCookiesJSON(ctx context.Context, rawURL string, data []byte) error {
	if !hasHTTPScheme(rawURL) {
		return fmt.Errorf("%w: %q（需要形如 https://host/path 的绝对地址）", ErrEmptyURL, rawURL)
	}
	cookies, err := ParseCookiesJSON(data)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return fmt.Errorf("%w: Cookie 列表为空，无法免登录", ErrInvalidCookie)
	}
	if err := t.SetCookies(ctx, cookies); err != nil {
		return fmt.Errorf("注入 Cookie 失败: %w", err)
	}
	return t.Navigate(ctx, rawURL)
}

// hasHTTPScheme 判断地址是否是可用的绝对 http(s) 地址。
func hasHTTPScheme(rawURL string) bool {
	u := strings.TrimSpace(rawURL)
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// GetCookies 返回指定 URL 下的所有 Cookie。
// urls 省略时返回当前隔离上下文内的全部 Cookie。
func (t *Tab) GetCookies(ctx context.Context, urls ...string) ([]*network.Cookie, error) {
	var cookies []*network.Cookie
	err := t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs(urls).Do(c)
		return err
	}))
	return cookies, err
}

// Cookies 返回当前隔离上下文内的全部 Cookie（转为可序列化的 Cookie 结构）。
func (t *Tab) Cookies(ctx context.Context, urls ...string) ([]Cookie, error) {
	raw, err := t.GetCookies(ctx, urls...)
	if err != nil {
		return nil, err
	}
	out := make([]Cookie, 0, len(raw))
	for _, c := range raw {
		out = append(out, Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			HTTPOnly: c.HTTPOnly,
			Secure:   c.Secure,
			SameSite: string(c.SameSite),
			Expires:  c.Expires,
		})
	}
	return out, nil
}

// ExportCookiesJSON 把指定 URL 下的 Cookie 导出为 JSON 字节。
// urls 省略时导出当前上下文内的全部 Cookie，便于与 session 包互相接力。
func (t *Tab) ExportCookiesJSON(ctx context.Context, urls ...string) ([]byte, error) {
	cookies, err := t.Cookies(ctx, urls...)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(cookies, "", "  ")
}

// ExportCookies 把指定 URL 下的 Cookie 导出到 JSON 文件。
// 父目录不存在会自动创建；文件权限 0600——Cookie 里含会话令牌，
// 沿用 0644 会让同机其他用户直接读到登录态。
func (t *Tab) ExportCookies(ctx context.Context, path string, urls ...string) error {
	data, err := t.ExportCookiesJSON(ctx, urls...)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// ImportCookies 从 JSON 文件导入 Cookie。父目录不存在等读取错误如实返回。
func (t *Tab) ImportCookies(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 Cookie 文件 %s 失败: %w", path, err)
	}
	_, err = t.ImportCookiesJSON(ctx, data)
	return err
}
