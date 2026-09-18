// main.go 是本仓库的本地综合测试 / 调试入口（示例文档请看 example/ 目录）。
// 拆分后这里只保留「单浏览器 + 多隔离上下文 + 多账户登录」一条真实登录流程，
// 站点与账户全部通过环境变量注入，仓库内不含任何真实站点与真实账户：
//
//	DEMO_SITE               目标站点（默认 https://example.com；默认站点无登录表单，退化为冒烟测试）
//	DEMO_USER_A/DEMO_PASS_A 账户 A 的用户名 / 密码
//	DEMO_USER_B/DEMO_PASS_B 账户 B 的用户名 / 密码
//
// PowerShell 示例：
//
//	$env:DEMO_SITE='https://your-site'
//	$env:DEMO_USER_A='user-a'; $env:DEMO_PASS_A='pass-a'
//	$env:DEMO_USER_B='user-b'; $env:DEMO_PASS_B='pass-b'
//	go run ./cmd/demo
//
// 之所以放在 cmd/demo 而不是模块根目录：根目录是库（package chromium / session），
// 若留一个 package main，`go install github.com/yymm456/go-drission@latest` 会装出一个
// 会去真实站点登录的 demo 程序，既不是用户想要的，也容易惹麻烦。
//
// 已拆分出去的示例：
//
//	example/concurrent_tabs  同一浏览器多标签页并发执行任务（互不干扰）+ 标签切换
//	example/profile          多进程浏览器隔离（ProfileManager，登录态持久化）
//	example/context          单浏览器多上下文 Cookie 隔离（含同窗口多标签共享会话证明）
package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// 测试账户（值来自环境变量，仓库不含真实凭据）
type account struct {
	label    string
	user     string
	password string
}

var (
	targetSite = envOr("DEMO_SITE", "https://example.com")
	accountA   = account{label: "A", user: envOr("DEMO_USER_A", "alice"), password: envOr("DEMO_PASS_A", "alice-pass")}
	accountB   = account{label: "B", user: envOr("DEMO_USER_B", "bob"), password: envOr("DEMO_PASS_B", "bob-pass")}
	shotDir    = "screenshots"
)

// envOr 读取环境变量，空则返回默认值
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// commonOpts 返回公共启动参数；目标若是自签证书站点，通过环境变量追加忽略证书错误参数
func commonOpts() []chromium.Option {
	opts := []chromium.Option{
		chromium.WithConnectTimeout(30 * time.Second),
	}
	if envOr("DEMO_INSECURE_TLS", "") != "" {
		opts = append(opts,
			chromium.WithFlag("ignore-certificate-errors", "true"),
			chromium.WithFlag("test-type", "true"),
		)
	}
	return opts
}

func main() {
	if err := os.MkdirAll(shotDir, 0o755); err != nil {
		log.Printf("创建截图目录失败: %v", err)
		return
	}
	ctx := context.Background()

	fmt.Printf("================ 单浏览器 + 多隔离上下文 + 多账户登录（站点=%s）================\n", targetSite)
	demoSingleBrowserIsolatedContexts(ctx)

	fmt.Println()
	fmt.Printf("完成，截图保存在 %s/ 目录；其余场景见 example/concurrent_tabs、example/profile、example/context\n", shotDir)
}

