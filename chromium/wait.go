package chromium

import (
	"context"
	"time"

	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// waitCond 表示 WaitBuilder 的等待条件类型
type waitCond int

const (
	condUnset   waitCond = iota
	condReady            // 页面 body 就绪（页面级）
	condVisible          // 元素可见（元素级）
	condPresent          // 元素存在，数量 >= 1（元素级）
	condText             // 元素文本包含子串（元素级）
	condCount            // 匹配元素数量 >= n（元素级）
	condURL              // 当前 URL 包含子串（页面级）
)

// WaitBuilder 是链式等待构建器，用流式 API 组织「等什么 + 等多久」。
//
// 它有两个入口，对应两种作用域：
//
//	// 元素级：等某个元素可见 / 出现 / 文本变化 / 数量达标
//	tab.EleCSS("#loading").Wait().Visible().Timeout(10 * time.Second).Do(ctx)
//	tab.EleXPath("//li").Wait().Count(5).Do(ctx)
//	tab.EleID("status").Wait().Text("已完成").Do(ctx)
//
//	// 页面级：等 URL 变化 / 页面就绪
//	tab.Wait().URL("/dashboard").Do(ctx)
//	tab.Wait().Ready().Do(ctx)
//
// 元素级条件必须由 Element.Wait() 提供元素；在 Tab.Wait() 上直接调
// Visible/Text/Count/Present 会返回 ErrSelectorRequired（而不是静默失败）。
// 若确实需要从 Tab 侧组装，可以用 Wait().Element(el) 显式补上元素。
//
// 超时优先级：若调用 Timeout(d) 设定了正值，Do 会在传入 ctx 之上再派生一个带
// d 超时的 ctx；否则完全沿用传入 ctx 的 deadline（符合「超时由调用方掌控」的约定）。
type WaitBuilder struct {
	tab  *Tab
	el   *Element // 元素级条件的来源；页面级条件为 nil
	cond waitCond

	substr  string
	count   int
	timeout time.Duration
}

// Wait 返回一个绑定到该标签页的链式等待构建器（页面级条件：URL / Ready）。
//
// 元素级等待请用 el.Wait()，例如 tab.EleCSS("#btn").Wait().Visible().Do(ctx)。
func (t *Tab) Wait() *WaitBuilder {
	return &WaitBuilder{tab: t}
}

// Element 显式指定本次等待作用于哪个元素，用于从 Tab 侧组装等待条件：
//
//	el := tab.EleCSS("#btn")
//	tab.Wait().Element(el).Visible().Do(ctx)
//
// 日常写法请直接用 el.Wait()。
func (w *WaitBuilder) Element(el *Element) *WaitBuilder {
	w.el = el
	return w
}

// Visible 等待元素可见（需有元素，见 Element）。
func (w *WaitBuilder) Visible() *WaitBuilder {
	w.cond = condVisible
	return w
}

// Present 等待元素存在于 DOM（数量 >= 1，需有元素）。
// 与 Visible 的区别：Present 不要求可见，只要求节点在。
func (w *WaitBuilder) Present() *WaitBuilder {
	w.cond = condPresent
	return w
}

// Text 等待元素文本包含 substr（需有元素）。
func (w *WaitBuilder) Text(substr string) *WaitBuilder {
	w.cond = condText
	w.substr = substr
	return w
}

// Count 等待元素匹配数量至少 n 个（需有元素），适合「列表渲染完成」这类场景。
func (w *WaitBuilder) Count(n int) *WaitBuilder {
	w.cond = condCount
	w.count = n
	return w
}

// URL 等待当前地址包含 substr（页面级条件，无需元素）。
func (w *WaitBuilder) URL(substr string) *WaitBuilder {
	w.cond = condURL
	w.substr = substr
	return w
}

// Ready 等待页面 body 就绪（页面级条件，无需元素）。
func (w *WaitBuilder) Ready() *WaitBuilder {
	w.cond = condReady
	return w
}

// Timeout 设定整体等待超时。d <= 0 视为不额外设超时，沿用传入 ctx 的 deadline。
func (w *WaitBuilder) Timeout(d time.Duration) *WaitBuilder {
	w.timeout = d
	return w
}

// Do 执行等待，超时或条件不满足时如实返回 error。
//
// 元素级条件缺少元素、或元素的选择器为空时返回 ErrSelectorRequired；
// 未指定任何条件时返回 ErrWaitConditionUnset；
// 元素始终没出现时返回包装过的 ErrElementNotFound。
func (w *WaitBuilder) Do(ctx context.Context) error {
	// 元素级条件先校验元素，避免把「忘了给元素」变成一个含糊的超时
	switch w.cond {
	case condVisible, condPresent, condText, condCount:
		if w.el == nil {
			return errs.ErrSelectorRequired
		}
		if err := w.el.sel.validate(); err != nil {
			return err
		}
	case condUnset:
		return errs.ErrWaitConditionUnset
	default:
		// condReady / condURL 为页面级条件，无需元素，校验通过
	}

	runCtx := ctx
	if w.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, w.timeout)
		defer cancel()
	}

	switch w.cond {
	case condReady:
		return w.tab.WaitReady(runCtx)
	case condURL:
		return w.tab.WaitURL(runCtx, w.substr)
	case condVisible:
		return w.el.WaitVisible(runCtx)
	case condText:
		return w.el.WaitText(runCtx, w.substr)
	case condPresent:
		return w.tab.waitElementCount(runCtx, w.el.sel, 1)
	case condCount:
		return w.tab.waitElementCount(runCtx, w.el.sel, w.count)
	default:
		return errs.ErrWaitConditionUnset
	}
}
