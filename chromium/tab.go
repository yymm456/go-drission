package chromium

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// discardLogger 是 Tab 未配置 logger 时的兜底：丢弃所有日志，不污染调用方输出。
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// Tab 代表一个被托管的标签页。
// Ctx 是该标签页的 chromedp 会话根上下文，承载 target 路由信息。
// 所有 I/O 方法都接受调用方传入的 ctx（应从 Ctx 派生），由调用方控制超时与取消。
//
// # 可变状态
//
// url / timeout 由 mu 保护。之所以必须保护 url：Browser 同步 target
// 列表时会在另一个 goroutine 里改写它，而调用方习惯直接读「当前地址」。早期 url 是
// 导出字段，这种裸读写构成数据竞争——go test -race 才能抓到，人工很难发现。
// 读取请用 URL()，写入只有包内 setURL。
//
// antiDetected 由独立的 antiMu 保护：它的判断与置位必须和注入动作一起串行化，
// 见 ensureAntiDetect。
type Tab struct {
	ID     target.ID
	Ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
	// url 是最近一次同步到的地址快照；可能滞后于页面真实地址，需要实时值请用 CurrentURL。
	url string
	// timeout 是内置默认超时（来自 WithDefaultTimeout，默认 30s）。
	// 仅当调用方传入的 ctx 没有 deadline 时才会套用；调用方自己设了超时就听调用方的。
	// 0 表示关闭内置超时。
	timeout time.Duration

	// antiMu 串行化反检测脚本的注入，保证 check-then-inject 是原子的。
	antiMu sync.Mutex
	// antiDetected 标记该标签页是否已注入反检测初始化脚本，用于保证注入幂等。
	antiDetected bool

	// logger 用于库内部告警（如 iframe 绑定失败）。默认静默，随 Browser 的配置传入。
	logger *slog.Logger
}

// log 返回可用的 logger；调用方直接构造 Tab 时可能为 nil，这里兜底成静默 logger。
func (t *Tab) log() *slog.Logger {
	if t.logger != nil {
		return t.logger
	}
	return discardLogger
}

// URL 返回最近一次同步到的页面地址快照，并发安全。
//
// 这个值由 Browser 同步 target 列表时写入，因此可能滞后（尤其是导航刚发生时）。
// 需要「此刻浏览器里真实显示的地址」请用 CurrentURL，它走一次 CDP 拿实时值。
func (t *Tab) URL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.url
}

// setURL 记录地址快照，仅供包内同步 target 列表时使用。
func (t *Tab) setURL(u string) {
	t.mu.Lock()
	t.url = u
	t.mu.Unlock()
}

// SetTimeout 调整该标签页的内置默认超时。
// 只影响「调用方未设 deadline」的那些调用；传 0 关闭内置超时。
func (t *Tab) SetTimeout(d time.Duration) {
	if d < 0 {
		d = 0
	}
	t.mu.Lock()
	t.timeout = d
	t.mu.Unlock()
}

// applyTimeout 在 ctx 无 deadline 时套用标签页内置超时。
//
// 取 t.timeout 需要加锁，真正的判定交给 withDefaultTimeout，避免「不覆盖调用方 deadline」
// 这条规则在两个地方各写一遍。
// 返回的 cancel 必须由调用方 defer 调用。
func (t *Tab) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	t.mu.RLock()
	timeout := t.timeout
	t.mu.RUnlock()

	return withDefaultTimeout(ctx, timeout)
}

// safeCtx 对调用方误传「裸 context」（不含 chromedp 路由信息）时的兜底：
// chromedp.Run 收到裸 context 会另起一个临时浏览器执行命令，命令根本落不到本标签页。
// 检测到裸 context 时回退到标签页自身的会话上下文；若裸 context 带 deadline 则保留其超时语义，
// 否则套用标签页内置超时。
//
// 返回的 cancel 必须由调用方 defer 调用；未派生新上下文时是空操作，调用方无需分情况处理。
func (t *Tab) safeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if chromedp.FromContext(ctx) == nil {
		if t.Ctx == nil {
			return ctx, noopCancel
		}
		if dl, ok := ctx.Deadline(); ok {
			// 父上下文只能是 t.Ctx：chromedp 的路由信息挂在 t.Ctx 这条链上，
			// 换别的父节点命令就落不到本标签页。
			c, cancel := context.WithDeadline(t.Ctx, dl)
			// 代价是调用方对 ctx 的取消不再自动传导到 c（c 的父已经不是 ctx 了）。
			// AfterFunc 把这条边补回来：ctx 结束时触发 cancel，
			// 而 run 返回前会 defer cancel()，其中 stop() 先解除注册，
			// 所以正常路径下不会留下常驻 goroutine。
			stop := context.AfterFunc(ctx, cancel)
			return c, func() {
				stop()
				cancel()
			}
		}
		return t.applyTimeout(t.Ctx)
	}
	// 已带路由信息：仅在调用方没设超时时补内置超时
	return t.applyTimeout(ctx)
}

