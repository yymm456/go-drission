package page

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/errs"
)

// discardLogger 是 Tab 未配置 logger 时的兜底：丢弃所有日志，不污染调用方输出。
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// Tab 代表一个被托管的标签页。
// Ctx 是该标签页的 chromedp 会话根上下文，承载 target 路由信息。
// 所有 I/O 方法接受调用方传入的 ctx（应从 Ctx 派生），由调用方控制超时与取消。
//
// # 可变状态
//
// url / timeout 由 mu 保护：Browser 同步 target 列表时会在另一 goroutine 改写 url，
// 裸读写会构成数据竞争（go test -race 才能发现）。读取用 URL()，写入仅包内 setURL。
//
// antiDetected 由独立的 antiMu 保护：其判断与置位须和注入动作一起串行化，见 ensureAntiDetect。
type Tab struct {
	ID     target.ID
	Ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
	// url 是最近一次同步到的地址快照；可能滞后于真实地址，需要实时值用 CurrentURL。
	url string
	// timeout 是内置默认超时（来自 WithDefaultTimeout，默认 30s），仅当调用方 ctx 无 deadline
	// 时套用；0 表示关闭内置超时。
	timeout time.Duration

	// antiMu 串行化反检测脚本注入，保证 check-then-inject 原子。
	antiMu sync.Mutex
	// antiDetected 标记是否已注入反检测初始化脚本，保证注入幂等。
	antiDetected bool

	// logger 用于库内告警（如 iframe 绑定失败），默认静默，随 Browser 配置传入。
	logger *slog.Logger
}

// log 返回可用的 logger；调用方直接构造 Tab 时可能为 nil，兜底成静默 logger。
func (t *Tab) log() *slog.Logger {
	if t.logger != nil {
		return t.logger
	}
	return discardLogger
}

// URL 返回最近一次同步到的页面地址快照，并发安全。
//
// 该值由 Browser 同步 target 列表时写入，可能滞后（尤其导航刚发生时）。
// 需要实时地址用 CurrentURL（走一次 CDP）。
func (t *Tab) URL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.url
}

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
// 取 t.timeout 需加锁，判定交给 cdpkit.WithDefaultTimeout，避免「不覆盖调用方 deadline」
// 规则写两遍。返回的 cancel 须由调用方 defer。
func (t *Tab) applyTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	t.mu.RLock()
	timeout := t.timeout
	t.mu.RUnlock()

	return cdpkit.WithDefaultTimeout(ctx, timeout)
}

// safeCtx 兜底调用方误传的「裸 context」（不含 chromedp 路由信息）：chromedp.Run 收到裸
// context 会另起临时浏览器，命令落不到本标签页。检测到裸 context 时回退到标签页会话上下文；
// 裸 context 带 deadline 则保留其超时语义，否则套用内置超时。
//
// 返回的 cancel 须由调用方 defer；未派生新上下文时是空操作。
func (t *Tab) safeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if chromedp.FromContext(ctx) == nil {
		if t.Ctx == nil {
			return ctx, cdpkit.NoopCancel
		}
		if dl, ok := ctx.Deadline(); ok {
			// 父上下文只能是 t.Ctx：chromedp 路由信息挂在 t.Ctx 链上，换父节点命令落不到本标签页。
			c, cancel := context.WithDeadline(t.Ctx, dl)
			// 代价是调用方对 ctx 的取消不再自动传导到 c（c 的父已不是 ctx）。用 AfterFunc 补回：
			// ctx 结束时触发 cancel；run 返回前 defer cancel()，其中 stop() 先解除注册，正常路径不留常驻 goroutine。
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

// run 是本包执行 CDP 命令的唯一入口：用 safeCtx 校正上下文，命令结束即回收派生上下文。
//
// 集中在此 defer cancel，避免各方法写成 chromedp.Run(t.safeCtx(ctx), ...)（拿不到 cancel，
// 超时定时器要挂到 deadline 才释放，go vet 的 lostcancel 也会报）。
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

// waitFatalErr 判定轮询等待途中的错误能否直接中止，不必等到超时。
//
// 判据只有一条：再等下去结论也不会变。目前满足的只有本库自产错误——入参不合法
// （空选择器）与连接 / 上下文已失效，重试无意义。
//
// CDP 回来的错误一律视为可重试：导航期间 CDP 必抛 "execution context destroyed" 等，
// 而 WaitURL / Present / Count 常常正跨着一次导航在等。靠匹配错误文本挑「永久性 CDP
// 错误」既脆弱又会把正常等待变成偶发失败，故只记录不中止，超时时一并返回。
//
// ErrElementNotFound 刻意不在名单里：元素未出现正是等待的目标。四条分支都有 TestWaitFatalErr 覆盖。
func waitFatalErr(err error) bool {
	return errors.Is(err, errs.ErrSelectorRequired) ||
		errors.Is(err, errs.ErrClosed) ||
		errors.Is(err, errs.ErrNotConnected) ||
		errors.Is(err, errs.ErrContextClosed)
}

// waitRecordErr 记下轮询期间最后一次「非 ctx 终止」的错误。
//
// ctx 自身的取消 / 超时不计：那由 select 的 ctx.Done() 分支处理，记下会让同一原因在最终错误里出现两遍。
func waitRecordErr(last, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return last
	}
	return err
}

// waitTimeoutErr 构造「等待超时」错误，并带上期间最后一次底层错误。
//
// 不带底层错误的话调用方只看到「等待超时」，背后的 invalid context / CSS 语法错误全被吞掉。
// 两个 %w（Go 1.20+）让 ctx 错误与底层错误都留在错误链里，errors.Is(err, DeadlineExceeded) 照旧成立。
func waitTimeoutErr(what string, ctxErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("%s 失败: %w", what, ctxErr)
	}
	return fmt.Errorf("%s 失败: %w（期间最后一次错误: %w）", what, ctxErr, lastErr)
}