// demoSingleBrowserIsolatedContexts 一个浏览器进程内创建多个「隔离上下文」，各自登录不同账户。
//
// 语义提醒：
//   - browser.NewTab() 开的标签页共享同一份 Cookie / 会话，多标签只能是同一账户；
//   - browser.Context(name) 创建隔离上下文（类似无痕，可并存多个），各自独立 Cookie / 存储；
//   - 同一上下文内多标签同窗口、可来回切换；不同上下文各占一个窗口（Chrome 无痕档案规则），
//     但所有窗口同属一个浏览器进程（证明见 demoTabSwitching）。
func demoSingleBrowserIsolatedContexts(ctx context.Context) {
	// 独立端口 + 独立用户数据目录启动全新 Chrome；两个账户共用这一个浏览器进程
	opts := append(commonOpts(), chromium.WithUserDataDir("./profiles/single-demo"))
	browser, _, err := chromium.OpenPage(ctx, 9230, opts...)
	if err != nil {
		log.Printf("[场景一] 打开浏览器失败: %v", err)
		return
	}
	defer browser.Close()

	fmt.Println("[上下文A] 新建隔离上下文，账户A 登录 ...")
	if err := loginInContext(ctx, browser, "ctxA", accountA, "single_ctxA"); err != nil {
		log.Printf("[上下文A] 失败: %v", err)
	}

	fmt.Println("[上下文B] 新建隔离上下文，账户B 登录（与A隔离）...")
	if err := loginInContext(ctx, browser, "ctxB", accountB, "single_ctxB"); err != nil {
		log.Printf("[上下文B] 失败: %v", err)
	}

	fmt.Printf("[场景一] 同一浏览器内并存的隔离上下文: %v\n", browser.Contexts())

	// 标签切换演示：同一上下文内多标签同窗口来回切换，并用 WindowID/PID 证明窗口与进程归属
	fmt.Println("[标签切换] 演示同一窗口内标签来回切换 + 窗口/进程归属证明 ...")
	demoTabSwitching(ctx, browser)
}

// loginInContext 在指定名字的隔离上下文里新建标签页并登录，随后打印该上下文的会话 Cookie
// 作为隔离证据（不同上下文的会话 Cookie 各不相同）。
func loginInContext(ctx context.Context, browser *chromium.Browser, name string, acc account, shotPrefix string) error {
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	bc, err := browser.Context(openCtx, name)
	if err != nil {
		return fmt.Errorf("创建隔离上下文失败: %w", err)
	}
	tab, err := bc.NewTab(openCtx)
	if err != nil {
		return fmt.Errorf("上下文内新建标签页失败: %w", err)
	}
	if err := login(tab, acc, shotPrefix); err != nil {
		return err
	}

	// 打印该上下文的会话 Cookie（隔离证据：两个上下文的 Cookie 互不相同）
	cookieCtx, cancelCookie := context.WithTimeout(tab.Ctx, 10*time.Second)
	defer cancelCookie()
	if cookies, err := tab.GetCookies(cookieCtx, targetSite); err == nil {
		fmt.Printf("    [上下文%s] 会话 Cookie 共 %d 条:", name, len(cookies))
		for i, c := range cookies {
			if i >= 5 {
				fmt.Printf(" ...")
				break
			}
			fmt.Printf(" %s=%s", c.Name, truncate(c.Value, 12))
		}
		fmt.Println()
	}
	return nil
}

