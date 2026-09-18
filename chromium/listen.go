package chromium

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
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

// clone 深拷贝一条记录。
//
// Records() 必须返回拷贝而不是内部指针：后台 goroutine（GetResponseBody /
// GetRequestPostData 的异步回填）会在调用方读取期间继续写 Status / ResponseBody，
// 直接暴露内部指针会构成数据竞争（go test -race 可稳定复现）。
// map 字段同样要复制，否则调用方与后台仍共享同一底层表。
func (r *Record) clone() *Record {
	if r == nil {
		return nil
	}
	c := &Record{
		RequestID:    r.RequestID,
		URL:          r.URL,
		Method:       r.Method,
		RequestBody:  r.RequestBody,
		Status:       r.Status,
		ResponseBody: r.ResponseBody,
	}
	if r.RequestHeaders != nil {
		c.RequestHeaders = make(map[string]string, len(r.RequestHeaders))
		maps.Copy(c.RequestHeaders, r.RequestHeaders)
	}
	if r.ResponseHeaders != nil {
		c.ResponseHeaders = make(map[string]string, len(r.ResponseHeaders))
		maps.Copy(c.ResponseHeaders, r.ResponseHeaders)
	}
	return c
}

// defaultMaxRecords 是 Listener 默认保留的最大记录条数。
//
// 监听常开是常见用法（爬虫挂一整天），记录只增不减会让内存随请求数线性上涨，
// 最后被一条普通页面的几百个静态资源请求拖垮。默认给一个「够分析、不失控」的上限，
// 超出后按 FIFO 淘汰最旧的记录。
const defaultMaxRecords = 1000

// Listener 使用 Network 域被动监听网络请求
type Listener struct {
	tab     *Tab
	pattern string

	mu      sync.Mutex
	records map[string]*Record
	order   []string
	// maxRecords 是记录保留上限（FIFO 淘汰）；<= 0 表示不限制。
	maxRecords int
	ctx        context.Context // 监听生命周期上下文，由 Start(ctx) 派生
	cancel     context.CancelFunc
	started    bool // 防止重复 Start 覆盖 ctx/cancel 造成监听泄漏

	// 信号量：限制并发 chromedp.Run 调用数，避免 session 冲突
	sem chan struct{}

	// pending 是在途后台任务数，受 mu 保护。
	// idle 是「在途任务归零」的广播通道：pending == 0 时它一定是已关闭状态。
	//
	// 这一对字段代替了一开始使用的 sync.WaitGroup，原因是 WaitGroup 在这个用法下会 panic：
	// WaitIdle 天生要与不断到达的事件并发，而 Wait 被 Done 唤醒之后、返回之前存在一个窗口，
	// 此时若又有一批事件执行了 Add(1)，标准库在 Wait 收尾检查里看到计数重新非零，就会
	// panic("sync: WaitGroup is reused before previous Wait has returned")。
	// 已关闭的通道从不被复用，因此不存在「被 Wait 消费过又被重新计数」的状态；
	// 顺带也不需要任何 goroutine，多个等待者还能被同一次 close 一起唤醒。
	pending int
	idle    chan struct{}
}

// Listen 在 tab 上创建监听器
func (t *Tab) Listen(pattern string) *Listener {
	// 初始没有在途任务，通道直接处于已关闭状态：WaitIdle / Stop 立刻返回
	idle := make(chan struct{})
	close(idle)

	return &Listener{
		tab:        t,
		pattern:    pattern,
		records:    map[string]*Record{},
		maxRecords: defaultMaxRecords,
		sem:        make(chan struct{}, 4), // 最多 4 个并发 CDP 调用
		idle:       idle,
	}
}

// MaxRecords 设置最多保留的记录条数，超出后自动淘汰最旧的（FIFO）。
//
// 传 0 或负数表示不限制——只有在能确定监听时长很短时才这么用，
// 否则内存会随请求数一直涨。返回 Listener 本身，便于链式书写：
//
//	tab.Listen("api").MaxRecords(200).Start(ctx)
//
// 调小上限时会立即按新上限裁剪已捕获的记录。
func (l *Listener) MaxRecords(n int) *Listener {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxRecords = n
	l.trimLocked()
	return l
}

