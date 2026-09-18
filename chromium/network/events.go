package network

import (
	"context"
	"encoding/base64"
	"fmt"

	cdpnetwork "github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// ---------- 事件处理 ----------

// recordLocked 取回指定 id 的记录，不存在则新建并登记。需在 mu 保护下调用。
//
// 新建时顺带裁剪记录上限：淘汰只发生在这里，保证「刚登记的这一条」永远不会
// 被同一次裁剪淘汰掉（trimLocked 只在 len > maxRecords 时才动手，且 maxRecords >= 1）。
func (l *Listener) recordLocked(id string) *Record {
	if rec, ok := l.records[id]; ok {
		return rec
	}
	rec := &Record{RequestID: id}
	l.records[id] = rec
	l.order = append(l.order, id)
	l.trimLocked()
	return rec
}

func (l *Listener) handleRequest(e *cdpnetwork.EventRequestWillBeSent) {
	if !l.matchURL(e.Request.URL) {
		return
	}

	id := string(e.RequestID)

	l.mu.Lock()
	rec := l.recordLocked(id)
	rec.URL = e.Request.URL
	rec.Method = e.Request.Method
	rec.RequestHeaders = flattenNetworkHeaders(e.Request.Headers)

	hasBody := false
	if len(e.Request.PostDataEntries) > 0 {
		b := e.Request.PostDataEntries[0].Bytes
		if b != "" {
			raw, err := base64.StdEncoding.DecodeString(b)
			if err == nil {
				rec.RequestBody = string(raw)
				hasBody = true
			} else {
				rec.RequestBody = b
				hasBody = true
			}
		}
	}
	l.mu.Unlock()

	// POST 请求但没有 PostDataEntries → 用 GetRequestPostData 兜底
	if !hasBody && isBodyMethod(e.Request.Method) {
		l.fetchBodyAsync(id,
			func(ctx context.Context) ([]byte, error) {
				return cdpnetwork.GetRequestPostData(e.RequestID).Do(ctx)
			},
			func(rec *Record, body []byte) {
				if rec.RequestBody == "" {
					rec.RequestBody = string(body)
				}
			})
	}
}

func (l *Listener) handleResponse(e *cdpnetwork.EventResponseReceived) {
	// 与 handleRequest 保持一致：不匹配 pattern 的响应直接丢弃，避免产生空壳记录
	if e.Response != nil && !l.matchURL(e.Response.URL) {
		return
	}

	id := string(e.RequestID)

	l.mu.Lock()
	defer l.mu.Unlock()

	// 如果 handleRequest 已经因不匹配而没建记录，这里也不建
	if _, exists := l.records[id]; !exists {
		if e.Response == nil || !l.matchURL(e.Response.URL) {
			return
		}
	}
	rec := l.recordLocked(id)

	if e.Response != nil {
		rec.Status = e.Response.Status
		rec.ResponseHeaders = flattenNetworkHeaders(e.Response.Headers)
		// URL 回退：如果 requestWillBeSent 还没到达，从 Response 取 URL
		if rec.URL == "" {
			rec.URL = e.Response.URL
		}
	}
}

// fetchBodyAsync 是「后台补取 body 并回填记录」骨架的唯一实现。
//
// 流程：track → 信号量进出 → chromedp.Run 里执行一次 CDP 调用 → 空体/失败直接丢弃 → 回填。
// 抽这一层是因为 handleRequest（补请求体）与 handleLoadingFinished（补响应体）
// 除了「取哪个 body、写哪个字段」之外完全相同。
//
// fetch 收到 chromedp 的 action 上下文（由 runCtx() 派生，带监听生命周期）；返回空体一律视为「没取到」，不回填。
// assign 在 l.mu 保护下以现存记录为参数被调用，由调用方决定「已填过就不覆盖」等判据；记录若已被
// Clear / FIFO 淘汰则直接跳过回填。
func (l *Listener) fetchBodyAsync(reqID string, fetch func(context.Context) ([]byte, error), assign func(*Record, []byte)) {
	l.track(func() {
		l.sem <- struct{}{}
		defer func() { <-l.sem }()

		var body []byte
		err := chromedp.Run(l.runCtx(), chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			body, err = fetch(ctx)
			return err
		}))
		if err != nil || len(body) == 0 {
			return
		}

		l.mu.Lock()
		defer l.mu.Unlock()
		if rec, ok := l.records[reqID]; ok {
			assign(rec, body)
		}
	})
}

func (l *Listener) handleLoadingFinished(e *cdpnetwork.EventLoadingFinished) {
	id := string(e.RequestID)

	l.mu.Lock()
	_, exists := l.records[id]
	if !exists {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	// 异步获取响应体（并发限流与回填逻辑见 fetchBodyAsync）
	l.fetchBodyAsync(id,
		func(ctx context.Context) ([]byte, error) {
			return cdpnetwork.GetResponseBody(e.RequestID).Do(ctx)
		},
		func(rec *Record, body []byte) {
			if rec.ResponseBody == "" {
				rec.ResponseBody = string(body)
			}
		})
}

func flattenNetworkHeaders(h cdpnetwork.Headers) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = fmt.Sprint(v)
	}
	return out
}