// demoTabSwitching 演示隔离上下文的窗口语义，澄清「新窗口 = 多个浏览器」的误解：
//
//  1. 在 ctxA 内再开一个标签页：它与登录标签同处一个窗口，并来回切换（普通标签体验）；
//  2. 用 Tab.WindowID 客观证明：同上下文标签同窗口、不同上下文不同窗口；
//  3. 用 Browser.PID + Port 证明：所有窗口同属一个浏览器进程、一条调试连接。
func demoTabSwitching(ctx context.Context, browser *chromium.Browser) {
	bcA, err := browser.Context(ctx, "ctxA")
	if err != nil {
		log.Printf("[标签切换] 获取 ctxA 失败: %v", err)
		return
	}
	tabsA := bcA.Tabs()
	if len(tabsA) == 0 {
		log.Printf("[标签切换] ctxA 内没有标签页")
		return
	}
	tab1 := tabsA[0]

	// ctxA 内第二个标签页：落入 tab1 所在窗口（只有上下文首个标签才开新窗口）
	tab2, err := bcA.NewTab(ctx)
	if err != nil {
		log.Printf("[标签切换] ctxA 内开第二个标签失败: %v", err)
		return
	}

	// 关键：所有 Tab 操作必须用从该标签 tab.Ctx 派生的 ctx（携带 target 路由）；
	// 传裸 context 会让 chromedp.Run 另起一个临时浏览器，命令根本落不到目标标签上。
	t2Ctx, cancel2 := context.WithTimeout(tab2.Ctx, 30*time.Second)
	defer cancel2()
	if err := tab2.Navigate(t2Ctx, "about:blank"); err == nil {
		_, _ = tab2.Eval(t2Ctx, `document.body.innerHTML='<h1 style="font-size:40px;margin:40px">ctxA 第二个标签页：与登录标签同窗口</h1>'`)
	}

	t1Ctx, cancel1 := context.WithTimeout(tab1.Ctx, 30*time.Second)
	defer cancel1()
	wid1, err1 := tab1.WindowID(t1Ctx)
	wid2, err2 := tab2.WindowID(t2Ctx)
	if err1 != nil || err2 != nil {
		log.Printf("[标签切换] 获取 windowID 失败: %v / %v", err1, err2)
	}
	fmt.Printf("    ctxA 标签1 windowID=%d 标签2 windowID=%d 同窗口=%v\n", wid1, wid2, wid1 == wid2)

	if err := tab2.BringToFront(t2Ctx); err == nil {
		fmt.Println("    已切到 标签2（大字标记页）")
		shot := filepath.Join(shotDir, "single_tab_switch.png")
		if err := tab2.Screenshot(t2Ctx, shot); err == nil {
			fmt.Printf("    截图: %s\n", shot)
		}
	} else {
		log.Printf("[标签切换] 切到标签2失败: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	if err := tab1.BringToFront(t1Ctx); err == nil {
		fmt.Println("    已切回 标签1（账户A 登录页）")
	}
	time.Sleep(800 * time.Millisecond)

	// 跨上下文窗口对比 + 同进程证据
	bcB, err := browser.Context(ctx, "ctxB")
	if err != nil {
		log.Printf("[标签切换] 获取 ctxB 失败: %v", err)
		return
	}
	tabsB := bcB.Tabs()
	if len(tabsB) == 0 {
		log.Printf("[标签切换] ctxB 内没有标签页")
		return
	}
	tabB := tabsB[0]
	bCtx, cancelB := context.WithTimeout(tabB.Ctx, 15*time.Second)
	defer cancelB()
	widB, _ := tabB.WindowID(bCtx)
	fmt.Printf("    ctxA 窗口=%d ctxB 窗口=%d（不同隔离上下文=不同窗口，Chrome 无痕档案规则）\n", wid1, widB)
	fmt.Printf("    但所有窗口同属一个浏览器进程: PID=%d 调试端口=%d（并非多进程浏览器）\n", browser.PID(), browser.Port())
}

// login 在给定标签页执行登录流程：导航 → 填账号密码 → 点击登录 → 截图留证。
// 所有针对 tab 的操作 ctx 均从 tab.Ctx 派生。
//
// 关键约定：
//  1. Navigate 后用 Wait().URL() 确认真的跳转成功，避免"命令发出但页面没动"的假成功。
//  2. 表单检测改用轮询等待，而非一次 Eval 就下结论（SPA 挂载前元素不存在）。
//  3. 提交表单后等待离开登录页，而非死等固定秒数。
//  4. 交互前 BringToFront 置前（后台窗口节流会丢输入），输入后校验值、未生效回退 JS 赋值。
//
// 不接受外层 ctx：本函数的每一步超时都必须以 tab.Ctx 为父派生，
// 否则会丢掉 chromedp 的 target 路由信息（见 README「ctx 必须从 tab.Ctx 派生」）。
func login(tab *chromium.Tab, acc account, shotPrefix string) error {
	navCtx, cancel := context.WithTimeout(tab.Ctx, 60*time.Second)
	defer cancel()

	fmt.Printf("    [导航] %s ...\n", targetSite)
	if err := tab.Navigate(navCtx, targetSite); err != nil {
		return fmt.Errorf("导航失败: %w", err)
	}

	// 隔离上下文各自开一个 OS 窗口，后台窗口会被 Chrome 节流，
	// 导致 SendKeys 输入丢失；交互前先把该标签页置前。
	if err := tab.BringToFront(navCtx); err != nil {
		log.Printf("    [警告] 置前标签页失败: %v", err)
	}

	// 等 URL 真正落到目标站点（Navigate 返回不代表 DOM 就绪）
	host := targetSite
	if u, err := url.Parse(targetSite); err == nil && u.Host != "" {
		host = u.Host
	}
	if err := tab.Wait().URL(host).Timeout(20 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("    [警告] 等待 URL 稳定超时: %v", err)
	}
	if err := tab.Wait().Ready().Timeout(15 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("    [警告] 等待就绪超时: %v", err)
	}

	// 检测是否存在登录表单（轮询等待元素出现，兼容 SPA 异步渲染）
	if hasLoginForm(tab) {
		fmt.Printf("    [登录] 检测到登录表单，开始填写 ...\n")

		if err := tab.EleID("username").SendKeys(navCtx, acc.user); err != nil {
			return fmt.Errorf("填写用户名失败: %w", err)
		}
		if err := tab.EleID("password").SendKeys(navCtx, acc.password); err != nil {
			return fmt.Errorf("填写密码失败: %w", err)
		}

		// 校验输入是否真的写入（后台窗口节流可能吃掉 SendKeys），未生效则用 JS 直接赋值兜底
		if v, _ := tab.EleID("username").Attribute(navCtx, "value"); v != acc.user {
			fmt.Printf("    [警告] SendKeys 未生效(#username=%q)，回退 SetValue\n", v)
			if err := tab.EleID("username").SetValue(navCtx, acc.user); err != nil {
				return fmt.Errorf("SetValue 填写用户名失败: %w", err)
			}
			if err := tab.EleID("password").SetValue(navCtx, acc.password); err != nil {
				return fmt.Errorf("SetValue 填写密码失败: %w", err)
			}
		}
		if err := tab.EleCSS(".p-button-label").Click(navCtx); err != nil {
			return fmt.Errorf("点击登录失败: %w", err)
		}

		// 等待离开登录页（URL 不再包含 "login"），而不是死等固定秒数。
		// 注意 break 必须用标签跳出外层 for：写在 select 里的 break 只跳出 select，
		// 循环会继续空转直到 deadline，白白烧 CPU。
		waitCtx, waitCancel := context.WithTimeout(tab.Ctx, 20*time.Second)
		defer waitCancel()
	pollLogin:
		for {
			u, err := tab.CurrentURL(waitCtx)
			if err == nil && u != "" && !strings.Contains(u, "login") {
				break
			}
			select {
			case <-waitCtx.Done():
				break pollLogin
			case <-time.After(300 * time.Millisecond):
			}
		}

		fmt.Printf("    账户%s(%s) 已提交登录表单\n", acc.label, acc.user)
	} else {
		fmt.Printf("    账户%s(%s) 检测到已是登录态或无登录表单，跳过登录\n", acc.label, acc.user)
	}

	// 截图/读取标题用独立 ctx：navCtx 已被前面的导航+等待消耗了大半，
	// 共享它会在慢机器上把截图拖到 deadline exceeded。
	shotCtx, cancelShot := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancelShot()

	shot := filepath.Join(shotDir, shotPrefix+"_login.png")
	if err := tab.Screenshot(shotCtx, shot); err != nil {
		return fmt.Errorf("截图失败: %w", err)
	}
	title, _ := tab.Title(shotCtx)
	curURL, _ := tab.CurrentURL(shotCtx)
	fmt.Printf("    → 标题=%q 地址=%s\n", title, truncate(curURL, 60))
	fmt.Printf("    截图: %s\n", shot)
	return nil
}

// hasLoginForm 判断当前页面是否存在登录表单（#username）。
// 轮询等待元素出现（最多 8 秒）：页面可能是异步渲染的（Vue/React），
// 挂载完成前 #username 不存在，一次 Eval 会误判为"已登录"。
func hasLoginForm(tab *chromium.Tab) bool {
	ctx, cancel := context.WithTimeout(tab.Ctx, 8*time.Second)
	defer cancel()

	for {
		v, err := tab.Eval(ctx, "!!document.querySelector('#username')")
		if err == nil {
			if b, ok := v.(bool); ok && b {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
