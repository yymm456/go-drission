//go:build smoke

package smoke

// 本文件覆盖两件单元测试回答不了的事：
//
//  1. **资源泄漏**：Chrome 进程有没有真的退出、goroutine 会不会一轮轮涨上去、
//     档案目录的排他锁在关闭后能不能释放。
//  2. **长时间稳定性**：反复启停、反复开关标签页之后，库还稳不稳。
//
// 为什么必须放 smoke：这些断言都依赖真实 Chrome 进程与真实端口，
// 纯单元测试里无从观察。
//
// 规模由环境变量控制，默认取小值以便每次提交都能跑：
//
//	GO_DRISSION_STABILITY_ROUNDS=30 go test -tags smoke -run TestStability ./smoke/...
//
// 想做文档里那种「30 分钟 / 1 小时」的长时间观察，把轮次调到几百即可，
// 逻辑与本文件完全一致，不需要另写一套。

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// ---------------------------------------------------------------- 端口分配
//
// 稳定性测试要反复起停 Chrome 上百次，端口段必须避开日常使用的低位端口。
//
// 为什么不能传 0 让库自动分配：库在 port==0 时会先试 FindFreePort，
// 但**分配失败就退回 9222**（见 browser.go）。9222 恰恰是最常见的
// 「用户自己开着 Chrome 调试」的端口；接管上去之后，测的就不再是新起的
// 干净实例 —— 要么连到别人的浏览器，要么直接失败，两种都让结论不可信。
const (
	portBase  = 40000
	portCeil  = 50000
	portBlock = 20 // 给 ProfileManager 预留的连续端口块大小
)

var portNext atomic.Int32

func init() { portNext.Store(portBase) }

// nextFreePort 取下一个当前未被监听的高位端口。
//
// 检查「是否已被监听」而不是无脑递增：机器上有可能真有服务占了 4xxxx，
// 撞上去会让整轮启停失败，而失败原因离端口很远，很难查。
func nextFreePort() int {
	for range portCeil - portBase {
		p := int(portNext.Add(1))
		if p >= portCeil {
			portNext.Store(portBase)
			p = int(portNext.Add(1))
		}
		if !portStillListening(p) {
			return p
		}
	}
	// 整段都被占用：不该发生，明确报出来比默默用错端口好
	panic("40000-50000 段内找不到空闲端口")
}

// nextProfileBasePort 给 ProfileManager 用：它内部会从 basePort 起按打开顺序递增，
// 所以要一次预留一整块连续端口，不能跟单实例测试抢同一个计数器。
func nextProfileBasePort() int {
	p := int(portNext.Add(portBlock))
	if p >= portCeil {
		portNext.Store(portBase)
		p = int(portNext.Add(portBlock))
	}
	return p
}

