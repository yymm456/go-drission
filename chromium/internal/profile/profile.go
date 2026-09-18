package profile

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/yymm456/go-drission/chromium/internal/browser"
	"github.com/yymm456/go-drission/chromium/internal/config"
	"github.com/yymm456/go-drission/chromium/internal/errs"
	"github.com/yymm456/go-drission/chromium/internal/page"
)

// Profile 代表一个命名的浏览器档案：独立的用户数据目录 + 独立端口，
// 因此各 Profile 之间的 Cookie / 登录态 / 代理 / UA 完全隔离，且登录态持久化到磁盘，
// 进程重启后用同名 Profile 打开即可恢复登录，适合多账户场景。
type Profile struct {
	Name    string // 档案名（调用方传入的原始名字）
	Dir     string // 该档案的用户数据目录
	Port    int    // 该档案使用的调试端口
	browser *browser.Browser
}

// Browser 返回该档案已打开的 Browser（可能为 nil，表示尚未 Open）。
func (p *Profile) Browser() *browser.Browser { return p.browser }

// ProfileManager 管理多个命名 Profile，实现多账户隔离。
//
// 每个 Profile 按名字映射到固定的用户数据目录（baseDir/<name>），端口从 basePort
// 起按打开顺序递增分配，互不冲突。同名 Profile 复用同一个 Browser；首次打开时才真正
// 连接/启动 Chrome（懒加载）。
//
// 并发模型：全局锁（mu）只用于保护注册表与端口分配这类瞬时操作；真正耗时的
// OpenPage（可能要冷启动 Chrome，十几秒）放在 per-name 锁内执行。
// 结果是「同名串行、异名并行」——多个档案可以同时打开，不会互相阻塞。
//
// 典型用法：
//
//	pm := chromium.NewProfileManager("./profiles", 9300,
//	    chromium.WithHeadless(true),
//	    chromium.WithLogger(logger),
//	)
//	defer pm.CloseAll()
//
//	_, tabA, err := pm.Open(ctx, "account_001") // 账户 A 的独立浏览器
//	_, tabB, err := pm.Open(ctx, "account_002") // 账户 B，与 A 完全隔离
type ProfileManager struct {
	baseDir  string
	basePort int
	opts     []config.Option

	mu       sync.Mutex
	profiles map[string]*Profile
	// inflight 为每个档案名维护一把锁，用于把耗时的 OpenPage 串行化到同名档案上；
	// 不同名字各用各的锁，因此互不影响。
	//
	// 这个 map 只增不删：Close 若在此处 delete，正在持锁 OpenPage 的 goroutine 仍用旧锁，
	// 而紧随其后的 Open 会新建一把新锁 —— 两者同时启动同一个 user-data-dir 的 Chrome，
	// 正是 per-name 锁要避免的事。条目数等于历史档案名数量，量级可忽略，
	// 用「少量内存」换「不可能出现双份锁」是划算的。
	inflight map[string]*sync.Mutex
	// free 回收打开失败时占用的端口号，优先复用，避免反复失败把端口段白白耗尽
	free   []int
	next   int // 端口分配计数，保证同一 manager 内端口唯一
	closed bool
}

// NewProfileManager 创建一个 Profile 管理器。
//   - baseDir：所有 Profile 用户数据目录的根目录，每个 Profile 落在 baseDir/<name>。
//   - basePort：起始调试端口，按打开顺序递增分配（basePort、basePort+1 ...）。
//     传 0 则使用默认起始端口 9300。
//   - opts：应用于每个 Profile 的公共配置项（如 WithHeadless / WithProxy / WithLogger）。
//     注意：不要在此传 WithUserDataDir，它会被 Profile 各自的目录覆盖。
func NewProfileManager(baseDir string, basePort int, opts ...config.Option) *ProfileManager {
	if basePort <= 0 {
		basePort = 9300
	}
	if baseDir == "" {
		baseDir = "profiles"
	}
	// 转为绝对路径：相对路径会让 Chrome handoff 到既有会话，导致新实例端口无法就绪。
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}
	return &ProfileManager{
		baseDir:  baseDir,
		basePort: basePort,
		opts:     opts,
		profiles: map[string]*Profile{},
		inflight: map[string]*sync.Mutex{},
	}
}

// lockFor 返回该档案名对应的 per-name 锁（需在 mu 保护下调用）。
func (pm *ProfileManager) lockForLocked(name string) *sync.Mutex {
	if l, ok := pm.inflight[name]; ok {
		return l
	}
	l := &sync.Mutex{}
	pm.inflight[name] = l
	return l
}

// takePortLocked 取一个未占用的端口：优先复用回收池，否则从 basePort 递增（需在 mu 下调用）。
func (pm *ProfileManager) takePortLocked() int {
	if n := len(pm.free); n > 0 {
		port := pm.free[n-1]
		pm.free = pm.free[:n-1]
		return port
	}
	port := pm.basePort + pm.next
	pm.next++
	return port
}

// releasePort 归还端口，供后续 Open 复用。重复归还同一端口会被忽略，避免池中出现重复项。
func (pm *ProfileManager) releasePort(port int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if slices.Contains(pm.free, port) {
		return
	}
	pm.free = append(pm.free, port)
}

