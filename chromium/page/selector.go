package page

import (
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/errs"
)

// 选择器模式，决定底层用哪条 CDP 定位路径。
type selMode int

const (
	modeCSS    selMode = iota // chromedp.ByQuery —— CSS 选择器
	modeXPath                 // chromedp.BySearch —— XPath 表达式
	modeID                    // chromedp.ByID —— 元素 id
	modeJSPath                // chromedp.ByJSPath —— DOM 树 JS path
)

// Selector 描述「怎么定位一个元素」。
//
// 底层 CDP 需显式指定解析方式，因此用构造器显式声明语义，避免传裸字符串猜错方式。
type Selector struct {
	expr string
	mode selMode
}

// CSS 按 CSS 选择器定位（chromedp.ByQuery），公开文档见门面 chromium。
func CSS(sel string) Selector { return Selector{expr: sel, mode: modeCSS} }

// XPath 按 XPath 表达式定位（chromedp.BySearch），公开文档见门面 chromium。
func XPath(expr string) Selector { return Selector{expr: expr, mode: modeXPath} }

// ID 按元素 id 定位（chromedp.ByID），不需要写 '#' 前缀，公开文档见门面 chromium。
func ID(id string) Selector { return Selector{expr: id, mode: modeID} }

// JS 用一段「返回 DOM 元素的 JS 表达式」定位（chromedp.ByJSPath），公开文档见门面 chromium。
// 表达式必须是可信内容（Runtime.evaluate 不做转义）；只支持返回单个元素。
func JS(expr string) Selector { return Selector{expr: expr, mode: modeJSPath} }

// String 返回选择器的原始表达式，便于日志与错误信息排查。
func (s Selector) String() string { return s.expr }

// Mode 返回选择器的定位方式名称，主要用于错误信息。
func (s Selector) Mode() string {
	switch s.mode {
	case modeXPath:
		return "xpath"
	case modeID:
		return "id"
	case modeJSPath:
		return "jspath"
	default:
		return "css"
	}
}

// Empty 判断选择器是否为空（未构造、传了空字符串，或只有空白字符）。
//
// 判空必须带 TrimSpace：`CSS(prefix + suffix)` 在变量为空时会拼出 "   "，它既非空串也定位不到
// 任何东西，直接发给浏览器只会得到底层语法错误。只用于判定、不改写原表达式（JS 模式可含前导空白）。
func (s Selector) Empty() bool { return strings.TrimSpace(s.expr) == "" }

// validate 在发起 CDP 调用之前拦住空选择器。
//
// 空表达式传到浏览器会变成 querySelector("") 并抛 SyntaxError，从错误信息看不出是调用方漏传参数；
// 某些实现甚至会静默命中根节点。这里统一转成 ErrSelectorRequired，调用方用 errors.Is 即可判断。
func (s Selector) validate() error {
	if s.Empty() {
		return fmt.Errorf("%w: 选择器为空（%s）", errs.ErrSelectorRequired, s.Mode())
	}
	return nil
}

// options 返回交给 chromedp 的查询选项。
// 零值 Selector（没走构造器）按 CSS 处理，避免直接构造结构体时语义丢失。
func (s Selector) options() []chromedp.QueryOption {
	switch s.mode {
	case modeXPath:
		return []chromedp.QueryOption{chromedp.BySearch}
	case modeID:
		return []chromedp.QueryOption{chromedp.ByID}
	case modeJSPath:
		return []chromedp.QueryOption{chromedp.ByJSPath}
	default:
		return []chromedp.QueryOption{chromedp.ByQuery}
	}
}

// jsExpr 生成「取到该元素」的 JS 表达式，供 Eval 类操作使用。
// 四种模式都能表达：ID → getElementById；XPath → document.evaluate(...).singleNodeValue
// （取单个节点，故用 FIRST_ORDERED_NODE_TYPE=9）；JS path 本身就是返回元素的合法 JS
// 表达式，原样返回；其余按 CSS 走 querySelector。
func (s Selector) jsExpr() string {
	switch s.mode {
	case modeID:
		return fmt.Sprintf("document.getElementById(%q)", s.expr)
	case modeXPath:
		return fmt.Sprintf("document.evaluate(%q, document, null, 9, null).singleNodeValue", s.expr)
	case modeJSPath:
		// ByJSPath 本身就是合法 JS 表达式，原样返回即可
		return s.expr
	default:
		return fmt.Sprintf("document.querySelector(%q)", s.expr)
	}
}

// selectorCountJS 生成「统计该选择器命中多少个元素」的 JS 表达式。
//
// 与 jsExpr 一样收敛在 Selector 上：Element.Count（经 chromedp.Evaluate）与 FrameElement.Count
// （在框架 isolated world 里 Runtime.evaluate）走两条不同通道，但「选择器 → 计数 JS」必须一致，
// 否则一边新增模式时另一边会静默落进 default 而数错个数。
//
// 语义：
//   - XPath：count(...) 返回数字，必须请求 NUMBER_TYPE(1)；表达式用 %q 拼进 JS，避免带双引号的属性值提前截断字符串。
//   - CSS：querySelectorAll(...).length。
//   - ID / JS path：语义上是单个元素，命中 1、未命中 0。
func selectorCountJS(sel Selector) string {
	switch sel.mode {
	case modeXPath:
		return fmt.Sprintf(`document.evaluate(%q, document, null, 1, null).numberValue`,
			"count("+sel.expr+")")
	case modeCSS:
		return fmt.Sprintf(`document.querySelectorAll(%q).length`, sel.expr)
	default:
		// modeID / modeJSPath
		return fmt.Sprintf(`(function(){ const el = %s; return el ? 1 : 0; })()`, sel.jsExpr())
	}
}