// Clear 丢弃已捕获的全部记录，但不停止监听。
// 适合「取走一批记录后立刻清空」的循环抓取场景，避免记录在内存里堆积。
func (l *Listener) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = map[string]*Record{}
	l.order = nil
}

// Len 返回当前保留的记录条数。
func (l *Listener) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.order)
}

// trimLocked 把记录数裁剪到上限之内，需在 mu 保护下调用。
//
// order 用「尾部 append、头部 Reslice」实现：Reslice 会同时缩小 len 与 cap，
// 于是 append 迟早触发重新分配，底层数组不会无限增长——len 稳定在上限附近，
// cap 最多约两倍，摊还到每条记录只是一次指针拷贝。
func (l *Listener) trimLocked() {
	if l.maxRecords <= 0 {
		return
	}
	for len(l.order) > l.maxRecords {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.records, oldest)
	}
}

// Start 启动监听，非阻塞。
// ctx 控制监听生命周期（应从 tab.Ctx 派生以携带 target 路由信息）；
// 取消 ctx 或调用 Stop 均可停止监听。
// 重复调用会返回错误：再次 Start 会覆盖 ctx/cancel，使上一轮监听永久失去控制句柄。
func (l *Listener) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return ErrListenerStarted
	}
	l.ctx, l.cancel = context.WithCancel(ctx)
	l.started = true
	l.mu.Unlock()

	// 裸 context 会让 chromedp.ListenTarget 直接 panic（invalid context）。
	// 提前拦住，把「ctx 传错了」变成一条可诊断的错误。
	if chromedp.FromContext(l.ctx) == nil {
		l.mu.Lock()
		l.started = false
		cancel := l.cancel
		l.cancel = nil
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return ErrInvalidContext
	}

	chromedp.ListenTarget(l.ctx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			l.track(func() { l.handleRequest(e) })
		case *network.EventResponseReceived:
			l.track(func() { l.handleResponse(e) })
		case *network.EventLoadingFinished:
			l.track(func() { l.handleLoadingFinished(e) })
		}
	})

	err := chromedp.Run(l.ctx, network.Enable())
	if err != nil {
		// 启动失败要回滚状态：否则 started 卡在 true，调用方连重试的机会都没有
		l.mu.Lock()
		l.started = false
		cancel := l.cancel
		l.cancel = nil
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return err
	}
	return nil
}

// Stop 停止监听，等待后台任务完成。
// 停止后该 Listener 可以重新 Start（复用同一批已捕获的记录）。
func (l *Listener) Stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.cancel = nil
	l.started = false
	// 锁内取通道快照：started 一旦为 false，track 就不会再换新通道，
	// 因此这条通道一定会在在途任务全部结束时被关闭，不会漏等。
	idle := l.idle
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	<-idle
}

// runCtx 在锁下取监听上下文，避免与 Start/Stop 并发读写 l.ctx 形成竞争。
func (l *Listener) runCtx() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctx
}

// track 在监听仍处于启动状态时登记一个后台任务，异步执行 fn。
//
// 为什么「检查 started + 计数 + 换通道」必须整体放在 l.mu 里：事件派发是异步的，
// Stop 取消 ctx 之后仍可能有最后一个事件正在派发。做成一个原子步骤后，要么这里
// 先拿到锁（于是计入 pending，一定会被 Stop / WaitIdle 等到），要么 Stop 先拿到锁
// 并把 started 置为 false（于是这里直接丢弃该事件，不计数也不起 goroutine）。
// 两条路都不会漏等，也不会空等。
func (l *Listener) track(fn func()) {
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	if l.pending == 0 {
		// 0 → 1：换一条新的、未关闭的通道。之后 WaitIdle / Stop 等的是它被关闭。
		l.idle = make(chan struct{})
	}
	l.pending++
	l.mu.Unlock()

	go func() {
		defer l.finish()
		fn()
	}()
}