// stabilityRounds 读取重复次数，默认 5。
//
// 取小值是有意的：这套用例要真起 Chrome，一次冷启动 1~2 秒，
// 默认 5 轮约 10 秒，可以进日常验证；想做长时间观察再显式调大。
func stabilityRounds() int {
	if s := os.Getenv("GO_DRISSION_STABILITY_ROUNDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 5
}

// portStillListening 判断本机端口是否还能连上。
//
// 这是跨平台判断「Chrome 到底退没退」的手段：关闭后调试端口应当连不上。
// 不用 os.FindProcess —— 它在 Windows 上无论进程是否存在都返回成功，
// 拿它做存活判断会得到假阴性。
func portStillListening(port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// waitPortClosed 等待端口不再监听，返回是否等到。
func waitPortClosed(port int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !portStillListening(port) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// newIsolatedBrowser 起一个独立 Browser：独立端口 + 独立用户数据目录，
// 并注册关闭，避免污染 setup() 的共享实例。
//
// 用户数据目录必须显式给：不指定时库会按端口在 %TEMP% 下建目录，
// 且刻意不删（真实登录态在里面），每跑一轮就留几百 MB。
// 这里交 t.TempDir()，并在它之后注册 Close —— Cleanup 按 LIFO 执行，
// 保证「先关浏览器、再删目录」，否则目录里有被 Chrome 独占的文件删不掉。
func newIsolatedBrowser(t *testing.T) (*chromium.Browser, *chromium.Tab) {
	t.Helper()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, tab, err := chromium.OpenPage(ctx, nextFreePort(),
		chromium.WithUserDataDir(dir),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	if err != nil {
		if errors.Is(err, chromium.ErrChromeNotFound) {
			t.Skipf("本机没有可用浏览器，跳过：%v", err)
		}
		t.Fatalf("启动独立浏览器失败：%v", err)
	}
	t.Cleanup(b.Close)
	return b, tab
}

// skipIfNoChrome 在浏览器不存在时跳过用例。
//
// 判据直接取 pm.Open / OpenPage 的返回，不另起一个浏览器来「探测」：
// 探测用的浏览器如果忘了关，就会泄漏一个 Chrome 进程，反而污染后面的断言。
func skipIfNoChrome(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, chromium.ErrChromeNotFound) {
		t.Skipf("本机没有可用浏览器，跳过：%v", err)
	}
}

// tempProfileDir 建一个由本用例自己管理的档案根目录。
//
// 不用 t.TempDir()：它的清理会在 CloseAll 之后紧接着执行，而 Chrome 进程退出是
// 异步的 —— crashpad 之类的文件这时还被占着，RemoveAll 必然失败并把用例判红。
// 那是清理时序问题，不是被测代码的问题。这里自己等一会儿再删，并容忍删不掉
// （临时目录，最坏情况只是留一点垃圾）。
func tempProfileDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "go-drission-profile-")
	if err != nil {
		t.Fatalf("创建临时目录失败：%v", err)
	}
	t.Cleanup(func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(dir)
	})
	return dir
}

// TestStabilityChromeExitsOnClose 验证 Close 之后 Chrome 真的退了。
//
// 这是最容易静默失败的一类泄漏：如果进程树的回收漏了一条路径，
// 测试进程退出后系统仍会留下一堆 Chrome，跑几十轮就把机器拖垮。
func TestStabilityChromeExitsOnClose(t *testing.T) {
	b, _ := newIsolatedBrowser(t)
	port := b.Port()
	if port <= 0 {
		t.Fatalf("Port() = %d，期望拿到实际调试端口", port)
	}
	if !portStillListening(port) {
		t.Fatalf("端口 %d 在浏览器还活着时就连不上，后续断言失去意义", port)
	}

	b.Close()

	// 进程退出与端口释放都不是瞬时的，给一段宽限时间再判定。
	if !waitPortClosed(port, 10*time.Second) {
		t.Errorf("Close() 之后端口 %d 仍在监听，Chrome 进程没有退出（PID=%d）", port, b.PID())
	}
}

// TestStabilityRepeatedOpenClose 反复启停 Browser，检查不 panic、不残留。
//
// 顺带覆盖「重复关闭」：Close 被调两次不应 panic（真实代码里 defer 与
// 显式 Close 很容易撞在一起）。
func TestStabilityRepeatedOpenClose(t *testing.T) {
	rounds := stabilityRounds()
	for i := 0; i < rounds; i++ {
		b, tab := newIsolatedBrowser(t)
		port := b.Port()

		ctx := ctxOf(t, tab, 20*time.Second)
		if _, err := b.NewTab(ctx); err != nil {
			t.Fatalf("第 %d 轮新建标签页失败：%v", i+1, err)
		}

		b.Close()
		b.Close() // 重复关闭必须安全

		if !waitPortClosed(port, 10*time.Second) {
			t.Fatalf("第 %d 轮 Close 后端口 %d 仍在监听", i+1, port)
		}
	}
}

// goroutineSlack 是允许的 goroutine 增长余量。
//
// 余量必须与轮次无关：若写成 rounds*N，那「每轮泄漏 N 个」这种货真价实的
// 泄漏就会被自己的阈值放过去，断言等于没有。
func goroutineSlack() int { return 30 }

