// Package network 是本库的网络监听纯逻辑层：Network 域事件的被动采集、请求/响应记录的存取，
// 以及「后台补取 body 并回填」这条异步骨架。
//
// 本包不感知 Tab / Browser（事件回调由 chromedp 直接派发），可脱离浏览器单独单测；与目标标签页
// 的绑定只靠传给 Start 的 ctx——它必须派生自 tab.Ctx。
//
// 包名 network 与上游 chromedp/cdproto/network 同名：按本仓库约定由上游让位，写成 cdpnetwork，
// 本包用干净语义名。
package network

import (
	"context"
	"sync"

	cdpnetwork "github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/errs"
)

// defaultMaxRecords 是 Listener 默认保留的最大记录条数。
//
// 监听常开（如爬虫挂一整天）时，记录只增不减会让内存随请求数线性上涨。默认给一个上限，
// 超出后按 FIFO 淘汰最旧记录。
const defaultMaxRecords = 1000

// Listener 使用 Network 域被动监听网络请求。
//
// 刻意不持 *Tab：监听所需的一切由 Start(ctx) 传入（ctx 派生自 tab.Ctx）。持有 Tab 会让本包
// 反向依赖上层（Tab 属于 chromium 门面 / page 实现），既构成 import 环，也破坏「记录存取是纯内存
// 逻辑、可脱离浏览器单测」。
type Listener struct {
	pattern string

	mu      sync.Mutex
	records map[string]*Record
	order   []string

	// extraHeaders 暂存 responseReceivedExtraInfo 带来的完整响应头，按 requestID 索引。
	//
	// 存在的原因：该事件与 responseReceived 的到达顺序不保证（CDP 文档明确说明），
	// 谁后到谁负责合并。合并完即删除；剩下的孤儿在 Clear / Stop 时整体清掉。
	extraHeaders map[string]cdpnetwork.Headers

	// maxRecords 是记录保留上限（FIFO 淘汰）；<= 0 表示不限制。
	maxRecords int
	ctx        context.Context // 监听生命周期上下文，由 Start(ctx) 派生
	cancel     context.CancelFunc
	started    bool // 防止重复 Start 覆盖 ctx/cancel 造成监听泄漏

	// 信号量：限制并发 chromedp.Run 调用数，避免 session 冲突
	sem chan struct{}

	// pending 是在途后台任务数，受 mu 保护。idle 是「在途任务归零」的广播通道：pending == 0 时
	// 它一定是已关闭状态。
	//
	// 这一对字段代替 sync.WaitGroup：WaitGroup 在此用法下会 panic——WaitIdle 天生要与不断到达
	// 的事件并发，而 Wait 被 Done 唤醒后、返回前存在窗口，此时若又有事件 Add(1)，标准库会
	// panic("WaitGroup is reused before previous Wait has returned")。已关闭的通道从不被复用，
	// 无此问题，也不需额外 goroutine，多个等待者能被同一次 close 一起唤醒。
	pending int
	idle    chan struct{}
}

// NewListener 创建一个网络监听器（pattern 为空表示全部命中）。
//
// 创建后不立即生效，需调用 Start(ctx)；传给 Start 的 ctx 必须派生自 tab.Ctx，否则命令落不到
// 目标标签页。重复 Start 返回 errs.ErrListenerStarted。
func NewListener(pattern string) *Listener {
	// 初始无在途任务，通道直接处于已关闭状态：WaitIdle / Stop 立刻返回
	idle := make(chan struct{})
	close(idle)

	return &Listener{
		pattern:    pattern,
		records:    map[string]*Record{},
		maxRecords: defaultMaxRecords,
		sem:        make(chan struct{}, 4), // 最多 4 个并发 CDP 调用
		idle:       idle,
	}
}

// MaxRecords 设置最多保留的记录条数，超出后按 FIFO 淘汰最旧的。
//
// 传 0 或负数表示不限制（仅在能确定监听时长很短时用，否则内存会随请求数一直涨）。返回 Listener
// 本身便于链式书写：
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
	// 一并清掉没等到 responseReceived 的孤儿条目，否则这份暂存表会一直涨。
	l.extraHeaders = nil
}