// pollWait 是三个轮询式等待（WaitURL / waitElementCount / Element.WaitText）共用的循环骨架。
//
// 它们只差「判什么条件」与「错误文案」，抽出一份可避免把错误分诊规则复制三遍（该规则最易改错，见 waitFatalErr）。
//
// 语义：
//   - probe 返回 (true, _) 即成功；err 仅在未命中时有意义；
//   - probe 错误命中 waitFatalErr → 立即中止，文案 "<what> 失败: <err>"；
//   - ctx 结束 → 用 waitTimeoutErr 拼「超时 + 期间最后一次错误」；notFound 为 true 时再套 ErrElementNotFound。
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
				return fmt.Errorf("%w: %w", errs.ErrElementNotFound, timeoutErr)
			}
			return timeoutErr
		case <-ticker.C:
		}
	}
}

// WaitURL 轮询等待当前 URL 包含指定子串，直到 ctx 结束
func (t *Tab) WaitURL(ctx context.Context, substr string) error {
	// 循环骨架与错误分诊规则见 pollWait。
	return pollWait(ctx, fmt.Sprintf("等待 URL 包含 %q", substr), false, func() (bool, error) {
		u, err := t.CurrentURL(ctx)
		return err == nil && strings.Contains(u, substr), err
	})
}

// waitElementCount 轮询等待选择器匹配到至少 n 个元素，直到 ctx 结束。
//
// 内部实现，供 WaitBuilder 的 Present / Count 复用；对外无 Tab 级入口，元素级等待统一走 el.Wait()。
func (t *Tab) waitElementCount(ctx context.Context, sel Selector, n int) error {
	// notFound=true：超时时保留 ErrElementNotFound（「始终没出现」是调用方靠 errors.Is 判断的语义）。
	return pollWait(ctx, fmt.Sprintf("等待 %s 出现至少 %d 个元素", sel, n), true, func() (bool, error) {
		count, err := t.Ele(sel).Count(ctx)
		return err == nil && count >= n, err
	})
}

// ---------- 操作 ----------

// BringToFront 将该标签页激活并置前。
// 后台窗口会被 Chrome 节流，SendKeys 等输入事件可能丢失，多窗口场景下交互前应先调用本方法。
func (t *Tab) BringToFront(ctx context.Context) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return cdppage.BringToFront().Do(c)
	}))
}

// WindowID 返回该标签页所属的 OS 窗口编号（Browser.getWindowForTarget）。
// 同一窗口内的多个标签页返回相同编号，可用于区分「同窗口多标签」与「独立窗口」。
// getWindowForTarget 是 browser 级命令，需切到 browser 级连接执行。
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

// Activate 激活（聚焦）该标签页所在的 target。
//
// 与 BringToFront 的区别：BringToFront 走 Page.bringToFront，CDP 原文是
// "brings page to front (activates tab)" —— 在该标签页所属窗口内置前；
// Activate 走 Target.activateTarget，原文是 "activates (focuses) the target"，
// 多一层聚焦，更接近真实用户点了一下这个标签页。
//
// 两者都作用于「标签页所在窗口内部」：窗口若被最小化或被其它应用遮挡，
// 需要先用 SetWindowState(ctx, "normal") 把窗口恢复出来。
// activateTarget 是 browser 级命令，需切到 browser 级连接执行（同 WindowID）。
func (t *Tab) Activate(ctx context.Context) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		return target.ActivateTarget(t.ID).Do(bexec)
	}))
}