// TestStabilityGoroutineNoGrowth 检查 goroutine 不随启停轮次持续增长。
//
// 判定方式：先跑一轮预热（CDP 事件分发等后台 goroutine 会在这时建立），
// 取基线；再跑 N 轮并等待系统安静下来比较前后差值。
// 断言的是「不随轮次线性增长」而不是「绝对回到原值」——后台 goroutine
// 的退出不是瞬时的，死等一个精确值只会得到随机失败。
func TestStabilityGoroutineNoGrowth(t *testing.T) {
	rounds := stabilityRounds()

	// 预热：把「首次建连」带来的一次性 goroutine 先建起来，不计入基线。
	{
		b, _ := newIsolatedBrowser(t)
		b.Close()
	}
	time.Sleep(2 * time.Second)

	baseline := runtime.NumGoroutine()

	for i := 0; i < rounds; i++ {
		b, tab := newIsolatedBrowser(t)
		ctx := ctxOf(t, tab, 20*time.Second)
		if _, err := b.NewTab(ctx); err != nil {
			t.Fatalf("第 %d 轮新建标签页失败：%v", i+1, err)
		}
		b.Close()
	}

	// 等后台 goroutine 收尾，最多等 15 秒
	after := runtime.NumGoroutine()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= baseline+goroutineSlack() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Logf("goroutine: 基线 %d → 结束 %d（%d 轮，允许 +%d）", baseline, after, rounds, goroutineSlack())
	if after > baseline+goroutineSlack() {
		t.Errorf("goroutine 数从 %d 涨到 %d，疑似泄漏", baseline, after)
	}
}

// TestStabilityTabChurn 反复创建并关闭标签页，确认 Browser 侧登记同步。
//
// 关注点是 b.tabs 的同步：新建的标签页要能被 Tabs() 看到，
// 关掉之后要从登记里消失，否则长时间运行会积累一堆陈旧条目。
func TestStabilityTabChurn(t *testing.T) {
	b, tab := setup(t)
	rounds := stabilityRounds() * 2

	base, err := b.Tabs(ctxOf(t, tab, 20*time.Second))
	if err != nil {
		t.Fatalf("读取初始标签页列表失败：%v", err)
	}

	for i := 0; i < rounds; i++ {
		ctx := ctxOf(t, tab, 20*time.Second)
		newTab, err := b.NewTab(ctx)
		if err != nil {
			t.Fatalf("第 %d 次 NewTab 失败：%v", i+1, err)
		}
		b.CloseTab(ctx, newTab)
	}

	// 关完应当回到起始数量；给同步一点时间
	var got int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tabs, err := b.Tabs(ctxOf(t, tab, 20*time.Second))
		if err != nil {
			t.Fatalf("读取标签页列表失败：%v", err)
		}
		got = len(tabs)
		if got <= len(base) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got > len(base) {
		t.Errorf("标签页数量没有回到基线：起始 %d，结束 %d（新建并关闭了 %d 个）", len(base), got, rounds)
	}
}

// TestStabilityProfileLockReleased 验证档案关闭后锁被释放，同目录能再次打开。
//
// Profile 用 OS 级排他文件锁。若库侧漏了 Release，第二轮 Open 会直接失败，
// 而且失败位置离真正的错误很远，非常难查 —— 所以必须钉住。
func TestStabilityProfileLockReleased(t *testing.T) {
	pm := chromium.NewProfileManager(tempProfileDir(t), nextProfileBasePort(),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
		chromium.WithFlag("no-proxy-server", ""),
	)
	t.Cleanup(pm.CloseAll)

	const name = "lock-check"
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, _, err := pm.Open(ctx, name)
		cancel()
		if err != nil {
			skipIfNoChrome(t, err)
			t.Fatalf("第 %d 次打开档案 %q 失败（锁可能没释放）：%v", i+1, name, err)
		}
		pm.Close(name)
	}
}

// TestStabilityDoubleCloseProfile 重复关闭同名档案、以及关闭从未打开过的档案，
// 都不应 panic。
func TestStabilityDoubleCloseProfile(t *testing.T) {
	pm := chromium.NewProfileManager(tempProfileDir(t), nextProfileBasePort(),
		chromium.WithHeadless(true),
		chromium.WithDefaultTimeout(15*time.Second),
	)
	t.Cleanup(pm.CloseAll)

	pm.Close("never-opened")
	pm.Close("never-opened")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	_, _, err := pm.Open(ctx, "twice")
	cancel()
	if err != nil {
		skipIfNoChrome(t, err)
		t.Fatalf("打开档案失败：%v", err)
	}
	pm.Close("twice")
	pm.Close("twice") // 重复关闭必须安全
}
