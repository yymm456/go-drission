package chromium

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Tab 代表一个被托管的标签页。
// Ctx 是该标签页的 chromedp 会话根上下文，承载 target 路由信息。
// 所有 I/O 方法都接受调用方传入的 ctx（应从 Ctx 派生），由调用方控制超时与取消。
type Tab struct {
	ID     target.ID
	Ctx    context.Context
	cancel context.CancelFunc
	URL    string
}

// Navigate 导航到 url，等待页面 load 完成。
// 超时与取消由 ctx 控制；导航失败（含超时）如实返回 error，不吞掉。
func (t *Tab) Navigate(ctx context.Context, url string) error {
	return chromedp.Run(ctx, chromedp.Navigate(url))
}

// Title 返回当前页面标题
func (t *Tab) Title(ctx context.Context) (string, error) {
	var title string
	err := chromedp.Run(ctx, chromedp.Title(&title))
	return title, err
}

// CurrentURL 返回当前页面地址
func (t *Tab) CurrentURL(ctx context.Context) (string, error) {
	var u string
	err := chromedp.Run(ctx, chromedp.Location(&u))
	return u, err
}

// HTML 返回当前页面完整 HTML
func (t *Tab) HTML(ctx context.Context) (string, error) {
	var html string
	err := chromedp.Run(ctx, chromedp.OuterHTML("html", &html))
	return html, err
}

// Eval 执行 JavaScript 并返回原始结果。
// 返回值按 JSON 语义解码：字符串→string、数字→float64、对象→map[string]interface{}、
// 数组→[]interface{}、布尔→bool、null→nil。
func (t *Tab) Eval(ctx context.Context, js string) (interface{}, error) {
	var result interface{}
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &result))
	return result, err
}

// ---------- 等待方法（超时统一由 ctx 控制）----------

// WaitVisible 等待元素可见
func (t *Tab) WaitVisible(ctx context.Context, selector string) error {
	return chromedp.Run(ctx, chromedp.WaitVisible(selector, chromedp.ByQuery))
}

// WaitReady 等待页面加载完成（body 就绪）
func (t *Tab) WaitReady(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.WaitReady("body", chromedp.ByQuery))
}

// WaitURL 轮询等待当前 URL 包含指定子串，直到 ctx 结束
func (t *Tab) WaitURL(ctx context.Context, substr string) error {
	for {
		if u, err := t.CurrentURL(ctx); err == nil && strings.Contains(u, substr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 URL 包含 %q 失败: %w", substr, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WaitText 轮询等待指定元素的文本包含指定子串，直到 ctx 结束
func (t *Tab) WaitText(ctx context.Context, selector, substr string) error {
	for {
		if text, err := t.Text(ctx, selector); err == nil && strings.Contains(text, substr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 %s 文本包含 %q 失败: %w", selector, substr, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WaitCount 轮询等待选择器匹配到至少 n 个元素，直到 ctx 结束
func (t *Tab) WaitCount(ctx context.Context, selector string, n int) error {
	for {
		if count, err := t.Count(ctx, selector); err == nil && count >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 %s 出现至少 %d 个元素失败: %w", selector, n, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---------- 操作 ----------

// Click 点击元素
func (t *Tab) Click(ctx context.Context, selector string) error {
	return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
}

// ClickJS 用 JS 触发点击，绕过可见性检查
func (t *Tab) ClickJS(ctx context.Context, selector string) error {
	js := fmt.Sprintf(`
(function() {
    const el = document.querySelector(%q);
    if (!el) return "not found";
    el.click();
    return "ok";
})()
`, selector)
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &result)); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("ClickJS 失败: %s (selector=%s)", result, selector)
	}
	return nil
}

// SendKeys 向输入框输入文本（会先清空）
func (t *Tab) SendKeys(ctx context.Context, selector, text string) error {
	return chromedp.Run(ctx,
		chromedp.Clear(selector, chromedp.ByQuery),
		chromedp.SendKeys(selector, text, chromedp.ByQuery),
	)
}

// SetValue 直接设置输入框的值并触发 input/change 事件
// 适用于 React/Vue 等框架控制的输入框，绕过 chromedp.SendKeys 的文本节点限制
func (t *Tab) SetValue(ctx context.Context, selector, value string) error {
	js := fmt.Sprintf(`
(function() {
    const el = document.querySelector(%q);
    if (!el) return "not found";
    el.focus();
    el.value = %q;
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new Event("change", { bubbles: true }));
    return "ok";
})()
`, selector, value)

	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &result)); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SetValue 失败: %s (selector=%s)", result, selector)
	}
	return nil
}

// ---------- 读取 ----------

// Text 返回元素的文本内容
func (t *Tab) Text(ctx context.Context, selector string) (string, error) {
	var text string
	err := chromedp.Run(ctx, chromedp.Text(selector, &text, chromedp.ByQuery))
	return text, err
}

// Attribute 返回指定选择器元素的属性值
func (t *Tab) Attribute(ctx context.Context, selector, name string) (string, error) {
	var value string
	err := chromedp.Run(ctx, chromedp.AttributeValue(selector, name, &value, nil, chromedp.ByQuery))
	return value, err
}

// Count 返回选择器匹配到的元素数量
func (t *Tab) Count(ctx context.Context, selector string) (int, error) {
	var count int
	js := fmt.Sprintf(`document.querySelectorAll(%q).length`, selector)
	err := chromedp.Run(ctx, chromedp.Evaluate(js, &count))
	return count, err
}

// ---------- 截图 ----------

// Screenshot 截取当前页面，保存到 path
func (t *Tab) Screenshot(ctx context.Context, path string) error {
	var buf []byte
	if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return err
	}
	return writeFile(path, buf)
}

// Reload 重新加载当前页面
func (t *Tab) Reload(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.Reload().Do(ctx)
	}))
}
