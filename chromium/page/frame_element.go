package page

import (
	"context"
	"fmt"
)

// FrameElement 是「iframe 内的一个待操作元素」：选择器 + 所属框架。
//
// 与 Element 是刻意的镜像关系：Element 之于 Tab，就是 FrameElement 之于 Frame。
// 差别在底层通道——Element 走 CDP DOM 域（真实鼠标 / 键盘事件），
// FrameElement 走框架 isolated world 里的 JS（见下方语义约束）。
//
// 同样把「怎么找」和「做什么」拆成两步，选择器从每次调用里提出来：
//
//	frame, _ := tab.Frame(ctx, chromium.CSS("iframe#login"))
//	frame.EleID("username").SetValue(ctx, "xxx")
//	frame.EleCSS("#password").SetValue(ctx, "xxx")
//	frame.EleCSS(".submit").Click(ctx)
//	title, _ := frame.EleXPath(`//h1`).Text(ctx)
//	n, _ := frame.EleCSS("li.item").Count(ctx)
//
// 相比旧写法 frame.Text(ctx, chromium.CSS("h1"))，选择器只出现一次。
//
// # 语义约束（与 Element 的差异，用之前必须知道）
//
//   - 操作全部在框架的 isolated world 内用 JS 完成：Click 是 el.click()，
//     Text 读的是 el.innerText，不经过 CDP DOM 域。
//     因此跨域 iframe 也能操作——脚本直接跑在目标框架的渲染进程里。
//   - Click 不做可见性检查：元素被遮挡、隐藏、零尺寸都照点不误，
//     也不会产生 mousedown/mouseup 序列。要真实鼠标事件请改用 Tab 上的
//     Element（那才走 input 域），代价是它进不去跨域 iframe。
//   - SetValue 是直接赋值并手动派发 input/change，不产生任何键盘事件。
//     只监听 keystroke 的组件不会被触发。
//   - 不缓存 DOM 节点：每次操作重新查询一次选择器。SPA 重渲染会换掉节点，
//     缓存住的 nodeID 会静默变成 detached 节点。
//
// # 错误语义（均可用 errors.Is 判断）
//
//   - 选择器为空（如 EleCSS("")）→ ErrSelectorRequired
//   - 元素不存在 / 框架内脚本返回 'not found' → ErrElementNotFound
//   - 框架已随导航失效且重新定位失败 → ErrFrameDetached
//
// 各操作都接受调用方 ctx（应从 tab.Ctx 派生），超时与取消由调用方掌控。
type FrameElement struct {
	frame *Frame
	sel   Selector
}

// ---------- 查询入口（Frame 上）----------

// Ele 用显式选择器构造框架内元素，选择器请用本包的构造器声明定位方式。
//
//	frame.Ele(chromium.CSS("#login"))
//	frame.Ele(chromium.XPath("//button[text()='登录']"))
//
// 一般用不着它，EleCSS / EleID / EleXPath / EleJS 更顺手。
func (f *Frame) Ele(sel Selector) *FrameElement {
	return &FrameElement{frame: f, sel: sel}
}

// EleCSS 按 CSS 选择器在框架内定位。
//
//	frame.EleCSS("#login")
//	frame.EleCSS("div.item > a")
func (f *Frame) EleCSS(sel string) *FrameElement {
	return f.Ele(CSS(sel))
}

// EleID 按元素 id 在框架内定位，不需要写 '#' 前缀。
//
//	frame.EleID("username")
func (f *Frame) EleID(id string) *FrameElement {
	return f.Ele(ID(id))
}

// EleXPath 按 XPath 表达式在框架内定位。
//
//	frame.EleXPath(`//a[text()="详情"]`)
func (f *Frame) EleXPath(expr string) *FrameElement {
	return f.Ele(XPath(expr))
}

// EleJS 用一段「返回 DOM 元素的 JS 表达式」在框架内定位。
// 典型用途是穿透框架内的 Shadow DOM：
//
//	frame.EleJS(`document.querySelector('#host').shadowRoot.querySelector('#inner')`)
func (f *Frame) EleJS(expr string) *FrameElement {
	return f.Ele(JS(expr))
}

// ---------- 元信息 ----------

