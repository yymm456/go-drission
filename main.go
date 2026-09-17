package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yymm456/go-drission/chromium"
	"log"
	"time"
)

func main() {
	// 顶层上下文：连接浏览器用
	rootCtx := context.Background()

	fmt.Println("[1] 连接浏览器...")
	browser, tab, err := chromium.OpenPage(rootCtx, 9222,
		chromium.WithConnectTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()
	fmt.Println("[1] 连接完成")

	// 关键约定：所有针对 tab 的操作，ctx 必须从 tab.Ctx 派生，
	// 才能携带 chromedp 的 target 路由信息；超时/取消由调用方通过 ctx 控制。

	fmt.Println("[2] 启动监听...")
	// 监听生命周期用独立的可取消 ctx（从 tab.Ctx 派生）
	listenCtx, stopListen := context.WithCancel(tab.Ctx)
	defer stopListen()

	// pattern 为子串包含匹配：URL 含该子串即命中（自动兼容带 query 的请求）
	listener := tab.Listen("cm.bilibili.com/cm/api/receive/content/pc")
	if err := listener.Start(listenCtx); err != nil {
		log.Fatal(err)
	}
	defer listener.Stop()
	fmt.Println("[2] 监听已启动")

	fmt.Println("[3] 导航到 bilibili.com...")
	navCtx, cancelNav := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancelNav()
	if err := tab.Navigate(navCtx, "https://www.bilibili.com"); err != nil {
		log.Fatal(err)
	}

	fmt.Println("[4] 等待页面加载...")
	readyCtx, cancelReady := context.WithTimeout(tab.Ctx, 15*time.Second)
	defer cancelReady()
	_ = tab.WaitReady(readyCtx)
	// time.Sleep(3 * time.Second) // 给 XHR / 上报类请求一些时间
	listener.WaitIdle() // 等待异步 GetResponseBody 回填完成

	records := listener.Records()
	fmt.Printf("[5] 捕获到 %d 条记录\n\n", len(records))

	var hasReqBody, hasRespHeaders, hasRespBody int

	for i, rec := range records {
		if len(rec.RequestBody) > 0 {
			hasReqBody++
		}
		if len(rec.ResponseHeaders) > 0 {
			hasRespHeaders++
		}
		if len(rec.ResponseBody) > 0 {
			hasRespBody++
		}

		if i >= 20 {
			fmt.Printf("... 只显示前 20 条，省略剩余 %d 条\n", len(records)-20)
			break
		}

		fmt.Printf("===== 记录 %d =====\n", i+1)
		fmt.Printf("URL: %s\n", truncate(rec.URL, 100))
		fmt.Printf("Method: %s\n", rec.Method)
		fmt.Printf("Status: %d\n", rec.Status)
		fmt.Printf("RequestHeaders: %d 项\n", len(rec.RequestHeaders))
		fmt.Printf("RequestBody: %d 字节\n", len(rec.RequestBody))
		if len(rec.RequestBody) > 0 {
			fmt.Printf("  内容: %s\n", truncate(rec.RequestBody, 200))
		}
		fmt.Printf("ResponseHeaders: %d 项\n", len(rec.ResponseHeaders))
		if len(rec.ResponseHeaders) > 0 {
			if data, err := json.MarshalIndent(rec.ResponseHeaders, "  ", "  "); err == nil {
				fmt.Printf("  %s\n", truncate(string(data), 300))
			}
		}
		fmt.Printf("ResponseBody: %d 字节\n", len(rec.ResponseBody))
		if len(rec.ResponseBody) > 0 {
			fmt.Printf("  内容: %s\n", truncate(rec.ResponseBody, 200))
		}
		fmt.Println()
	}

	fmt.Printf("===== 统计 =====\n")
	fmt.Printf("总记录: %d\n", len(records))
	fmt.Printf("有请求体: %d\n", hasReqBody)
	fmt.Printf("有响应头: %d\n", hasRespHeaders)
	fmt.Printf("有响应体: %d\n", hasRespBody)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
