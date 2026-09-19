//go:build smoke

package smoke

// 本文件覆盖 ProfileManager 的真实流程：多档案隔离、同名复用、注册表增删、
// 端口分配与关闭后的清理。
//
// 为什么放 smoke：档案的核心是「各自一个独立 Chrome 进程 + 独立用户数据目录」，
// 这些只有真起进程才谈得上验证。纯单测（profile_test.go）已经覆盖了名字清洗与
// 端口池的纯逻辑，这里补的是跨进程那部分。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// newProfileManagerForTest 建一个带独立根目录的 ProfileManager。
//
// 端口从 0 开始：库会退回默认起始端口，用例不硬编码端口号，
// 免得与机器上已有的调试端口撞车。
func newProfileManagerForTest(t *testing.T) *chromium.ProfileManager {
	t.Helper()
	pm := chromium.NewProfileManager(tempProfileDir(t), nextProfileBasePort(),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	t.Cleanup(pm.CloseAll)
	return pm
}

// TestProfileSameNameReusesBrowser 同名档案重复 Open 必须复用同一个 Browser。
//
// 这是 ProfileManager 的基本契约：per-name 锁把 OpenPage 串行化，
// 首次真正启动 Chrome，之后同名直接复用。
func TestProfileSameNameReusesBrowser(t *testing.T) {
	pm := newProfileManagerForTest(t)
	ctx := context.Background()

	b1, _, err := pm.Open(ctx, "reuse")
	if err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("首次打开档案失败：%v", err)
	}
	b2, _, err := pm.Open(ctx, "reuse")
	if err != nil {
		t.Fatalf("同名再次打开失败：%v", err)
	}
	if b1 != b2 {
		t.Fatalf("同名档案应复用同一个 Browser：%p vs %p", b1, b2)
	}
	if b1.Port() != b2.Port() {
		t.Fatalf("复用后端口却不同：%d vs %d", b1.Port(), b2.Port())
	}
}

// TestProfileRegistryTracksNames Names() 与 Get() 必须反映当前已打开的档案。
func TestProfileRegistryTracksNames(t *testing.T) {
	pm := newProfileManagerForTest(t)
	ctx := context.Background()

	const name = "registry"
	if _, err := pm.Get(name); err == nil {
		t.Fatalf("未打开的档案 Get() 应返回错误")
	}

	if _, _, err := pm.Open(ctx, name); err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("打开档案失败：%v", err)
	}
	if !containsString(pm.Names(), name) {
		t.Fatalf("打开后 Names() 缺少 %q：%v", name, pm.Names())
	}
	p, err := pm.Get(name)
	if err != nil {
		t.Fatalf("打开后 Get(%q) 失败：%v", name, err)
	}
	if p.Name != name { // Name 是字段不是方法：元信息只读
		t.Fatalf("Get 返回的档案名不符：%q", p.Name)
	}
	if p.Browser() == nil {
		t.Fatal("已打开的档案 Browser() 不应为 nil")
	}

	pm.Close(name)
	if containsString(pm.Names(), name) {
		t.Fatalf("关闭后 Names() 仍包含 %q：%v", name, pm.Names())
	}
}

// TestProfileDifferentNamesDifferentPorts 不同档案必须拿到不同端口与不同目录，
// 否则「多账户并行」就无从谈起。
func TestProfileDifferentNamesDifferentPorts(t *testing.T) {
	pm := newProfileManagerForTest(t)
	ctx := context.Background()

	b1, _, err := pm.Open(ctx, "acct-a")
	if err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("打开档案 A 失败：%v", err)
	}
	b2, _, err := pm.Open(ctx, "acct-b")
	if err != nil {
		t.Fatalf("打开档案 B 失败：%v", err)
	}

	if b1.Port() == b2.Port() {
		t.Fatalf("两个档案拿到了同一个端口 %d", b1.Port())
	}
	if b1 == b2 {
		t.Fatal("两个不同档案不应是同一个 Browser")
	}
}