// run 是本包执行 CDP 命令的唯一入口：用 safeCtx 校正上下文，命令结束即刻回收派生上下文。
//
// 有了它，各方法不必写成 chromedp.Run(t.safeCtx(ctx), ...)——那种写法拿不到 cancel，
// 派生上下文的超时定时器要一直挂到 deadline 才释放，go vet 的 lostcancel 也会报出来。
// 集中在这里 defer 一次，调用点保持一行。
func (t *Tab) run(ctx context.Context, actions ...chromedp.Action) error {
	c, cancel := t.safeCtx(ctx)
	defer cancel()
	return chromedp.Run(c, actions...)
}

// Navigate 导航到 url，等待页面 load 完成。
// 超时与取消由 ctx 控制；导航失败（含超时）如实返回 error，不吞掉。
func (t *Tab) Navigate(ctx context.Context, url string) error {
	return t.run(ctx, chromedp.Navigate(url))
}

// Title 返回当前页面标题
func (t *Tab) Title(ctx context.Context) (string, error) {
	var title string
	err := t.run(ctx, chromedp.Title(&title))
	return title, err
}

// CurrentURL 返回当前页面地址
func (t *Tab) CurrentURL(ctx context.Context) (string, error) {
	var u string
	err := t.run(ctx, chromedp.Location(&u))
	return u, err
}

// HTML 返回当前页面完整 HTML
func (t *Tab) HTML(ctx context.Context) (string, error) {
	var html string
	err := t.run(ctx, chromedp.OuterHTML("html", &html))
	return html, err
}

// Eval 执行 JavaScript 并返回原始结果。
// 返回值按 JSON 语义解码：字符串→string、数字→float64、对象→map[string]any、
// 数组→[]any、布尔→bool、null→nil。
func (t *Tab) Eval(ctx context.Context, js string) (any, error) {
	var result any
	err := t.run(ctx, chromedp.Evaluate(js, &result))
	return result, err
}

// ---------- 等待方法（超时统一由 ctx 控制）----------

// WaitReady 等待页面加载完成（body 就绪）
func (t *Tab) WaitReady(ctx context.Context) error {
	return t.run(ctx, chromedp.WaitReady("body", chromedp.ByQuery))
}

// waitPollInterval 是轮询式等待（WaitURL / waitElementCount / Element.WaitText）的间隔。
const waitPollInterval = 200 * time.Millisecond

// waitFatalErr 判定轮询等待途中出现的错误是否可以直接中止，不必等到超时。
//
// 判据只有一条：**再等下去结论也不会变**。目前满足这一条的只有本库自己产生的错误——
// 入参不合法（空选择器），以及连接 / 上下文已经没了。这类错误重试一万次还是同一个。
//
// 从 CDP 回来的错误一律当作可重试，哪怕看着像「不可能恢复」：页面导航期间 CDP 必然抛
// "execution context destroyed" / "Inspected target navigated or closed"，而
// WaitURL / Present / Count 恰恰经常就是跨着一次导航在等（点了按钮等新页面）。
// 靠匹配错误文本来挑出「永久性 CDP 错误」既脆弱，又会把正常等待变成偶发失败。
// 这类错误只记录不中止，超时时会一并出现在返回值里，诊断信息不丢。
//
// ErrElementNotFound 刻意不在名单里：元素还没出现正是等待本身的目标。
//
// 今天从这三个轮询循环里真正够得着的只有 ErrSelectorRequired（公开的 Element.WaitText
// 会先撞上校验）；其余三个同属「连接已经没了」这一类，一并留在这里是为了让判定规则只有
// 一处，而不是等它们真的可达时再补。四条分支都有 TestWaitFatalErr 覆盖。
func waitFatalErr(err error) bool {
	return errors.Is(err, ErrSelectorRequired) ||
		errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrNotConnected) ||
		errors.Is(err, ErrContextClosed)
}

// waitRecordErr 记下轮询期间最后一次「非 ctx 终止」的错误。
//
// ctx 自身的取消 / 超时不算：那由 select 的 ctx.Done() 分支统一处理，
// 记下来只会让最终错误里同一个原因出现两遍。
func waitRecordErr(last, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return last
	}
	return err
}

