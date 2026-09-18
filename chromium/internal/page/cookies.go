package page

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/internal/cookie"
	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// SetCookie 注入单个 Cookie。
// 注意：Cookie 必须带 Domain（或改用 SetCookieByURL），否则返回 ErrInvalidCookie。
func (t *Tab) SetCookie(ctx context.Context, c cookie.Cookie) error {
	if err := cookie.ValidateCookie(c, 1); err != nil {
		return err
	}
	return t.run(ctx, cookie.SetCookieParams(c))
}

// SetCookies 批量注入 Cookie。
//
// 作用域说明：Cookie 会写进当前标签页所属的隔离上下文（BrowserContext），
// 不会泄漏到其它上下文或默认上下文——已由 smoke 用例验证。
// 注入时机上，先 SetCookies 再 Navigate 是可行的（Cookie 会在首次请求前就位），
// 因此免登录不需要「先打开页面 → 注入 → 刷新」这套多余步骤。
func (t *Tab) SetCookies(ctx context.Context, cookies []cookie.Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	params := make([]*network.CookieParam, 0, len(cookies))
	for i, c := range cookies {
		if err := cookie.ValidateCookie(c, i+1); err != nil {
			return err
		}
		params = append(params, cookie.CookieParam(c))
	}
	return t.run(ctx, network.SetCookies(params))
}

// ImportCookiesSource 从任意 CookieSource 导入 Cookie，返回导入条数。
//
// 典型用法（浏览器免登录）：
//
//	tab.LoginWithCookies(ctx, "https://site.com/home", sess.Jar())
func (t *Tab) ImportCookiesSource(ctx context.Context, src cookie.CookieSource) (int, error) {
	if src == nil {
		return 0, fmt.Errorf("%w: CookieSource 为 nil", errs.ErrInvalidCookie)
	}
	data, err := src.ExportJSON()
	if err != nil {
		return 0, fmt.Errorf("导出 Cookie 失败: %w", err)
	}
	return t.ImportCookiesJSON(ctx, data)
}

// ImportCookiesJSON 从 JSON 字节导入 Cookie，返回导入条数。
func (t *Tab) ImportCookiesJSON(ctx context.Context, data []byte) (int, error) {
	cookies, err := cookie.ParseCookiesJSON(data)
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
func (t *Tab) LoginWithCookies(ctx context.Context, rawURL string, src cookie.CookieSource) error {
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
		return fmt.Errorf("%w: %q（需要形如 https://host/path 的绝对地址）", errs.ErrEmptyURL, rawURL)
	}
	cookies, err := cookie.ParseCookiesJSON(data)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return fmt.Errorf("%w: Cookie 列表为空，无法免登录", errs.ErrInvalidCookie)
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
func (t *Tab) Cookies(ctx context.Context, urls ...string) ([]cookie.Cookie, error) {
	raw, err := t.GetCookies(ctx, urls...)
	if err != nil {
		return nil, err
	}
	out := make([]cookie.Cookie, 0, len(raw))
	for _, c := range raw {
		out = append(out, cookie.Cookie{
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
