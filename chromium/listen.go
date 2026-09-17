package chromium

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Record 一条完整的请求/响应记录
type Record struct {
	RequestID string
	URL       string
	Method    string

	RequestHeaders map[string]string
	RequestBody    string

	Status          int64
	ResponseHeaders map[string]string
	ResponseBody    string
}

// Listener 使用 Network 域被动监听网络请求
type Listener struct {
	tab     *Tab
	pattern string

	mu      sync.Mutex
	records map[string]*Record
	order   []string
	ctx     context.Context // 监听生命周期上下文，由 Start(ctx) 派生
	cancel  context.CancelFunc

	// 信号量：限制并发 chromedp.Run 调用数，避免 session 冲突
	sem chan struct{}
	// wg 等待所有后台任务完成
	wg sync.WaitGroup
}

// Listen 在 tab 上创建监听器
func (t *Tab) Listen(pattern string) *Listener {
	return &Listener{
		tab:     t,
		pattern: pattern,
		records: map[string]*Record{},
		sem:     make(chan struct{}, 4), // 最多 4 个并发 CDP 调用
	}
}

// Start 启动监听，非阻塞。
// ctx 控制监听生命周期（应从 tab.Ctx 派生以携带 target 路由信息）；
// 取消 ctx 或调用 Stop 均可停止监听。
func (l *Listener) Start(ctx context.Context) error {
	l.ctx, l.cancel = context.WithCancel(ctx)

	chromedp.ListenTarget(l.ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			l.wg.Add(1)
			go func() { defer l.wg.Done(); l.handleRequest(e) }()
		case *network.EventResponseReceived:
			l.wg.Add(1)
			go func() { defer l.wg.Done(); l.handleResponse(e) }()
		case *network.EventLoadingFinished:
			l.wg.Add(1)
			go func() { defer l.wg.Done(); l.handleLoadingFinished(e) }()
		}
	})

	return chromedp.Run(l.ctx, network.Enable())
}

// Stop 停止监听，等待后台任务完成
func (l *Listener) Stop() {
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
}

// WaitIdle 等待所有已入队的后台 CDP 调用（GetResponseBody / GetRequestPostData）完成，
// 但不停止监听。适合在 Navigate 之后、Records 之前调用，确保 body 已回填。
func (l *Listener) WaitIdle() {
	l.wg.Wait()
}

// Records 返回所有已捕获的记录，按到达顺序
func (l *Listener) Records() []*Record {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]*Record, 0, len(l.order))
	for _, id := range l.order {
		if rec, ok := l.records[id]; ok {
			out = append(out, rec)
		}
	}
	return out
}

// ---------- 事件处理 ----------

func (l *Listener) handleRequest(e *network.EventRequestWillBeSent) {
	if !l.matchURL(e.Request.URL) {
		return
	}

	id := string(e.RequestID)

	l.mu.Lock()
	rec, exists := l.records[id]
	if !exists {
		rec = &Record{RequestID: id}
		l.records[id] = rec
		l.order = append(l.order, id)
	}
	rec.URL = e.Request.URL
	rec.Method = e.Request.Method
	rec.RequestHeaders = flattenNetworkHeaders(e.Request.Headers)

	// 请求体：优先从 PostDataEntries 获取
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
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.sem <- struct{}{}
			defer func() { <-l.sem }()

			var postData []byte
			err := chromedp.Run(l.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				postData, err = network.GetRequestPostData(e.RequestID).Do(ctx)
				return err
			}))
			if err != nil || len(postData) == 0 {
				return
			}

			l.mu.Lock()
			if rec.RequestBody == "" {
				rec.RequestBody = string(postData)
			}
			l.mu.Unlock()
		}()
	}
}

func (l *Listener) handleResponse(e *network.EventResponseReceived) {
	// 与 handleRequest 保持一致：不匹配 pattern 的响应直接丢弃，避免产生空壳记录
	if e.Response != nil && !l.matchURL(e.Response.URL) {
		return
	}

	id := string(e.RequestID)

	l.mu.Lock()
	defer l.mu.Unlock()

	// 如果 handleRequest 已经因不匹配而没建记录，这里也不建
	rec, exists := l.records[id]
	if !exists {
		if e.Response == nil || !l.matchURL(e.Response.URL) {
			return
		}
		rec = &Record{RequestID: id}
		l.records[id] = rec
		l.order = append(l.order, id)
	}

	if e.Response != nil {
		rec.Status = e.Response.Status
		rec.ResponseHeaders = flattenNetworkHeaders(e.Response.Headers)
		// URL 回退：如果 requestWillBeSent 还没到达，从 Response 取 URL
		if rec.URL == "" {
			rec.URL = e.Response.URL
		}
	}
}

func (l *Listener) handleLoadingFinished(e *network.EventLoadingFinished) {
	id := string(e.RequestID)

	l.mu.Lock()
	_, exists := l.records[id]
	if !exists {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	// 异步获取响应体，用信号量控制并发
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.sem <- struct{}{}
		defer func() { <-l.sem }()

		var body []byte
		err := chromedp.Run(l.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			body, err = network.GetResponseBody(e.RequestID).Do(ctx)
			return err
		}))
		if err != nil {
			return
		}

		l.mu.Lock()
		if rec, ok := l.records[id]; ok && rec.ResponseBody == "" {
			rec.ResponseBody = string(body)
		}
		l.mu.Unlock()
	}()
}

// ---------- 辅助 ----------

// isBodyMethod 判断是否可能有请求体的 HTTP 方法
func isBodyMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH"
}

// matchURL 采用子串包含匹配：pattern 为空则全部命中，否则 URL 包含 pattern 即命中。
func (l *Listener) matchURL(u string) bool {
	if l.pattern == "" {
		return true
	}
	return strings.Contains(u, l.pattern)
}

func flattenNetworkHeaders(h network.Headers) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = fmt.Sprint(v)
	}
	return out
}
