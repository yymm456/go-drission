// example/basic 演示最基础的流程：连接浏览器、导航、等待就绪、读取标题与地址、截图。
//
// 运行前请确保 Chrome 可用（未开启调试端口时库会自动启动一个）。
//
//	go run ./example/basic
package main

import (
	"context"
	"log"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func main() {
	ctx := context.Background()

	// port 传具体值 = 固定端口：活着就直接连，没活就自动启动 Chrome
	browser, tab, err := chromium.OpenPage(ctx, 9222,
		chromium.WithConnectTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	// 关键约定：所有针对 tab 的操作，ctx 必须从 tab.Ctx 派生
	navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancel()
	if err := tab.Navigate(navCtx, "https://example.com"); err != nil {
		log.Fatal(err)
	}

	// 链式等待页面就绪（也可用 tab.WaitReady(readyCtx)）
	if err := tab.Wait().Ready().Timeout(15 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待就绪超时: %v", err)
	}

	title, err := tab.Title(navCtx)
	if err != nil {
		log.Fatal(err)
	}
	curURL, err := tab.CurrentURL(navCtx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("标题: %s", title)
	log.Printf("地址: %s", curURL)

	// 截图保存到当前目录
	if err := tab.Screenshot(navCtx, "basic.png"); err != nil {
		log.Fatal(err)
	}
	log.Println("已截图: basic.png")
}