// SetWindowState 设置该标签页所属 OS 窗口的状态（Browser.setWindowBounds）。
//
// state 只能是 "normal" / "minimized" / "maximized" / "fullscreen" 之一，
// 其它值在发起 CDP 调用前就被拦成 ErrInvalidWindowState —— 让 Chrome 去报错会得到
// 一句看不出哪里错的 "Invalid window bounds"，和 Cookie 校验同一个道理。
//
// 常见用法：窗口被最小化时先 SetWindowState(ctx, "normal") 把它恢复出来，
// 再 BringToFront / Activate 切换标签页。
// 注意它只改窗口状态，**不保证把窗口顶到其它应用之上**（那需要 OS 级能力，CDP 没有）。
func (t *Tab) SetWindowState(ctx context.Context, state string) error {
	switch state {
	case "normal", "minimized", "maximized", "fullscreen":
	default:
		return fmt.Errorf("%w: 窗口状态 %q 非法（只能是 normal / minimized / maximized / fullscreen）",
			errs.ErrInvalidWindowState, state)
	}

	wid, err := t.WindowID(ctx)
	if err != nil {
		return err
	}
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		bexec := cdp.WithExecutor(c, chromedp.FromContext(c).Browser)
		return browser.SetWindowBounds(browser.WindowID(wid), &browser.Bounds{
			WindowState: browser.WindowState(state),
		}).Do(bexec)
	}))
}

// ---------- 节点级操作（ClickJS / SetValue 的公共基础）----------

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
// 这些方法交给 chromedp 内部重试直到 ctx 到期，元素不存在时只抛裸的 context deadline exceeded，
// 看不出是选择器写错还是网络慢，故统一翻译成哨兵错误。调用方主动取消原样透传。
func wrapNotFound(err error, sel Selector) error {
	if err == nil {
		return nil
	}
	return notFoundError(sel, err)
}

// noMatchError 是「选择器没匹配到」的纯版包装（无底层 err 可挂）。
//
// 与 notFoundError 共用「%s=%q」文案，避免两处各写一份格式。标签页侧（firstNode）与
// 框架侧（FrameElement.Text）都走这里。
func noMatchError(sel Selector) error {
	return fmt.Errorf("%w: %s=%q", errs.ErrElementNotFound, sel.Mode(), sel.String())
}

// notFoundError 把「选择器没命中」的底层错误统一包装成 ErrElementNotFound（附选择器与原因）。
// 调用方主动取消原样透传。文案由 noMatchError 产出后再缀「（等待元素超时）」，与纯版保持同一前缀。
func notFoundError(sel Selector, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w（等待元素超时）: %w", noMatchError(sel), err)
}

// firstNode 取第一个匹配节点；无匹配时返回 ErrElementNotFound，避免调用方拿到空切片后下标越界。
func (t *Tab) firstNode(ctx context.Context, sel Selector) (*cdp.Node, error) {
	nodes, err := t.nodes(ctx, sel)
	if err != nil {
		return nil, notFoundError(sel, err)
	}
	if len(nodes) == 0 {
		// 节点列表为空不是「等待超时」，无底层 err 可挂，走纯版包装（与框架侧同一格式）。
		return nil, noMatchError(sel)
	}
	return nodes[0], nil
}

// evalOnNode 在指定 DOM 节点上执行 JS（函数体内 this 即该节点）。
//
// 走 dom.resolveNode + runtime.callFunctionOn 而非拼字符串 querySelector，
// 使 XPath / JS path 这类无法用 CSS 表达的选择器也能一致参与 JS 类操作。
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
			return fmt.Errorf("节点上执行 JS 失败: %s", cdpkit.ExceptionText(exception))
		}
		res = cdpkit.DecodeRemoteValue(result)
		return nil
	}))
	return res, err
}

// ---------- 截图 ----------

// Screenshot 截取当前页面，保存到 path
func (t *Tab) Screenshot(ctx context.Context, path string) error {
	var buf []byte
	if err := t.run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// Reload 重新加载当前页面
func (t *Tab) Reload(ctx context.Context) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return cdppage.Reload().Do(c)
	}))
}

