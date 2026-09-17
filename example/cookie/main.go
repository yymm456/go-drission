// example/cookie 演示 Cookie 的注入、读取、导出与导入。
//
//	go run ./example/cookie
package main

import (
	"context"
	"log"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func main() {
	ctx := context.Background()

	browser, tab, err := chromium.OpenPage(ctx, 9222, chromium.WithConnectTimeout(5*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancel()
	if err := tab.Navigate(navCtx, "https://example.com"); err != nil {
		log.Fatal(err)
	}
	_ = tab.Wait().Ready().Timeout(10 * time.Second).Do(tab.Ctx)

	// 1) 注入单个 Cookie
	c := chromium.Cookie{
		Name:   "demo",
		Value:  "hello-go-drission",
		Domain: "example.com",
		Path:   "/",
	}
	if err := tab.SetCookie(navCtx, c); err != nil {
		log.Printf("注入 Cookie 失败: %v", err)
	} else {
		log.Println("已注入 Cookie: demo")
	}

	// 2) 读取指定 URL 下的 Cookie
	cookies, err := tab.GetCookies(navCtx, "https://example.com")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("当前 Cookie 数量: %d", len(cookies))

	// 3) 导出到 JSON 文件
	if err := tab.ExportCookies(navCtx, "cookies.json", "https://example.com"); err != nil {
		log.Printf("导出 Cookie 失败: %v", err)
	} else {
		log.Println("已导出 Cookie 到 cookies.json")
	}

	// 4) 从 JSON 文件导入（常用于把已保存的登录态恢复到新会话/隔离上下文）
	if err := tab.ImportCookies(navCtx, "cookies.json"); err != nil {
		log.Printf("导入 Cookie 失败: %v", err)
	} else {
		log.Println("已从 cookies.json 导入 Cookie")
	}
}
