package chromium

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Browser 持有整个浏览器连接和所有标签页
type Browser struct {
	port int
	opts *options

	allocCtx    context.Context
	allocCancel context.CancelFunc
	// rootCtx 是常驻的浏览器上下文：所有标签页上下文都作为它的子上下文派生，
	// 以共享同一个 browser 连接（chromedp 要求如此，否则新建标签页会失败）。
	// rootTargetID 是 rootCtx 自身占用的锚点 target，不纳入标签页管理。
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	rootTargetID target.ID
	launched     bool
	chromeCmd    *exec.Cmd    // 启动的 Chrome 进程，Close 时用于杀进程
	lock         *profileLock // 数据目录排他锁，仅自己启动 Chrome 时持有
	closed       bool

	mu   sync.Mutex
	tabs []*Tab

	// ctxMu 保护 contexts（命名隔离上下文注册表），与 mu 分开以降低锁竞争
	ctxMu    sync.Mutex
	contexts map[string]*BrowserContext
}

// NewBrowser 创建 Browser
// port 传 0 表示随机端口，传具体值表示固定端口
func NewBrowser(port int, opts ...Option) *Browser {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}

	if port == 0 {
		if free, err := findFreePort(); err == nil {
			port = free
		} else {
			port = 9222 // 分配失败时退回默认端口
		}
	}

	// 用户数据目录：未显式指定时，按端口生成默认路径（需在端口确定之后）。
	// 必须转为绝对路径：相对路径会让 Chrome handoff 到已存在的浏览器会话，
	// 从而不新开实例，请求的调试端口也就永远无法就绪。
	if o.userDataDir == "" {
		o.userDataDir = defaultUserDataDir(port)
	} else if abs, err := filepath.Abs(o.userDataDir); err == nil {
		o.userDataDir = abs
	}

	return &Browser{
		port:     port,
		opts:     o,
		contexts: map[string]*BrowserContext{},
	}
}

// removeContext 从命名上下文注册表中移除指定名字的隔离上下文。
// 仅持有 ctxMu，不触碰 mu，避免与 Browser.Close 产生锁顺序环。
func (b *Browser) removeContext(name string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	delete(b.contexts, name)
}

// Port 返回当前 Browser 使用的调试端口
func (b *Browser) Port() int {
	return b.port
}

// PID 返回本实例启动的 Chrome 主进程 PID；接管已有 Chrome 时返回 0。
// 可与 Tab.WindowID 配合证明「多个窗口同属一个浏览器进程」。
func (b *Browser) PID() int {
	if b.chromeCmd == nil || b.chromeCmd.Process == nil {
		return 0
	}
	return b.chromeCmd.Process.Pid
}

// Connect 探测端口：活着就连，空闲就启动。
// 连接握手使用 ctx 控制；若 ctx 未设置 deadline，则退回 opts.connectTimeout 作为默认超时。
func (b *Browser) Connect(ctx context.Context) error {
	if isPortAlive(b.port) {
		b.opts.logger.Info("检测到已有 Chrome，直接连接", "port", b.port)
		b.launched = false
	} else {
		b.opts.logger.Info("未检测到 Chrome，正在启动", "port", b.port)
		// 先拿数据目录的 OS 级排他锁：两个实例并发用同一目录启动 Chrome 时，
		// 后启动者会弹「无法对其数据目录执行读写操作」对话框，这里提前转为明确的 Go 错误。
		lock, err := acquireProfileLock(b.opts.userDataDir)
		if err != nil {
			return err
		}
		cmd, err := launchChrome(b.port, b.opts)
		if err != nil {
			lock.release()
			return err
		}
		b.lock = lock
		b.launched = true
		b.chromeCmd = cmd
	}

	b.allocCtx, b.allocCancel = chromedp.NewRemoteAllocator(
		context.Background(),
		fmt.Sprintf("http://127.0.0.1:%d", b.port),
	)

	if err := b.initRootContext(); err != nil {
		if b.allocCancel != nil {
			b.allocCancel()
		}
		return err
	}
	b.opts.logger.Info("已连接 Chrome", "port", b.port, "launched", b.launched)
	return nil
}