// Back 后退到上一条历史记录，等待导航完成。
//
// 超时与取消语义与 Navigate 完全一致：走 t.run → safeCtx 校正，
// 调用方 ctx 的 deadline 优先，未设时才套用 Tab 内置超时。不另起一套 timeout 机制。
//
// 已经在第一条历史记录时返回 ErrNoHistoryEntry（可用 errors.Is 判断），
// 页面保持原样 —— 不做静默 no-op，否则调用方分不清「退了」和「没得退」。
func (t *Tab) Back(ctx context.Context) error {
	return t.navigateHistory(ctx, -1)
}

// Forward 前进到下一条历史记录，等待导航完成。语义与 Back 完全对称。
func (t *Tab) Forward(ctx context.Context) error {
	return t.navigateHistory(ctx, 1)
}

// navigateHistory 按 offset 移动历史指针并等待导航完成；offset 为 -1 后退、+1 前进。
//
// 先自己取一次历史，有两个原因：
//  1. 越界时要给**本库**的哨兵错误 —— 上游 chromedp.NavigateBack 给的是
//     errors.New("invalid navigation entry")，无法用 errors.Is 判断；
//  2. NavigateToHistoryEntry 需要目标条目的 ID，只能从历史里取。
//
// 跳转这一步复用上游的 chromedp.NavigateToHistoryEntry：它是 NavigateAction，
// 自带 load 等待，正好与 Navigate 的语义对齐（不必自己实现等待）。
func (t *Tab) navigateHistory(ctx context.Context, offset int64) error {
	return t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		cur, entries, err := cdppage.GetNavigationHistory().Do(c)
		if err != nil {
			return err
		}
		target := cur + offset
		if len(entries) == 0 || target < 0 || target >= int64(len(entries)) {
			return fmt.Errorf("%w（当前第 %d 条，共 %d 条）", errs.ErrNoHistoryEntry, cur, len(entries))
		}

		// 用裸 CDP 命令，而不是上游的 chromedp.NavigateToHistoryEntry。
		// 后者是 NavigateAction，会等一次 load 事件；但**历史导航通常不会再触发 load**
		// （页面从缓存恢复），实测结果是导航早已成功、却一路等到 ctx 超时：
		//     裸命令      771µs   URL 与标题都正确
		//     等 load 封装 8.0s    deadline exceeded（页面其实已经切好了）
		// 所以这里自己发命令，再自己判定「导航完成」。
		if err := cdppage.NavigateToHistoryEntry(entries[target].ID).Do(c); err != nil {
			return err
		}

		// 「导航完成」的判据取「地址变成目标条目」，而不是等 load 或等 body：
		// 历史导航不会再触发一次 load，等它会一路卡到 ctx 超时（实测导航其实早就成功了）。
		// 复用 pollWait 骨架，与 WaitURL 共用同一套错误分诊规则。
		want := entries[target].URL
		return pollWait(c, fmt.Sprintf("等待地址变为 %q", want), false, func() (bool, error) {
			var u string
			if err := chromedp.Location(&u).Do(c); err != nil {
				return false, err
			}
			return u == want || strings.Contains(u, want), nil
		})
	}))
}

// Close 关闭该标签页。
//
// 不必先拿到 Browser：关掉 Chrome 里的 target 之后，Browser 台账里的那条登记会在
// 下一次 Tabs() / LatestTab() / GetTab() 时被自动清掉 —— syncTabs 发现某个 target
// 已不在实时快照里，就会调 ReleaseTab 并把它从 b.tabs 摘掉（见 browser/targets.go）。
// 这里不等那次同步，主动 ReleaseTab 一次，免得 chromedp 会话白挂一段时间。
//
// 与 Browser.Close / BrowserContext.Close 一致，不收 ctx 也不返回错误：关闭是收尾动作，
// 内部自带超时保护（与 Browser.CloseTab 同一个 cdpkit.DefaultCallTimeout），
// Chrome 无响应也不该把调用方挂住。重复调用安全。
//
// 注意：关闭窗口里的第一个标签页可能连同窗口一起关掉，这与 BrowserContext.CloseTab
// 记录的行为一致；标签页关掉后它上面的操作会因 ctx 已取消而报错。
func (t *Tab) Close() {
	if t.Ctx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.Ctx, cdpkit.DefaultCallTimeout)
	defer cancel()

	// 顺序不能颠倒：先发命令，再释放会话。ReleaseTab 会 cancel t.Ctx，
	// 反过来做的话命令拿到的是已取消的上下文，压根发不出去。
	_ = t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		return target.CloseTarget(t.ID).Do(c)
	}))
	ReleaseTab(t)
}
