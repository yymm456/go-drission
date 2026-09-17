package chromium

import (
	"context"
	"fmt"
	"time"
)

// waitCond 表示 WaitBuilder 的等待条件类型
type waitCond int

const (
	condUnset   waitCond = iota
	condReady            // 页面 body 就绪
	condVisible          // 元素可见
	condPresent          // 元素存在（数量 >= 1）
	condText             // 元素文本包含子串
	condCount            // 匹配元素数量 >= n
	condURL              // 当前 URL 包含子串
)

// WaitBuilder 是链式等待构建器，用流式 API 组织「等什么 + 等多久」。
// 它是 Tab 上各 WaitXxx 方法之上的语法糖，最终仍复用它们执行，超时语义保持一致。
//
// 典型用法：
//
//	tab.Wait().Element("#btn").Visible().Timeout(10 * time.Second).Do(ctx)
//	tab.Wait().Element("li").Count(5).Do(ctx)
//	tab.Wait().Element("#status").Text("完成").Do(ctx)
//	tab.Wait().URL("dashboard").Do(ctx)
//	tab.Wait().Ready().Do(ctx)
//
// 超时优先级：若调用 Timeout(d) 设定了正值，Do 会在传入 ctx 之上再派生一个带
// d 超时的 ctx；否则完全沿用传入 ctx 的 deadline（符合「超时由调用方掌控」的约定）。
type WaitBuilder struct {
	tab      *Tab
	cond     waitCond
	selector string
	substr   string
	count    int
	timeout  time.Duration
}

// Wait 返回一个链式等待构建器。
func (t *Tab) Wait() *WaitBuilder {
	return &WaitBuilder{tab: t}
}

// Element 设定目标选择器，供 Visible / Present / Text / Count 使用。
func (w *WaitBuilder) Element(selector string) *WaitBuilder {
	w.selector = selector
	return w
}

// Visible 等待元素可见（需先 Element 指定选择器）。
func (w *WaitBuilder) Visible() *WaitBuilder {
	w.cond = condVisible
	return w
}

// Present 等待元素存在于 DOM（数量 >= 1，需先 Element 指定选择器）。
func (w *WaitBuilder) Present() *WaitBuilder {
	w.cond = condPresent
	return w
}

// Text 等待元素文本包含 substr（需先 Element 指定选择器）。
func (w *WaitBuilder) Text(substr string) *WaitBuilder {
	w.cond = condText
	w.substr = substr
	return w
}

// Count 等待选择器匹配到至少 n 个元素（需先 Element 指定选择器）。
func (w *WaitBuilder) Count(n int) *WaitBuilder {
	w.cond = condCount
	w.count = n
	return w
}

// URL 等待当前地址包含 substr（独立条件，无需 Element）。
func (w *WaitBuilder) URL(substr string) *WaitBuilder {
	w.cond = condURL
	w.substr = substr
	return w
}

// Ready 等待页面 body 就绪（独立条件，无需 Element）。
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
func (w *WaitBuilder) Do(ctx context.Context) error {
	// 需要选择器的条件先做校验，避免把空选择器传到底层导致语义含糊
	switch w.cond {
	case condVisible, condPresent, condText, condCount:
		if w.selector == "" {
			return fmt.Errorf("wait: 条件需要先调用 Element(selector) 指定选择器")
		}
	case condUnset:
		return fmt.Errorf("wait: 未指定等待条件（Visible/Present/Text/Count/URL/Ready 之一）")
	default:
		// condReady / condURL 为独立条件，无需选择器，校验通过
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
	case condVisible:
		return w.tab.WaitVisible(runCtx, w.selector)
	case condPresent:
		return w.tab.WaitCount(runCtx, w.selector, 1)
	case condText:
		return w.tab.WaitText(runCtx, w.selector, w.substr)
	case condCount:
		return w.tab.WaitCount(runCtx, w.selector, w.count)
	case condURL:
		return w.tab.WaitURL(runCtx, w.substr)
	default:
		return fmt.Errorf("wait: 未知的等待条件")
	}
}