// Len 返回当前保留的记录条数。
func (l *Listener) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.order)
}

// trimLocked 把记录数裁剪到上限之内，需在 mu 保护下调用。
//
// order 用「尾部 append、头部 Reslice」实现：Reslice 同时缩小 len 与 cap，append 迟早触发重新
// 分配，底层数组不会无限增长——len 稳定在上限附近，cap 最多约两倍，摊还到每条记录只是一次指针拷贝。
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
// ctx 控制监听生命周期（应从 tab.Ctx 派生以携带 target 路由信息）；取消 ctx 或调用 Stop 均可停止。
// 重复调用返回错误：再次 Start 会覆盖 ctx/cancel，使上一轮监听永久失去控制句柄。
func (l *Listener) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return errs.ErrListenerStarted
	}
	l.ctx, l.cancel = context.WithCancel(ctx)
	l.started = true
	l.mu.Unlock()

	// 裸 context 会让 chromedp.ListenTarget 直接 panic（invalid context），提前拦成可诊断的错误。
	if chromedp.FromContext(l.ctx) == nil {
		l.mu.Lock()
		l.started = false
		cancel := l.cancel
		l.cancel = nil
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return errs.ErrInvalidContext
	}

	chromedp.ListenTarget(l.ctx, func(ev any) {
		switch e := ev.(type) {
		case *cdpnetwork.EventRequestWillBeSent:
			l.track(func() { l.handleRequest(e) })
		case *cdpnetwork.EventResponseReceived:
			l.track(func() { l.handleResponse(e) })
		case *cdpnetwork.EventLoadingFinished:
			l.track(func() { l.handleLoadingFinished(e) })
		case *cdpnetwork.EventResponseReceivedExtraInfo:
			l.track(func() { l.handleResponseExtraInfo(e) })
		}
	})

	err := chromedp.Run(l.ctx, cdpnetwork.Enable())
	if err != nil {
		// 启动失败要回滚状态：否则 started 卡在 true，调用方连重试机会都没有
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

// Stop 停止监听，等待后台任务完成。停止后该 Listener 可重新 Start（复用同一批已捕获记录）。
func (l *Listener) Stop() {
	l.mu.Lock()
	cancel := l.cancel
	l.cancel = nil
	l.started = false
	// 锁内取通道快照：started 一旦为 false，track 就不再换新通道，故这条通道一定在在途任务全部
	// 结束时被关闭，不会漏等。
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
// 「检查 started + 计数 + 换通道」必须整体放在 l.mu 里：事件派发是异步的，Stop 取消 ctx 后仍可能
// 有最后一个事件正在派发。做成原子步骤后，要么这里先拿到锁（计入 pending，一定会被 Stop / WaitIdle
// 等到），要么 Stop 先拿到锁把 started 置 false（这里直接丢弃该事件，不计数也不起 goroutine）。
func (l *Listener) track(fn func()) {
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	if l.pending == 0 {
		// 0 → 1：换一条新的、未关闭的通道；之后 WaitIdle / Stop 等的是它被关闭
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

// WaitIdle 等待所有已入队的后台 CDP 调用（GetResponseBody / GetRequestPostData）完成，但不停止
// 监听。适合在 Navigate 之后、Records 之前调用，确保 body 已回填。
//
// 语义边界：只保证「进入等待这一刻已入队的任务」收尾；等待期间新到达的事件仍会产生新任务，本方法
// 不去等它们（那等同于冻结监听）。当前无在途任务时立即返回。
//
// 等的是 idle 通道被 close，而非 sync.WaitGroup.Wait（后者与持续到达的事件并发会 panic，见 pending 字段注释）。
func (l *Listener) WaitIdle() {
	l.mu.Lock()
	idle := l.idle
	l.mu.Unlock()
	<-idle
}

// Records 返回当前保留的记录，按到达顺序。
//
// 返回深拷贝（为什么不能直接给内部指针，见 Record.clone）；可在监听进行中反复调用。
//
// 记录数受 MaxRecords 上限约束（默认 1000），超出后最旧的已被淘汰；需要全量请调大上限或用 Clear 分批取走。
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