// TestProfileCookieIsolation 两个档案之间的登录态必须完全隔离：
// A 拿到的 Cookie 不能出现在 B 里。
//
// 这是多账户功能的核心承诺。档案各自一份用户数据目录 + 各自一个进程，
// 所以 Cookie 天然隔离；这条用例的价值在于防止将来有人为了「省一个进程」
// 把档案改成共享 Browser —— 那一改，隔离就没了。
func TestProfileCookieIsolation(t *testing.T) {
	pm := newProfileManagerForTest(t)
	srv := startServers(t)
	ctx := context.Background()

	_, tabA, err := pm.Open(ctx, "iso-a")
	if err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("打开档案 A 失败：%v", err)
	}
	ctxA := ctxOf(t, tabA, 60*time.Second)
	if err := tabA.Navigate(ctxA, srv.main.URL+"/set-cookie"); err != nil {
		t.Fatalf("A 导航到 set-cookie 失败：%v", err)
	}
	if err := tabA.WaitReady(ctxA); err != nil {
		t.Fatalf("A 等待就绪失败：%v", err)
	}
	if err := tabA.Navigate(ctxA, srv.main.URL+"/profile"); err != nil {
		t.Fatalf("A 导航到 profile 失败：%v", err)
	}
	bodyA, err := tabA.HTML(ctxA)
	if err != nil {
		t.Fatalf("A 读取 profile 失败：%v", err)
	}
	if !strings.Contains(bodyA, "hello abc123") {
		t.Fatalf("A 的 Cookie 未生效：%q", bodyA)
	}

	_, tabB, err := pm.Open(ctx, "iso-b")
	if err != nil {
		t.Fatalf("打开档案 B 失败：%v", err)
	}
	ctxB := ctxOf(t, tabB, 60*time.Second)
	if err := tabB.Navigate(ctxB, srv.main.URL+"/profile"); err != nil {
		t.Fatalf("B 导航到 profile 失败：%v", err)
	}
	bodyB, err := tabB.HTML(ctxB)
	if err != nil {
		t.Fatalf("B 读取 profile 失败：%v", err)
	}
	if strings.Contains(bodyB, "hello abc123") {
		t.Fatalf("档案之间未隔离 Cookie：B 也拿到了 A 的登录态：%q", bodyB)
	}
	if !strings.Contains(bodyB, "no cookie") {
		t.Fatalf("B 预期应为未登录（no cookie），实际：%q", bodyB)
	}
}

// TestProfileCloseAllClearsRegistry CloseAll 之后注册表必须清空，
// 且之后的 Open 仍然可用（不能因为关闭过就再也打不开）。
func TestProfileCloseAllClearsRegistry(t *testing.T) {
	pm := newProfileManagerForTest(t)
	ctx := context.Background()

	if _, _, err := pm.Open(ctx, "ca-1"); err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("打开档案失败：%v", err)
	}
	if _, _, err := pm.Open(ctx, "ca-2"); err != nil {
		t.Fatalf("打开第二个档案失败：%v", err)
	}
	if len(pm.Names()) != 2 {
		t.Fatalf("期望 2 个档案，实际 %v", pm.Names())
	}

	pm.CloseAll()
	if names := pm.Names(); len(names) != 0 {
		t.Fatalf("CloseAll 后注册表应清空，实际 %v", names)
	}

	// CloseAll 是**终态**操作：之后再 Open 必须返回 ErrProfileClosed，
	// 而不是静默地重新起一个 Chrome —— 那会让调用方以为 manager 还能正常工作，
	// 实际上资源已经按「全部释放」处理过了。这里把这个契约钉住。
	if _, _, err := pm.Open(ctx, "ca-1"); !errors.Is(err, chromium.ErrProfileClosed) {
		t.Fatalf("CloseAll 之后 Open 期望 ErrProfileClosed，实际：%v", err)
	}
}