// initRootContext 建立常驻的浏览器上下文 rootCtx，并借此确认 Chrome 真的可连。
//
// chromedp 中，只有从「已分配 browser 连接的上下文」派生的子上下文，才会共享该连接并通过
// Target.createTarget 新建标签页；若直接从 allocator(allocCtx) 派生，每个上下文都会各自新建
// 一条 browser 连接，在已有连接存在时 createTarget 可能失败（no browser is open）。
// 因此这里先建立并 Run 一个 rootCtx 作为所有标签页上下文的父级。rootCtx 自身会占用一个空白
// target 作为锚点（rootTargetID），不纳入标签页管理，Browser.Close 时随 rootCancel 一并释放。
//
// 首次 Run 直接作用于 rootCtx（不包超时子 ctx），以免连接被提前取消。
func (b *Browser) initRootContext() error {
	b.rootCtx, b.rootCancel = chromedp.NewContext(b.allocCtx)

	// 首次 Run 必须直接作用于 b.rootCtx，不能包一层「可取消的超时子 ctx」：
	// RemoteAllocator 会把 browser 连接的生命周期绑定到首次 Run 传入的 ctx，
	// 一旦该 ctx 被取消，连接随之关闭，rootCtx 立即失效（后续操作报 context canceled）。
	// 端口存活已由 isPortAlive / launchChrome 保证，这里直接建立长连接即可。
	if err := chromedp.Run(b.rootCtx, chromedp.ActionFunc(func(context.Context) error {
		return nil
	})); err != nil {
		return fmt.Errorf("连接 Chrome (端口 %d) 失败: %w", b.port, err)
	}

	// 记录 rootCtx 自身占用的锚点 target，后续同步标签页时跳过它
	var info *target.Info
	if err := chromedp.Run(b.rootCtx, chromedp.ActionFunc(func(c context.Context) error {
		var e error
		info, e = target.GetTargetInfo().Do(c)
		return e
	})); err == nil && info != nil {
		b.rootTargetID = info.TargetID
	}
	return nil
}

// Tabs 返回当前浏览器里所有 page 类型的标签页
func (b *Browser) Tabs(ctx context.Context) ([]*Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}
	return b.syncTabsLocked(ctx)
}

// NewTab 新建一个标签页并托管
func (b *Browser) NewTab(ctx context.Context) (*Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}

	before, err := b.listTargets(ctx)
	if err != nil {
		return nil, err
	}

	// 从常驻 rootCtx 派生子上下文：子上下文共享 browser 连接，Run 时通过 createTarget 新建标签页
	tabCtx, cancel := chromedp.NewContext(b.rootCtx)
	if err := chromedp.Run(tabCtx); err != nil {
		cancel()
		return nil, err
	}

	after, err := b.listTargets(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	newID := findNewTargetID(before, after)
	if newID == "" {
		cancel()
		return nil, fmt.Errorf("新建标签页成功，但未能定位它的 target ID")
	}

	tab := &Tab{ID: newID, Ctx: tabCtx, cancel: cancel, URL: "about:blank"}
	b.tabs = append(b.tabs, tab)
	return tab, nil
}

// GetTab 返回指定索引的标签页
func (b *Browser) GetTab(ctx context.Context, index int) (*Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}

	tabs, err := b.syncTabsLocked(ctx)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(tabs) {
		return nil, fmt.Errorf("标签页索引 %d 越界（共 %d 个）", index, len(tabs))
	}
	return tabs[index], nil
}