// finish 在后台任务收尾时把在途计数减一；归零则关闭广播通道，唤醒全部等待者。
func (l *Listener) finish() {
	l.mu.Lock()
	l.pending--
	zero := l.pending == 0
	if zero {
		close(l.idle)
	}
	l.mu.Unlock()
}

// WaitIdle 等待所有已入队的后台 CDP 调用（GetResponseBody / GetRequestPostData）完成，
// 但不停止监听。适合在 Navigate 之后、Records 之前调用，确保 body 已回填。
//
// 语义边界：只保证「进入等待这一刻已经入队的任务」收尾；等待期间新到达的事件仍会产生
// 新任务，本方法不会去等它们（那等同于冻结监听）。当前没有在途任务时立即返回。
//
// 实现说明：等的是 idle 通道被 close，而不是 sync.WaitGroup.Wait ——
// 后者与持续到达的事件并发时会 panic，原因见 Listener.pending 字段的注释。
func (l *Listener) WaitIdle() {
	l.mu.Lock()
	idle := l.idle
	l.mu.Unlock()
	<-idle
}

// Records 返回当前保留的记录，按到达顺序。
//
// 返回的是深拷贝：后台异步回填仍在运行，直接返回内部指针会让调用方读到
// 正在被写入的字段（数据竞争）。调用方可以安全地在监听进行中反复读取。
//
// 记录数受 MaxRecords 上限约束（默认 1000 条），超出后最旧的已被淘汰——
// 需要完整全量请调大上限或用 Clear 分批取走。
func (l *Listener) Records() []*Record {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]*Record, 0, len(l.order))
	for _, id := range l.order {
		if rec, ok := l.records[id]; ok {
			out = append(out, rec.clone())
		}
	}
	return out
}

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

func (l *Listener) handleRequest(e *network.EventRequestWillBeSent) {
	if !l.matchURL(e.Request.URL) {
		return
	}

	id := string(e.RequestID)

	l.mu.Lock()
	rec := l.recordLocked(id)
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
		l.fetchBodyAsync(id,
			func(ctx context.Context) ([]byte, error) {
				return network.GetRequestPostData(e.RequestID).Do(ctx)
			},
			func(rec *Record, body []byte) {
				if rec.RequestBody == "" {
					rec.RequestBody = string(body)
				}
			})
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

// fetchBodyAsync 是「后台补取 body 并回填记录」这条骨架的唯一实现。
//
// track → 信号量进出 → chromedp.Run 里执行一次 CDP 调用 → 空体/失败直接丢弃 → 回填。
// handleRequest（补请求体）与 handleLoadingFinished（补响应体）除了取哪个 body、
// 写哪个字段之外完全一样，抄两遍就会漂移。
//
// fetch 收到的是 chromedp 的 action 上下文（由 runCtx() 派生，带监听生命周期）；
// 返回空体一律视为「没取到」，不做回填——对两个调用方都是正确的。
//
// assign 在 l.mu 保护下、以「按 reqID 查到的现存记录」为参数被调用，由调用方决定
// 「已经填过就不覆盖」等判据。记录若已被 Clear / FIFO 淘汰则直接跳过回填：
// 写进一条已不在记录表里的对象没有意义。
func (l *Listener) fetchBodyAsync(reqID string, fetch func(context.Context) ([]byte, error), assign func(*Record, []byte)) {
	l.track(func() {
		// 信号量限并发；defer 保证任何返回路径都释放，不会把限量永久占住
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

func (l *Listener) handleLoadingFinished(e *network.EventLoadingFinished) {
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
			return network.GetResponseBody(e.RequestID).Do(ctx)
		},
		func(rec *Record, body []byte) {
			if rec.ResponseBody == "" {
				rec.ResponseBody = string(body)
			}
		})
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
