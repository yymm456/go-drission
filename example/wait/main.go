// example/wait 演示链式 Wait Builder 的各种等待条件。
//
//	go run ./example/wait
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

	// 1) 等页面就绪
	if err := tab.Wait().Ready().Timeout(10 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待就绪失败: %v", err)
	} else {
		log.Println("页面已就绪")
	}

	// 2) 等元素可见
	if err := tab.Wait().Element("h1").Visible().Timeout(5 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("h1 不可见: %v", err)
	} else {
		log.Println("h1 已可见")
	}

	// 3) 等元素数量 >= n
	if err := tab.Wait().Element("a").Count(1).Timeout(5 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("链接数量不足: %v", err)
	} else {
		log.Println("至少有 1 个 <a>")
	}

	// 4) 等元素文本包含子串
	if err := tab.Wait().Element("h1").Text("Example").Timeout(5 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("h1 文本不匹配: %v", err)
	} else {
		log.Println("h1 文本包含 Example")
	}

	// 5) 等 URL 包含子串
	if err := tab.Wait().URL("example.com").Timeout(5 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("URL 不匹配: %v", err)
	} else {
		log.Println("URL 命中 example.com")
	}

	// 未指定条件时 Do 会明确报错，而不是静默失败
	if err := tab.Wait().Timeout(time.Second).Do(tab.Ctx); err != nil {
		log.Printf("预期的校验错误: %v", err)
	}
}
