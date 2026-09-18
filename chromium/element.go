package chromium

import (
	"context"
	"fmt"
	"strings"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// Element 是「一个待操作的页面元素」：选择器 + 所属标签页。
//
// 设计上刻意把「怎么找」和「做什么」拆成两步，把选择器从每次调用里提出来：
//
//	tab.EleID("username").SendKeys(ctx, "xxx")
//	tab.EleCSS(".p-button-label").Click(ctx)
//	title, _ := tab.EleCSS("h1").Text(ctx)
//
// 相比旧写法 tab.SendKeys(ctx, chromium.ID("username"), "xxx")，
// 选择器只出现一次，读起来就是「找到什么 → 对它做什么」。
//
// # 不缓存 DOM 节点
//
// Element 只保存选择器，**每次操作都重新查询一遍**。这不是偷懒，是 SPA 场景下的
// 硬性要求：Vue/React 重渲染会把旧节点整批换成新节点，缓存的 nodeID 会变成
// detached 节点——此时读属性拿到的是旧值，点击会静默无效，而且完全不报错。
// 重新查询的成本是一次 CDP 往返，比排查「为什么点了没反应」便宜得多。
//
// # 错误语义（均可用 errors.Is 判断）
//
//   - 选择器为空（如 EleCSS("")）→ ErrSelectorRequired
//   - 元素不存在 / 等待超时 → ErrElementNotFound
//
// 各操作都接受调用方 ctx（应从 tab.Ctx 派生），超时与取消由调用方掌控。
type Element struct {
	tab *Tab
	sel Selector
}

// ---------- 查询入口（Tab 上）----------

// Ele 用显式选择器构造元素，选择器请用本包的构造器声明定位方式。
//
//	tab.Ele(chromium.CSS("#login"))
//	tab.Ele(chromium.XPath("//button[text()='登录']"))
//
// 一般用不着它，EleCSS / EleID / EleXPath / EleJS 更顺手。
func (t *Tab) Ele(sel Selector) *Element {
	return &Element{tab: t, sel: sel}
}

// EleCSS 按 CSS 选择器定位（内部即 chromedp.ByQuery）。
//
//	tab.EleCSS("#login")
//	tab.EleCSS("div.item > a")
func (t *Tab) EleCSS(sel string) *Element {
	return t.Ele(CSS(sel))
}

// EleID 按元素 id 定位，不需要写 '#' 前缀。
//
//	tab.EleID("username")
func (t *Tab) EleID(id string) *Element {
	return t.Ele(ID(id))
}

// EleXPath 按 XPath 表达式定位（内部即 chromedp.BySearch）。
//
//	tab.EleXPath(`//a[text()="详情"]`)
func (t *Tab) EleXPath(expr string) *Element {
	return t.Ele(XPath(expr))
}

// EleJS 用一段「返回 DOM 元素的 JS 表达式」定位（内部即 chromedp.ByJSPath）。
// 典型用途是穿透 Shadow DOM：
//
//	tab.EleJS(`document.querySelector('#host').shadowRoot.querySelector('#inner')`)
func (t *Tab) EleJS(expr string) *Element {
	return t.Ele(JS(expr))
}

// ---------- 元信息 ----------

// Selector 返回该元素使用的选择器，便于日志与错误排查。
func (e *Element) Selector() Selector { return e.sel }

// Tab 返回该元素所属的标签页，便于在链式调用中回到页面级操作。
func (e *Element) Tab() *Tab { return e.tab }

// String 返回形如 css="#login" 的可读描述。
func (e *Element) String() string {
	if e == nil {
		return "<nil element>"
	}
	return fmt.Sprintf("%s=%q", e.sel.Mode(), e.sel.String())
}

// node 重新查询一次并返回第一个匹配节点。
// 所有节点级操作（ClickJS / SetValue / Eval）都经由它，因此天然不缓存。
func (e *Element) node(ctx context.Context) (*cdproto.Node, error) {
	return e.tab.firstNode(ctx, e.sel)
}

// ---------- 操作 ----------

// Click 点击元素（真实鼠标事件，会做可见性检查）。
// 元素不可见或不存在时返回 ErrElementNotFound。
func (e *Element) Click(ctx context.Context) error {
	if err := e.sel.validate(); err != nil {
		return err
	}
	return wrapNotFound(e.tab.run(ctx,
		chromedp.Click(e.sel.expr, e.sel.options()...)), e.sel)
}

// ClickJS 用 JS 触发 el.click()，绕过可见性检查。
//
// 适用场景：元素被遮挡、动画未结束、或本身就是隐藏的（如自定义 checkbox）。
// 代价是没有真实鼠标事件序列，某些依赖 mousedown/mouseup 的组件不会响应。
func (e *Element) ClickJS(ctx context.Context) error {
	node, err := e.node(ctx)
	if err != nil {
		return err
	}
	_, err = e.tab.evalOnNode(ctx, node, "function() { this.click(); return 'ok'; }")
	return err
}

// SendKeys 向输入框输入文本（会先清空）。
// 走真实键盘事件，React/Vue 的受控输入能正确感知。
// 若目标组件吞掉了按键（或标签页在后台被节流），改用 SetValue。
func (e *Element) SendKeys(ctx context.Context, text string) error {
	if err := e.sel.validate(); err != nil {
		return err
	}
	err := e.tab.run(ctx,
		chromedp.Clear(e.sel.expr, e.sel.options()...),
		chromedp.SendKeys(e.sel.expr, text, e.sel.options()...),
	)
	return wrapNotFound(err, e.sel)
}

// SetValue 直接设置输入框的值并触发 input/change 事件。
//
// 与 SendKeys 的区别：这里是 JS 赋值，不会产生键盘事件，因此不受输入法、
// 键盘布局、后台节流影响；反过来说，只监听 keystroke 的组件不会被触发。
func (e *Element) SetValue(ctx context.Context, value string) error {
	node, err := e.node(ctx)
	if err != nil {
		return err
	}
	fn := fmt.Sprintf(`function() {
		this.focus();
		this.value = %q;
		this.dispatchEvent(new Event("input", { bubbles: true }));
		this.dispatchEvent(new Event("change", { bubbles: true }));
		return 'ok';
	}`, value)
	_, err = e.tab.evalOnNode(ctx, node, fn)
	return err
}

// Text 返回元素的文本内容（innerText）。
// 元素不存在时返回 ErrElementNotFound，而不是空字符串——
// 否则调用方无法区分「元素没有文字」和「元素根本没出现」。
func (e *Element) Text(ctx context.Context) (string, error) {
	if err := e.sel.validate(); err != nil {
		return "", err
	}
	var text string
	err := e.tab.run(ctx,
		chromedp.Text(e.sel.expr, &text, e.sel.options()...))
	return text, wrapNotFound(err, e.sel)
}

// Attribute 返回元素的属性值，例如 el.Attribute(ctx, "href")。
// 元素不存在时返回 ErrElementNotFound。
func (e *Element) Attribute(ctx context.Context, name string) (string, error) {
	if err := e.sel.validate(); err != nil {
		return "", err
	}
	var value string
	err := e.tab.run(ctx,
		chromedp.AttributeValue(e.sel.expr, name, &value, nil, e.sel.options()...))
	return value, wrapNotFound(err, e.sel)
}

// Count 返回该选择器匹配到的元素数量。
//
// 注意语义：它统计的是「选择器命中多少个」，不要求元素唯一。
// 因此 Count 为 0 是正常返回值（不是错误），用 err 判断即可。
//
// 四种定位方式一律走一次 JS 求值（与 FrameElement.Count 对齐）：既省掉把节点树
// 拉回来的开销，也保证「未命中」在每种方式下都是 0。早期只有 CSS 分支走 JS，
// XPath / ID 走 chromedp.Nodes——而 chromedp 的节点查询在命中前会一直重试到
// ctx 到期，于是未命中变成「白等一个完整超时再报错」，与上面这段文档自相矛盾
// （历史缺陷 BUG-04）。
func (e *Element) Count(ctx context.Context) (int, error) {
	if err := e.sel.validate(); err != nil {
		return 0, err
	}

	// 选择器 → 计数 JS 的映射收敛在 selectorCountJS，与 FrameElement.Count 共用同一份；
	// 两条路径各写一份 mode 分支迟早会漂移（这段分支历史上就出过 BUG-04）。
	js := selectorCountJS(e.sel)

	var count int
	if err := e.tab.run(ctx, chromedp.Evaluate(js, &count)); err != nil {
		return 0, err
	}
	return count, nil
}

// Eval 在该元素上执行 JS 并返回结果，函数体内 this 即该元素。
//
// fn 支持三种写法，会自动识别（省掉每次手写 function 包装的噪音）：
//
//	el.Eval(ctx, "this.innerText")                        // 表达式
//	el.Eval(ctx, "return this.dataset.id")                // 函数体
//	el.Eval(ctx, "function(){ return this.tagName }")     // 完整函数声明
//
// 走 Runtime.callFunctionOn 直接作用于节点，因此 Shadow DOM 内的元素、
// 无法用 CSS 表达的选择器（XPath / JS path）都能一致地参与求值。
// 元素不存在时返回 ErrElementNotFound。
func (e *Element) Eval(ctx context.Context, fn string) (any, error) {
	node, err := e.node(ctx)
	if err != nil {
		return nil, err
	}
	return e.tab.evalOnNode(ctx, node, toFunctionDecl(fn))
}

// ---------- 等待 ----------

// WaitVisible 等待元素可见（display:none / visibility:hidden / 零尺寸都不算可见）。
func (e *Element) WaitVisible(ctx context.Context) error {
	if err := e.sel.validate(); err != nil {
		return err
	}
	return wrapNotFound(e.tab.run(ctx,
		chromedp.WaitVisible(e.sel.expr, e.sel.options()...)), e.sel)
}

// WaitText 轮询等待元素文本包含 substr，直到 ctx 结束。
//
// 与 Wait().Text(substr).Do(ctx) 等价，用于只等一个条件、不想拉链式调用的场景。
func (e *Element) WaitText(ctx context.Context, substr string) error {
	// 循环骨架与错误分诊规则见 pollWait。
	return pollWait(ctx, fmt.Sprintf("等待 %s 文本包含 %q", e, substr), false, func() (bool, error) {
		text, err := e.Text(ctx)
		return err == nil && strings.Contains(text, substr), err
	})
}

// Wait 返回绑定到该元素的链式等待构建器。
//
//	el.Wait().Visible().Timeout(10 * time.Second).Do(ctx)
//	el.Wait().Text("已完成").Do(ctx)
//	el.Wait().Count(5).Do(ctx)
//
// 页面级条件仍走 Tab.Wait()：tab.Wait().URL("/home").Do(ctx)。
func (e *Element) Wait() *WaitBuilder {
	return &WaitBuilder{tab: e.tab, el: e}
}

// ---------- 内部辅助 ----------

// toFunctionDecl 把用户传入的 JS 片段规整成 runtime.callFunctionOn 需要的函数声明。
//
// callFunctionOn 只接受函数声明，直接传 "this.innerText" 会被浏览器
// 当成语法错误（"not a function"）。这里做最小猜测：
//   - 已经是 function / 箭头函数 → 原样用；
//   - 单表达式（无分号、无换行、不以语句关键字开头）→ 包成 return (expr)；
//   - 其余（多语句函数体）→ 包成 function(){ ... }。
//
// 猜测失败时不会静默：浏览器会抛出异常，错误信息里带原始 JS，足以定位。
func toFunctionDecl(fn string) string {
	s := strings.TrimSpace(fn)
	switch {
	case s == "":
		return "function() {}"
	case strings.HasPrefix(s, "function"), strings.Contains(s, "=>"):
		return s
	case !strings.ContainsAny(s, ";\n") && !hasStatementKeyword(s):
		return "function(){ return (" + s + "); }"
	default:
		return "function(){ " + s + " }"
	}
}

// hasStatementKeyword 判断 JS 片段是否以语句关键字开头（这类不能塞进 return (...)）。
func hasStatementKeyword(s string) bool {
	for _, kw := range []string{
		"return", "var", "let", "const", "if", "for", "while", "do", "switch",
		"throw", "try", "class", "import", "export", "delete", "await",
	} {
		if s == kw || strings.HasPrefix(s, kw+" ") || strings.HasPrefix(s, kw+"\t") {
			return true
		}
	}
	return false
}
