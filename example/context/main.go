// example/context 演示「单浏览器内多上下文 Cookie 隔离」：同一个 Chrome 进程里创建多个
// 隔离上下文（BrowserContext），各自拥有独立 Cookie / 存储（类似无痕，可并存多个）。
//
// 本示例用 Cookie 直观证明三件事：
//  1. 不同隔离上下文的 Cookie 互不可见（各存各的会话令牌）；
//  2. 同一上下文内的多个标签页共享 Cookie（同窗口多标签 = 同一会话）；
//  3. 窗口语义：同上下文标签同窗口（可来回切换），不同上下文各占一个窗口，但同属一个浏览器进程。
//
// 特点：轻量，多账户共用一个浏览器进程；但为内存态，Chrome 关闭后登录态丢失。
// 需要跨重启持久登录请看 example/profile。
//
//	go run ./example/context
package main

import (
	"context"
	"log"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

const site = "https://example.com"

func main() {
	ctx := context.Background()

	// 一个浏览器进程；默认标签页用不到，忽略之，改为在各隔离上下文里建标签页
	browser, _, err := chromium.OpenPage(ctx, 9222, chromium.WithConnectTimeout(15*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	// 两个隔离上下文：模拟两个账户各自的会话空间
	ctxA, err := browser.Context(ctx, "user_a")
	if err != nil {
		log.Fatal(err)
	}
	defer ctxA.Close()
	ctxB, err := browser.Context(ctx, "user_b")
	if err != nil {
		log.Fatal(err)
	}
	defer ctxB.Close()

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	defer cancelOpen()
	tabA, err := ctxA.NewTab(openCtx)
	if err != nil {
		log.Fatal(err)
	}
	tabB, err := ctxB.NewTab(openCtx)
	if err != nil {
		log.Fatal(err)
	}

	// 各自导航到同一站点，并写入各自的「会话 Cookie」（值不同，模拟不同账户登录态）
	navA, cancelA := context.WithTimeout(tabA.Ctx, 30*time.Second)
	defer cancelA()
	if err := tabA.Navigate(navA, site); err != nil {
		log.Fatal(err)
	}
	navB, cancelB := context.WithTimeout(tabB.Ctx, 30*time.Second)
	defer cancelB()
	if err := tabB.Navigate(navB, site); err != nil {
		log.Fatal(err)
	}
	if err := tabA.SetCookie(navA, chromium.Cookie{Name: "session", Value: "token-of-user_a", Domain: "example.com"}); err != nil {
		log.Fatal(err)
	}
	if err := tabB.SetCookie(navB, chromium.Cookie{Name: "session", Value: "token-of-user_b", Domain: "example.com"}); err != nil {
		log.Fatal(err)
	}

	// 证明 1：Cookie 隔离——各上下文只能读到自己写入的会话令牌
	log.Printf("上下文 user_a 的 Cookie: %s", cookieSummary(navA, tabA))
	log.Printf("上下文 user_b 的 Cookie: %s", cookieSummary(navB, tabB))

	// 证明 2：同上下文多标签共享 Cookie——user_a 再开一个标签页，会话令牌与 tabA 相同
	tabA2, err := ctxA.NewTab(openCtx)
	if err != nil {
		log.Fatal(err)
	}
	navA2, cancelA2 := context.WithTimeout(tabA2.Ctx, 30*time.Second)
	defer cancelA2()
	if err := tabA2.Navigate(navA2, site); err != nil {
		log.Fatal(err)
	}
	log.Printf("上下文 user_a 第二个标签页的 Cookie: %s（应与第一个标签相同）", cookieSummary(navA2, tabA2))

	// 证明 3：窗口语义——同上下文标签同窗口、不同上下文不同窗口、但同属一个浏览器进程
	widA, _ := tabA.WindowID(navA)
	widA2, _ := tabA2.WindowID(navA2)
	widB, _ := tabB.WindowID(navB)
	log.Printf("windowID: user_a 标签1=%d 标签2=%d（同窗口=%v）; user_b 标签=%d", widA, widA2, widA == widA2, widB)
	log.Printf("浏览器进程 PID=%d 端口=%d（所有窗口同属这一个进程）", browser.PID(), browser.Port())

	// 标签切换：同窗口内把 user_a 的第二个标签切到前台，再切回第一个
	if err := tabA2.BringToFront(navA2); err == nil {
		log.Println("已切到 user_a 标签2")
		time.Sleep(500 * time.Millisecond)
	}
	if err := tabA.BringToFront(navA); err == nil {
		log.Println("已切回 user_a 标签1")
	}

	log.Printf("当前所有隔离上下文: %v", browser.Contexts())
}

// cookieSummary 读取标签页在 site 下的 Cookie，压缩成 "name=value, ..." 便于对比
func cookieSummary(ctx context.Context, tab *chromium.Tab) string {
	cookies, err := tab.GetCookies(ctx, site)
	if err != nil {
		return "(读取失败: " + err.Error() + ")"
	}
	out := ""
	for i, c := range cookies {
		if i > 0 {
			out += ", "
		}
		out += c.Name + "=" + c.Value
	}
	if out == "" {
		out = "(空)"
	}
	return out
}