// GetTabByURL 按 URL 包含关系查找标签页
func (b *Browser) GetTabByURL(ctx context.Context, substr string) (*Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}

	tabs, err := b.syncTabsLocked(ctx)
	if err != nil {
		return nil, err
	}
	for _, tab := range tabs {
		if contains(tab.URL, substr) {
			return tab, nil
		}
	}
	return nil, fmt.Errorf("未找到 URL 包含 %q 的标签页", substr)
}

// LatestTab 返回最后打开的标签页
func (b *Browser) LatestTab(ctx context.Context) (*Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, fmt.Errorf("浏览器连接已关闭")
	}

	tabs, err := b.syncTabsLocked(ctx)
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, fmt.Errorf("当前没有标签页")
	}
	return tabs[len(tabs)-1], nil
}

// CloseTab 显式关闭某个标签页
func (b *Browser) CloseTab(ctx context.Context, tab *Tab) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 调用方传裸 context 时回退到 rootCtx，避免 chromedp.Run 另起临时浏览器
	runCtx := ctx
	if chromedp.FromContext(ctx) == nil {
		runCtx = b.rootCtx
	}

	// 先通过 CDP 命令真正关闭 Chrome 中的标签页
	_ = chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return target.CloseTarget(tab.ID).Do(ctx)
	}))
	if tab.cancel != nil {
		tab.cancel()
	}
	for i, t := range b.tabs {
		if t.ID == tab.ID {
			b.tabs = append(b.tabs[:i], b.tabs[i+1:]...)
			break
		}
	}
}

// Close 断开连接并释放资源。默认不关标签页。
// 属于 teardown 操作，遵循 io.Closer 惯例不接受 ctx。
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true
	// 先释放常驻 rootCtx（会关闭其锚点空白标签页），再释放 allocator
	if b.rootCancel != nil {
		b.rootCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	// 如果是自己启动的 Chrome，杀掉整棵进程树（含 renderer/gpu/crashpad 等子进程），
	// 否则残留子进程会继续占用 user-data-dir 文件锁，导致下次启动报「无法读写数据目录」。
	if b.launched {
		killProcessTree(b.chromeCmd)
	}
	// 进程树杀完后再释放数据目录排他锁，让等待方能立刻接管
	if b.lock != nil {
		b.lock.release()
		b.lock = nil
	}
}

// OpenPage 创建并连接浏览器，返回一个可用标签页
// port 传 0 表示随机端口，传具体值表示固定端口
// 无论是新启动还是接管已有 Chrome，都优先复用最近打开的标签页，没有则新建一个
func OpenPage(ctx context.Context, port int, opts ...Option) (*Browser, *Tab, error) {
	b := NewBrowser(port, opts...)
	if err := b.Connect(ctx); err != nil {
		b.Close() // 连接失败也要清理：若已启动 Chrome 则结束进程，避免汄漏僵尸实例
		return nil, nil, err
	}

	// 优先获取最新（最后打开）的标签页；若当前没有任何标签页
	// （启动瞬间 target 未就绪、或已有浏览器标签全被关掉），则新建一个。
	tab, err := b.LatestTab(ctx)
	if err != nil {
		tab, err = b.NewTab(ctx)
		if err != nil {
			b.Close()
			return nil, nil, err
		}
	}

	// 仅对新启动的 Chrome 清理多余标签页（如初始空白页）；
	// 接管已有 Chrome 时绝不能关闭用户自己开的其他标签页。
	if b.launched {
		b.mu.Lock()
		keep := tab.ID
		var others []*Tab
		for _, t := range b.tabs {
			if t.ID != keep {
				others = append(others, t)
			}
		}
		b.tabs = []*Tab{tab}
		b.mu.Unlock()

		// 在锁外关闭其他标签，避免长时间持锁
		for _, t := range others {
			_ = chromedp.Run(t.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				return target.CloseTarget(t.ID).Do(ctx)
			}))
			if t.cancel != nil {
				t.cancel()
			}
		}
	}

	return b, tab, nil
}