// Selector 返回该元素使用的选择器，便于日志与错误排查。
func (fe *FrameElement) Selector() Selector { return fe.sel }

// Frame 返回该元素所属的框架，便于在链式调用中回到框架级操作
// （如 frame.EleCSS("#tip").Frame().HTML(ctx)）。
func (fe *FrameElement) Frame() *Frame { return fe.frame }

// String 返回形如 css="#login" 的可读描述。
func (fe *FrameElement) String() string {
	if fe == nil {
		return "<nil frame element>"
	}
	return fmt.Sprintf("%s=%q", fe.sel.Mode(), fe.sel.String())
}

// ---------- 操作 ----------

// runOp 执行一次「定位元素 → 执行 body → 校验返回值」的框架内操作。
//
// Click / SetValue 的函数体不同，但外壳完全一样：先按选择器取 el，取不到就返回
// 'not found'，取到了再跑 body，最后交给 frameOpResult 翻译成 Go 错误。
// 把外壳收在这里，两侧只需要关心自己的 body，也不会再出现「漏判 el 为空」的变体。
//
// body 是拿到非空 el 之后要跑的语句，须自带 return（成功返回 'ok'）。
func (fe *FrameElement) runOp(ctx context.Context, action, body string) error {
	expr, err := fe.frame.requireExpr(fe.sel)
	if err != nil {
		return err
	}
	js := fmt.Sprintf(`(function(){ const el = %s; if (!el) return 'not found';
  %s })()`, expr, body)
	v, err := fe.frame.eval(ctx, js)
	if err != nil {
		return err
	}
	return frameOpResult(v, action, fe.sel)
}

// Click 在框架内触发元素点击（JS 语义，el.click()，不做可见性检查）。
//
// 元素不存在时返回 ErrElementNotFound——静默「成功」会让调用方以为点过了。
func (fe *FrameElement) Click(ctx context.Context) error {
	return fe.runOp(ctx, "点击", "el.click(); return 'ok';")
}

// Text 返回框架内该元素的文本（innerText）。
//
// 元素不存在时返回 ErrElementNotFound，而不是空字符串——
// 否则调用方无法区分「元素没有文字」和「元素根本没出现」。
func (fe *FrameElement) Text(ctx context.Context) (string, error) {
	expr, err := fe.frame.requireExpr(fe.sel)
	if err != nil {
		return "", err
	}
	v, err := fe.frame.eval(ctx, fmt.Sprintf(`(function(){ const el = %s; return el ? el.innerText : null; })()`, expr))
	if err != nil {
		return "", err
	}
	if v == nil {
		// 与标签页侧 firstNode 共用同一个「没命中」包装（noMatchError）：
		// 同一种失败在两条通道上必须给出同一种文案。
		return "", noMatchError(fe.sel)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// Count 返回框架内匹配该选择器的元素数量。
//
// 四种定位方式都支持：CSS 走 querySelectorAll，XPath 走 evaluate("count(...)")，
// ID / JS path 是「单个元素」语义，命中返回 1、未命中返回 0。
// 因此 Count 为 0 是正常返回值（不是错误），用 err 判断即可。
func (fe *FrameElement) Count(ctx context.Context) (int, error) {
	if err := fe.sel.validate(); err != nil {
		return 0, err
	}

	// 与 Element.Count 共用同一份「选择器 → 计数 JS」映射（见 selectorCountJS）。
	js := selectorCountJS(fe.sel)

	v, err := fe.frame.eval(ctx, js)
	if err != nil {
		return 0, err
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("无法解析元素数量: %v", v)
	}
}

// SetValue 在框架内给输入框赋值并触发 input/change 事件。
//
// 不产生键盘事件（与 Element.SetValue 一致），因此不受输入法、键盘布局、
// 后台节流影响；反过来说，只监听 keystroke 的组件不会被触发。
// 元素不存在时返回 ErrElementNotFound。
func (fe *FrameElement) SetValue(ctx context.Context, value string) error {
	body := fmt.Sprintf(`el.focus(); el.value = %q;
  el.dispatchEvent(new Event('input', {bubbles:true}));
  el.dispatchEvent(new Event('change', {bubbles:true}));
  return 'ok';`, value)
	return fe.runOp(ctx, "赋值", body)
}
