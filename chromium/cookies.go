package chromium

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Cookie 描述一个要注入的 Cookie
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

// SetCookie 注入单个 Cookie
func (t *Tab) SetCookie(ctx context.Context, c Cookie) error {
	return chromedp.Run(t.safeCtx(ctx), buildCookieParam(c))
}

// SetCookies 批量注入 Cookie
func (t *Tab) SetCookies(ctx context.Context, cookies []Cookie) error {
	params := make([]*network.CookieParam, 0, len(cookies))
	for _, c := range cookies {
		params = append(params, buildCookieParamBulk(c))
	}
	return chromedp.Run(t.safeCtx(ctx), network.SetCookies(params))
}

// GetCookies 返回指定 URL 下的所有 Cookie
func (t *Tab) GetCookies(ctx context.Context, urls ...string) ([]*network.Cookie, error) {
	var cookies []*network.Cookie
	err := chromedp.Run(t.safeCtx(ctx), chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs(urls).Do(ctx)
		return err
	}))
	return cookies, err
}

// ExportCookies 把指定 URL 下的 Cookie 导出到 JSON 文件
func (t *Tab) ExportCookies(ctx context.Context, path string, urls ...string) error {
	cookies, err := t.GetCookies(ctx, urls...)
	if err != nil {
		return err
	}

	out := make([]Cookie, 0, len(cookies))
	for _, c := range cookies {
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

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ImportCookies 从 JSON 文件导入 Cookie
func (t *Tab) ImportCookies(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cookies []Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return err
	}
	return t.SetCookies(ctx, cookies)
}
