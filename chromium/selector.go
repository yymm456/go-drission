package chromium

import (
	"fmt"
	"strings"

	"github.com/chromedp/chromedp"
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
// 直接传裸字符串很容易出错：CSS、XPath、id、JS path 长得完全不一样，
// 而底层 CDP 需要显式指定用哪种方式解析，猜错就报一堆含糊的 node not found。
// 用构造器显式声明语义，让这类错误在写代码时就消失。
type Selector struct {
	expr string
	mode selMode
}

// CSS 按 CSS 选择器定位（chromedp.ByQuery）。
//
//	chromium.CSS("div.item > a")
//	chromium.CSS("#login")
func CSS(sel string) Selector { return Selector{expr: sel, mode: modeCSS} }

// XPath 按 XPath 表达式定位（chromedp.BySearch）。
//
//	chromium.XPath("//div[@class='item']/a")
//	chromium.XPath("//button[text()='登录']")
func XPath(expr string) Selector { return Selector{expr: expr, mode: modeXPath} }

// ID 按元素 id 定位（chromedp.ByID），不需要写 '#' 前缀。
//
//	chromium.ID("username")
func ID(id string) Selector { return Selector{expr: id, mode: modeID} }

// JS 直接用一段「返回 DOM 元素的 JS 表达式」定位（chromedp.ByJSPath）。
//
// 表达式会被交给 Runtime.evaluate 执行，因此必须是可信内容（不做任何转义）。
// 它最大的用处是拿到其它三种方式都够不着的元素，典型是 Shadow DOM：
//
//	chromium.JS(`document.querySelector('#host').shadowRoot.querySelector('#inner')`)
//	chromium.JS(`document.querySelector('#main-title')`)
//
// 注意：这只支持返回单个元素；要取多个请改用 CSS + 遍历。
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
// 判空必须带 TrimSpace：`CSS(prefix + suffix)` 在变量为空时很容易拼出 "   "，
// 它既不是空串也定位不到任何东西，直接发给浏览器只会得到
// 「'   ' is not a valid selector」这种底层语法错误——调用方看不出是自己漏传了参数。
// 这里只用于判定、不改写原表达式：JS 模式的表达式理论上可以含前导空白。
func (s Selector) Empty() bool { return strings.TrimSpace(s.expr) == "" }

// validate 在发起 CDP 调用之前拦住空选择器。
//
// 空表达式传到浏览器会变成 querySelector("") 并抛 SyntaxError，
// 从错误信息完全看不出是调用方漏传参数；更糟的是某些实现会静默命中根节点，
// 让「忘了传选择器」变成一个看起来正常的错误结果。这里统一转成
// ErrSelectorRequired，调用方用 errors.Is 就能判断。
func (s Selector) validate() error {
	if s.Empty() {
		return fmt.Errorf("%w: 选择器为空（%s）", ErrSelectorRequired, s.Mode())
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
