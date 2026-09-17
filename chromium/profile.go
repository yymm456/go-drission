package chromium

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Profile 代表一个命名的浏览器档案：独立的用户数据目录 + 独立端口，
// 因此各 Profile 之间的 Cookie / 登录态 / 代理 / UA 完全隔离，且登录态持久化到磁盘，
// 进程重启后用同名 Profile 打开即可恢复登录，适合多账户场景。
type Profile struct {
	Name    string // 档案名（调用方传入的原始名字）
	Dir     string // 该档案的用户数据目录
	Port    int    // 该档案使用的调试端口
	browser *Browser
}

// Browser 返回该档案已打开的 Browser（可能为 nil，表示尚未 Open）。
func (p *Profile) Browser() *Browser { return p.browser }

// ProfileManager 管理多个命名 Profile，实现多账户隔离。
//
// 每个 Profile 按名字映射到固定的用户数据目录（baseDir/<name>），端口从 basePort
// 起按打开顺序递增分配，互不冲突。同名 Profile 复用同一个 Browser；首次打开时才真正
// 连接/启动 Chrome（懒加载）。
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
	opts     []Option

	mu       sync.Mutex
	profiles map[string]*Profile
	next     int // 端口分配计数，保证同一 manager 内端口唯一
	closed   bool
}

// NewProfileManager 创建一个 Profile 管理器。
//   - baseDir：所有 Profile 用户数据目录的根目录，每个 Profile 落在 baseDir/<name>。
//   - basePort：起始调试端口，按打开顺序递增分配（basePort、basePort+1 ...）。
//     传 0 则使用默认起始端口 9300。
//   - opts：应用于每个 Profile 的公共配置项（如 WithHeadless / WithProxy / WithLogger）。
//     注意：不要在此传 WithUserDataDir，它会被 Profile 各自的目录覆盖。
func NewProfileManager(baseDir string, basePort int, opts ...Option) *ProfileManager {
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
	}
}

// Open 打开（或复用）指定名字的 Profile，返回其 Browser 与一个可用 Tab。
// 同名 Profile 已打开时直接复用；否则以 baseDir/<name> 为用户数据目录、
// 分配一个独立端口，连接已有 Chrome 或启动新的。
func (pm *ProfileManager) Open(ctx context.Context, name string) (*Browser, *Tab, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, fmt.Errorf("ProfileManager: profile 名字不能为空")
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.closed {
		return nil, nil, fmt.Errorf("ProfileManager: 已关闭，无法再打开 Profile")
	}

	// 复用已打开的同名 Profile
	if p, ok := pm.profiles[name]; ok && p.browser != nil {
		tab, err := p.browser.LatestTab(ctx)
		if err != nil {
			tab, err = p.browser.NewTab(ctx)
		}
		if err != nil {
			return nil, nil, err
		}
		return p.browser, tab, nil
	}

	dir := filepath.Join(pm.baseDir, sanitizeProfileName(name))
	port := pm.basePort + pm.next
	pm.next++

	// 复制公共配置，再强制覆盖为该 Profile 独立的用户数据目录
	opts := make([]Option, 0, len(pm.opts)+1)
	opts = append(opts, pm.opts...)
	opts = append(opts, WithUserDataDir(dir))

	browser, tab, err := OpenPage(ctx, port, opts...)
	if err != nil {
		// 打开失败则回收刚分配的端口序号，避免端口空洞
		pm.next--
		return nil, nil, fmt.Errorf("打开 Profile %q 失败: %w", name, err)
	}

	pm.profiles[name] = &Profile{Name: name, Dir: dir, Port: port, browser: browser}
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
func (pm *ProfileManager) Close(name string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if p, ok := pm.profiles[name]; ok {
		if p.browser != nil {
			p.browser.Close()
		}
		delete(pm.profiles, name)
	}
}

// CloseAll 关闭所有已打开的 Profile 浏览器。磁盘用户数据目录全部保留。
func (pm *ProfileManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.closed = true
	for name, p := range pm.profiles {
		if p.browser != nil {
			p.browser.Close()
		}
		delete(pm.profiles, name)
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