// Open 打开（或复用）指定名字的 Profile，返回其 Browser 与一个可用 Tab。
// 同名 Profile 已打开时直接复用；否则以 baseDir/<name> 为用户数据目录、
// 分配一个独立端口，连接已有 Chrome 或启动新的。
func (pm *ProfileManager) Open(ctx context.Context, name string) (*browser.Browser, *page.Tab, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, fmt.Errorf("ProfileManager: profile 名字不能为空")
	}

	// 阶段一：在全局锁下只做瞬时操作——状态检查、分配端口、取 per-name 锁。
	pm.mu.Lock()
	if pm.closed {
		pm.mu.Unlock()
		return nil, nil, fmt.Errorf("%w，无法再打开 Profile", errs.ErrProfileClosed)
	}
	if p, ok := pm.profiles[name]; ok && p.browser != nil {
		browser := p.browser
		pm.mu.Unlock()
		return pm.reuseTab(ctx, browser)
	}

	dir := filepath.Join(pm.baseDir, sanitizeProfileName(name))
	port := pm.takePortLocked()
	lock := pm.lockForLocked(name)
	pm.mu.Unlock()

	// 阶段二：只在 per-name 锁内执行真正耗时的 OpenPage。
	// 此时全局锁已释放，其他档案可以并行打开；
	// 同名档案会在此串行，避免两个 goroutine 同时对同一 user-data-dir 启动 Chrome。
	lock.Lock()
	defer lock.Unlock()

	// 双检：等待 per-name 锁期间，同名档案可能已被另一个 goroutine 打开
	pm.mu.Lock()
	if pm.closed {
		pm.mu.Unlock()
		pm.releasePort(port)
		return nil, nil, fmt.Errorf("%w，无法再打开 Profile", errs.ErrProfileClosed)
	}
	if p, ok := pm.profiles[name]; ok && p.browser != nil {
		browser := p.browser
		pm.mu.Unlock()
		pm.releasePort(port)
		return pm.reuseTab(ctx, browser)
	}
	pm.mu.Unlock()

	// 复制公共配置，再强制覆盖为该 Profile 独立的用户数据目录
	opts := make([]config.Option, 0, len(pm.opts)+1)
	opts = append(opts, pm.opts...)
	opts = append(opts, config.WithUserDataDir(dir))

	browser, tab, err := browser.OpenPage(ctx, port, opts...)
	if err != nil {
		pm.releasePort(port)
		return nil, nil, fmt.Errorf("打开 Profile %q 失败: %w", name, err)
	}

	pm.mu.Lock()
	pm.profiles[name] = &Profile{Name: name, Dir: dir, Port: port, browser: browser}
	pm.mu.Unlock()
	return browser, tab, nil
}

// reuseTab 复用已打开的浏览器：优先取最新标签页，没有就新建。
// 不持全局锁——LatestTab / NewTab 内部会走 CDP 调用。
func (pm *ProfileManager) reuseTab(ctx context.Context, browser *browser.Browser) (*browser.Browser, *page.Tab, error) {
	tab, err := browser.LatestTab(ctx)
	if err != nil {
		tab, err = browser.NewTab(ctx)
	}
	if err != nil {
		return nil, nil, err
	}
	return browser, tab, nil
}

// Get 返回已打开的同名 Profile；未打开则返回 error（不会自动打开）。
func (pm *ProfileManager) Get(name string) (*Profile, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	p, ok := pm.profiles[name]
	if !ok || p.browser == nil {
		return nil, fmt.Errorf("Profile %q 尚未打开", name)
	}
	return p, nil
}

// Names 返回当前已打开的所有 Profile 名字。
func (pm *ProfileManager) Names() []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	names := make([]string, 0, len(pm.profiles))
	for name := range pm.profiles {
		names = append(names, name)
	}
	return names
}

// Close 关闭指定名字的 Profile 浏览器（若是自启的 Chrome 则结束进程），
// 磁盘上的用户数据目录会保留，下次同名 Open 仍可恢复登录态。
//
// 注意不清理 pm.inflight：理由见 inflight 字段的说明。
func (pm *ProfileManager) Close(name string) {
	pm.mu.Lock()
	p, ok := pm.profiles[name]
	if ok {
		delete(pm.profiles, name)
	}
	pm.mu.Unlock()

	// 在锁外关闭：Browser.Close 会走 CDP 与进程回收，耗时不短
	if ok && p.browser != nil {
		p.browser.Close()
	}
}

// CloseAll 关闭所有已打开的 Profile 浏览器。磁盘用户数据目录全部保留。
func (pm *ProfileManager) CloseAll() {
	pm.mu.Lock()
	pm.closed = true
	opened := make([]*Profile, 0, len(pm.profiles))
	for name, p := range pm.profiles {
		opened = append(opened, p)
		delete(pm.profiles, name)
	}
	pm.mu.Unlock()

	for _, p := range opened {
		if p.browser != nil {
			p.browser.Close()
		}
	}
}

// sanitizeProfileName 把 Profile 名字转换为安全的目录名：
// 仅保留字母、数字、'-'、'_'，其余字符替换为 '_'，避免路径穿越与非法字符。
func sanitizeProfileName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "default"
	}
	return s
}