// waitTimeoutErr 构造「等待超时」错误，并把期间最后一次底层错误一并带上。
//
// why：不带的话调用方只能看到「等待超时」，而背后一直在报的
// "invalid context"、CSS 语法错误之类全被吞掉，排查时根本无从下手。
// 两个 %w（Go 1.20+）让 ctx 错误与底层错误都留在错误链里：
// errors.Is(err, context.DeadlineExceeded) 照旧成立，调用方原有判断不受影响。
func waitTimeoutErr(what string, ctxErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("%s 失败: %w", what, ctxErr)
	}
	return fmt.Errorf("%s 失败: %w（期间最后一次错误: %w）", what, ctxErr, lastErr)
}

// pollWait 是三个轮询式等待（WaitURL / waitElementCount / Element.WaitText）共用的循环骨架。
//
// 它们只差「判什么条件」与「错误文案」，三份循环各写一遍等于把下面这套错误分诊规则
// 复制了三份——而分诊规则恰恰是本库最容易改错的地方（见 waitFatalErr 的说明）。
//
// 语义：
//   - probe 返回 (true, _) 即成功返回；err 只在未命中时才有意义；
//   - probe 的错误命中 waitFatalErr（再等也不会变）→ 立即中止，文案 "<what> 失败: <err>"；
//   - ctx 结束 → 用 waitTimeoutErr 拼「超时 + 期间最后一次错误」；
//     notFound 为 true 时再套一层 ErrElementNotFound（「始终没出现」是调用方要 errors.Is 的语义）。
func pollWait(ctx context.Context, what string, notFound bool, probe func() (bool, error)) error {
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		ok, err := probe()
		if ok {
			return nil
		}
		if waitFatalErr(err) {
			return fmt.Errorf("%s 失败: %w", what, err)
		}
		lastErr = waitRecordErr(lastErr, err)

		select {
		case <-ctx.Done():
			timeoutErr := waitTimeoutErr(what, ctx.Err(), lastErr)
			if notFound {
				return fmt.Errorf("%w: %w", ErrElementNotFound, timeoutErr)
			}
			return timeoutErr
		case <-ticker.C:
		}
	}
}

// WaitURL 轮询等待当前 URL 包含指定子串，直到 ctx 结束
func (t *Tab) WaitURL(ctx context.Context, substr string) error {
	// 循环骨架与错误分诊规则见 pollWait（复用同一个 Ticker 而非每轮 time.After 的理由也在那里）。
	return pollWait(ctx, fmt.Sprintf("等待 URL 包含 %q", substr), false, func() (bool, error) {
		u, err := t.CurrentURL(ctx)
		return err == nil && strings.Contains(u, substr), err
	})
}

// waitElementCount 轮询等待选择器匹配到至少 n 个元素，直到 ctx 结束。
//
// 内部实现，供 WaitBuilder 的 Present / Count 复用。
// 对外没有直接的 Tab 级入口——元素级等待统一走 el.Wait()，
// 避免出现「同一个能力有两条调用路径」。
func (t *Tab) waitElementCount(ctx context.Context, sel Selector, n int) error {
	// notFound=true：超时时保留 ErrElementNotFound——「始终没出现」是调用方靠 errors.Is 判断的语义。
	return pollWait(ctx, fmt.Sprintf("等待 %s 出现至少 %d 个元素", sel, n), true, func() (bool, error) {
		count, err := t.Ele(sel).Count(ctx)
		return err == nil && count >= n, err
	})
}

// ---------- 操作 ----------

// BringToFront 将该标签页激活并置前（切换到前台窗口/标签）。
// 后台窗口会被 Chrome 节流，SendKeys 等输入事件可能丢失，
// 多窗口（如隔离上下文各自开窗）场景下交互前应先调用本方法。
func (t *Tab) BringToFront(ctx context.Context) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return page.BringToFront().Do(c)
	}))
}

// WindowID 返回该标签页所属的 OS 窗口编号（Browser.getWindowForTarget）。
// 同一窗口内的多个标签页返回相同编号，可用于验证「同窗口多标签」与「独立窗口」：
// 同一隔离上下文内的标签页同窗口，不同隔离上下文各自一个窗口。
// 注意：getWindowForTarget 是 browser 级命令，需切到 browser 级连接执行。
func (t *Tab) WindowID(ctx context.Context) (int64, error) {
	var wid int64
	err := t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		id, _, e := browser.GetWindowForTarget().WithTargetID(t.ID).Do(bexec)
		if e != nil {
			return e
		}
		wid = id.Int64()
		return nil
	}))
	return wid, err
}

