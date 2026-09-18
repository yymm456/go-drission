package page

import (
	"context"
	"fmt"
)

// FrameElement 是「iframe 内的一个待操作元素」：选择器 + 所属框架。
//
// 与 Element 是镜像关系：Element 之于 Tab，即 FrameElement 之于 Frame。差别在底层通道——
// Element 走 CDP DOM 域（真实鼠标 / 键盘事件），FrameElement 走框架 isolated world 里的 JS。
//
// 同样把「怎么找」和「做什么」拆成两步，选择器只出现一次：
//
//	frame, _ := tab.Frame(ctx, chromium.CSS("iframe#login"))
//	frame.EleID("username").SetValue(ctx, "xxx")
//	frame.EleCSS(".submit").Click(ctx)
//	title, _ := frame.EleXPath(`//h1`).Text(ctx)
//
// # 语义约束（与 Element 的差异）
//
//   - 操作全部在框架 isolated world 内用 JS 完成：Click 是 el.click()，Text 读 el.innerText，
//     不经 CDP DOM 域，故跨域 iframe 也能操作。
//   - Click 不做可见性检查：元素被遮挡、隐藏、零尺寸都照点，也不产生 mousedown/mouseup 序列。
//     要真实鼠标事件改用 Tab 上的 Element（走 input 域），但它进不了跨域 iframe。
//   - 不缓存任何节点引用：每次操作都用选择器在框架内重新求值（本通道不经 CDP DOM 域，不存在 nodeID）。
//
// # 错误语义（均可用 errors.Is 判断）
//
//   - 选择器为空（如 EleCSS("")）→ ErrSelectorRequired
//   - 元素不存在 / 框架内脚本返回 'not found' → ErrElementNotFound
//   - 框架已随导航失效且重新定位失败 → ErrFrameDetached
//
// 各操作接受调用方 ctx（应从 tab.Ctx 派生），超时与取消由调用方掌控。
type FrameElement struct {
	frame *Frame
	sel   Selector
}

// ---------- 查询入口（Frame 上）----------

// Ele 用显式选择器构造框架内元素。
//
//	frame.Ele(chromium.CSS("#login"))
//	frame.Ele(chromium.XPath("//button[text()='登录']"))
//
// 一般用 EleCSS / EleID / EleXPath / EleJS 更方便。
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

// runOp 是 Click / SetValue 共用的外壳：el 定位与判空、返回值翻译集中在此，两侧只提供 body。
//
// body 是拿到非空 el 后要跑的语句，须自带 return（成功返回 'ok'）。
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
// 元素不存在时返回 ErrElementNotFound，避免静默「成功」让调用方误以为点过了。
func (fe *FrameElement) Click(ctx context.Context) error {
	return fe.runOp(ctx, "点击", "el.click(); return 'ok';")
}

// Text 返回框架内该元素的文本（innerText）。
// 元素不存在时返回 ErrElementNotFound 而非空字符串，以便区分「无文字」与「未出现」。
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
		// 与标签页侧 firstNode 共用同一「没命中」包装（noMatchError）：同种失败在两条通道给出同种文案。
		return "", noMatchError(fe.sel)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// Count 返回框架内匹配该选择器的元素数量。
//
// 四种定位方式都支持（模式到 JS 的映射见 selectorCountJS）。Count 为 0 是正常返回值（非错误），用 err 判断即可。
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
// 不产生键盘事件（与 Element.SetValue 一致），不受输入法、键盘布局、后台节流影响；
// 只监听 keystroke 的组件不会触发。元素不存在时返回 ErrElementNotFound。
func (fe *FrameElement) SetValue(ctx context.Context, value string) error {
	body := fmt.Sprintf(`el.focus(); el.value = %q;
  el.dispatchEvent(new Event('input', {bubbles:true}));
  el.dispatchEvent(new Event('change', {bubbles:true}));
  return 'ok';`, value)
	return fe.runOp(ctx, "赋值", body)
}
