package chromium

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSelectorConstructors(t *testing.T) {
	cases := []struct {
		sel  Selector
		expr string
		mode string
	}{
		{CSS("div.item > a"), "div.item > a", "css"},
		{XPath("//div[@class='item']/a"), "//div[@class='item']/a", "xpath"},
		{ID("username"), "username", "id"},
		{JS("document.querySelector('#a')"), "document.querySelector('#a')", "jspath"},
	}

	for _, c := range cases {
		if c.sel.String() != c.expr {
			t.Errorf("String() = %q，期望 %q", c.sel.String(), c.expr)
		}
		if got := c.sel.Mode(); got != c.mode {
			t.Errorf("Mode() = %q，期望 %q", got, c.mode)
		}
		if c.sel.Empty() {
			t.Errorf("非空选择器被判定为空: %q", c.expr)
		}
		if len(c.sel.options()) == 0 {
			t.Errorf("选择器 %q 未产出 chromedp 查询选项", c.expr)
		}
	}
}

// TestSelectorZeroValueFallsBackToCSS 保证不走构造器直接构造时也有确定语义（按 CSS 处理），
// 避免 mode 零值意外落到别的定位方式上。
func TestSelectorZeroValueFallsBackToCSS(t *testing.T) {
	var s Selector
	if got := s.Mode(); got != "css" {
		t.Errorf("零值选择器 Mode() = %q，期望 css", got)
	}
	if !s.Empty() {
		t.Error("零值选择器应判定为空")
	}
}

// TestSelectorJSString 覆盖 node 级操作用的 JS 表达式生成。
// 四种模式都要能产出可执行的表达式：ID / CSS 直接拼 DOM API，
// XPath 走 document.evaluate，JS 模式本身就是表达式所以原样返回。
func TestSelectorJSString(t *testing.T) {
	if got := ID("uid").jsExpr(); got != `document.getElementById("uid")` {
		t.Errorf("ID 的 JS 表达式不符合预期: %s", got)
	}
	if got := CSS("#a").jsExpr(); got != `document.querySelector("#a")` {
		t.Errorf("CSS 的 JS 表达式不符合预期: %s", got)
	}
	if got := XPath("//a").jsExpr(); got == "" {
		t.Error("XPath 应能生成 JS 表达式")
	}
	// ByJSPath 本身就是 JS 表达式，应原样返回
	jsSel := JS("document.querySelector('#host').shadowRoot.querySelector('#inner')")
	if got := jsSel.jsExpr(); got != jsSel.String() {
		t.Errorf("JS 选择器应原样返回表达式，实际 %q", got)
	}
}

// TestSelectorValidate 覆盖空选择器的拦截。
//
// 空表达式传到浏览器会变成 querySelector("") 并抛 SyntaxError，错误文案里
// 完全看不出是调用方漏传参数——必须在发 CDP 命令之前就挡掉。
func TestSelectorValidate(t *testing.T) {
	empties := []Selector{
		CSS(""), XPath(""), ID(""), JS(""), {},
	}
	for _, s := range empties {
		if err := s.validate(); !errors.Is(err, ErrSelectorRequired) {
			t.Errorf("空选择器 %v 期望 ErrSelectorRequired，实际 %v", s.Mode(), err)
		}
	}

	// 非空选择器不应被误拦
	for _, s := range []Selector{CSS("#a"), XPath("//a"), ID("a"), JS("document.body")} {
		if err := s.validate(); err != nil {
			t.Errorf("非空选择器 %q 被误拦：%v", s.String(), err)
		}
	}
}

// TestWrapNotFound 覆盖「等待到超时」到 ErrElementNotFound 的翻译。
// 调用方主动取消是决策，必须原样透传，不能被伪装成「元素没找到」。
func TestWrapNotFound(t *testing.T) {
	if err := wrapNotFound(nil, CSS("#a")); err != nil {
		t.Errorf("nil 错误应原样返回 nil，实际 %v", err)
	}

	wrapped := wrapNotFound(context.DeadlineExceeded, CSS("#miss"))
	if !errors.Is(wrapped, ErrElementNotFound) {
		t.Errorf("超时应包装成 ErrElementNotFound，实际 %v", wrapped)
	}
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("原始错误应保留在错误链里，实际 %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "#miss") {
		t.Errorf("错误信息里应带上没命中的选择器，实际 %q", wrapped.Error())
	}

	if err := wrapNotFound(context.Canceled, CSS("#a")); !errors.Is(err, context.Canceled) {
		t.Errorf("主动取消应原样透传，实际 %v", err)
	}
}
