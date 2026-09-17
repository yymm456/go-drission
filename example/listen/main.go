// example/listen 演示被动监听网络请求：捕获匹配 URL 的请求/响应完整信息。
//
//	go run ./example/listen
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func main() {
	rootCtx := context.Background()

	browser, tab, err := chromium.OpenPage(rootCtx, 9222, chromium.WithConnectTimeout(5*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	// 监听生命周期用独立可取消 ctx（从 tab.Ctx 派生）
	listenCtx, stop := context.WithCancel(tab.Ctx)
	defer stop()

	// pattern 为「子串包含」匹配：URL 含该子串即命中（自动兼容带 query 的请求）
	listener := tab.Listen("cm.bilibili.com/cm/api/receive/content/pc")
	if err := listener.Start(listenCtx); err != nil {
		log.Fatal(err)
	}
	defer listener.Stop()

	navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancel()
	if err := tab.Navigate(navCtx, "https://www.bilibili.com"); err != nil {
		log.Fatal(err)
	}

	readyCtx, cancelReady := context.WithTimeout(tab.Ctx, 15*time.Second)
	defer cancelReady()
	_ = tab.Wait().Ready().Do(readyCtx)

	listener.WaitIdle() // 等待异步 GetResponseBody 回填完成

	records := listener.Records()
	fmt.Printf("捕获到 %d 条记录\n", len(records))
	for i, rec := range records {
		if i >= 10 {
			fmt.Printf("... 仅显示前 10 条，省略剩余 %d 条\n", len(records)-10)
			break
		}
		fmt.Printf("[%d] %s %s -> %d（请求体 %d 字节，响应体 %d 字节）\n",
			i+1, rec.Method, truncate(rec.URL, 80), rec.Status, len(rec.RequestBody), len(rec.ResponseBody))
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
