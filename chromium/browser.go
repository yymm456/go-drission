package chromium

import (
	"context"
	"fmt"
	"os/exec"
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
	launched    bool
	chromeCmd   *exec.Cmd // 启动的 Chrome 进程，Close 时用于杀进程
	closed      bool

	mu   sync.Mutex
	tabs []*Tab
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

	// 未显式指定用户数据目录时，按端口生成默认路径（需在端口确定之后）
	if o.userDataDir == "" {
		o.userDataDir = defaultUserDataDir(port)
	}

	return &Browser{
		port: port,
		opts: o,
	}
}

// Port 返回当前 Browser 使用的调试端口
func (b *Browser) Port() int {
	return b.port
}

// Connect 探测端口：活着就连，空闲就启动。
// 连接握手使用 ctx 控制；若 ctx 未设置 deadline，则退回 opts.connectTimeout 作为默认超时。
func (b *Browser) Connect(ctx context.Context) error {
	if isPortAlive(b.port) {
		fmt.Printf("检测到已有 Chrome (端口 %d)，直接连接...\n", b.port)
		b.launched = false
	} else {
		fmt.Printf("未检测到 Chrome，正在启动 (端口 %d)...\n", b.port)
		cmd, err := launchChrome(b.port, b.opts)
		if err != nil {
			return err
		}
		b.launched = true
		b.chromeCmd = cmd
	}

	b.allocCtx, b.allocCancel = chromedp.NewRemoteAllocator(
		context.Background(),
		fmt.Sprintf("http://127.0.0.1:%d", b.port),
	)

	if err := b.probeConnection(ctx); err != nil {
		if b.allocCancel != nil {
			b.allocCancel()
		}
		return err
	}
	return nil
}

// probeConnection 触发一次握手，确认 Chrome 真的可连。
// chromedp.Run 需要承载路由信息的 chromedp 上下文，因此以 allocCtx 派生的
// tmpCtx 为准，同时沿用调用方 ctx 的 deadline（若有），否则退回 opts.connectTimeout。
func (b *Browser) probeConnection(ctx context.Context) error {
	tmpCtx, tmpCancel := chromedp.NewContext(b.allocCtx)
	defer tmpCancel()

	runCtx, cancel := context.WithCancel(tmpCtx)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		runCtx, cancel = context.WithDeadline(tmpCtx, deadline)
		defer cancel()
	} else if b.opts.connectTimeout > 0 {
		runCtx, cancel = context.WithTimeout(tmpCtx, b.opts.connectTimeout)
		defer cancel()
	}

	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(context.Context) error {
		return nil
	})); err != nil {
		return fmt.Errorf("连接 Chrome (端口 %d) 失败: %w", b.port, err)
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

	tabCtx, cancel := chromedp.NewContext(b.allocCtx)
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

	// 先通过 CDP 命令真正关闭 Chrome 中的标签页
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
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
	if b.allocCancel != nil {
		b.allocCancel()
	}
	// 如果是自己启动的 Chrome，杀掉进程
	if b.launched && b.chromeCmd != nil && b.chromeCmd.Process != nil {
		_ = b.chromeCmd.Process.Kill()
		_ = b.chromeCmd.Wait()
	}
}

// OpenPage 创建并连接浏览器，返回一个可用标签页
// port 传 0 表示随机端口，传具体值表示固定端口
// 无论是新启动还是接管已有 Chrome，都优先复用最近打开的标签页，没有则新建一个
func OpenPage(ctx context.Context, port int, opts ...Option) (*Browser, *Tab, error) {
	b := NewBrowser(port, opts...)
	if err := b.Connect(ctx); err != nil {
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
		for _, t := range b.tabs {
			if t.ID != tab.ID {
				// 先通过 CDP 真正关闭 Chrome 中的标签页，再释放 context
				_ = chromedp.Run(t.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
					return target.CloseTarget(t.ID).Do(ctx)
				}))
				if t.cancel != nil {
					t.cancel()
				}
			}
		}
		b.tabs = []*Tab{tab}
		b.mu.Unlock()
	}

	return b, tab, nil
}