// ---------- 节点级操作（ClickJS / SetValue 的公共基础）----------

// nodes 按选择器取回全部匹配节点。
func (t *Tab) nodes(ctx context.Context, sel Selector) ([]*cdp.Node, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	var nodes []*cdp.Node
	err := t.run(ctx, chromedp.Nodes(sel.expr, &nodes, sel.options()...))
	return nodes, err
}

// wrapNotFound 把 chromedp 直连方法（Text / Attribute / Click / WaitVisible 等）抛出的
// 「等待到超时」包装成 ErrElementNotFound。
//
// 这些方法不像 ClickJS / SetValue 那样先走 firstNode 拿节点，而是交给 chromedp 内部
// 反复重试直到 ctx 到期。元素不存在时它们只会抛裸的 context deadline exceeded，
// 从错误里完全看不出是「选择器写错了」还是「网络慢」。这里统一翻译成哨兵错误。
// 调用方主动取消是决策，原样透传，不伪装成「没找到元素」。
func wrapNotFound(err error, sel Selector) error {
	if err == nil {
		return nil
	}
	return notFoundError(sel, err)
}

// noMatchError 是「选择器确实没匹配到东西」的纯版包装：没有底层 err 可挂。
//
// 与 notFoundError 共用同一段「%s=%q」文案——两处各写一份格式，改一处忘一处，
// 调用方就会看到「同一个错误两种写法」。标签页侧（firstNode）与框架侧
// （FrameElement.Text）都走这里。
func noMatchError(sel Selector) error {
	return fmt.Errorf("%w: %s=%q", ErrElementNotFound, sel.Mode(), sel.String())
}

// notFoundError 把「选择器没命中」的底层错误统一包装成 ErrElementNotFound（附选择器与原因）。
// 调用方主动取消是决策，原样透传，不伪装成「没找到元素」。
//
// 文案由 noMatchError 产出后再缀「（等待元素超时）」，与纯版保持同一前缀。
func notFoundError(sel Selector, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w（等待元素超时）: %w", noMatchError(sel), err)
}

// firstNode 取第一个匹配节点；没有匹配时返回 ErrElementNotFound，
// 而不是让调用方拿到空切片后下标越界。
func (t *Tab) firstNode(ctx context.Context, sel Selector) (*cdp.Node, error) {
	nodes, err := t.nodes(ctx, sel)
	if err != nil {
		// 与 wrapNotFound 共用 notFoundError：包成 ErrElementNotFound，
		// 调用方既能用 errors.Is 判断，也能从文案直接看出是哪个选择器没命中；
		// 主动取消则原样透传。
		return nil, notFoundError(sel, err)
	}
	if len(nodes) == 0 {
		// 节点列表为空不是「等待超时」，没有底层 err 可挂，走纯版包装（与框架侧同一格式）。
		return nil, noMatchError(sel)
	}
	return nodes[0], nil
}

// evalOnNode 在指定 DOM 节点上执行 JS（函数体内 this 即该节点）。
//
// 走 dom.resolveNode + runtime.callFunctionOn 而不是拼字符串 querySelector，
// 这样 XPath / JS path 这类无法用 CSS 表达的选择器也能一致地参与 JS 类操作。
func (t *Tab) evalOnNode(ctx context.Context, node *cdp.Node, fn string) (any, error) {
	var res any
	err := t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		obj, err := dom.ResolveNode().WithNodeID(node.NodeID).Do(c)
		if err != nil {
			return err
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("无法把节点解析为 JS 对象（nodeID=%d）", node.NodeID)
		}
		result, exception, err := runtime.CallFunctionOn(fn).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(c)
		if err != nil {
			return err
		}
		if exception != nil {
			return fmt.Errorf("节点上执行 JS 失败: %s", exceptionText(exception))
		}
		res = decodeRemoteValue(result)
		return nil
	}))
	return res, err
}

// exceptionText 把 CDP 异常详情转成可读字符串。
func exceptionText(e *runtime.ExceptionDetails) string {
	if e == nil {
		return ""
	}
	if e.Exception != nil && e.Exception.Description != "" {
		return e.Exception.Description
	}
	if e.Text != "" {
		return e.Text
	}
	return "未知异常"
}

// ---------- 截图 ----------

// Screenshot 截取当前页面，保存到 path
func (t *Tab) Screenshot(ctx context.Context, path string) error {
	var buf []byte
	if err := t.run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return err
	}
	return writeFile(path, buf)
}

// Reload 重新加载当前页面
func (t *Tab) Reload(ctx context.Context) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return page.Reload().Do(c)
	}))
}
