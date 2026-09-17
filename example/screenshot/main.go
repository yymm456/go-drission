// example/screenshot 演示页面截图。
//
// tab.Screenshot 基于 CDP CaptureScreenshot，捕获的是当前视口。
// 通过 WithWindowSize 可控制视口尺寸，从而控制截图大小。
//
//	go run ./example/screenshot
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func main() {
	ctx := context.Background()

	browser, tab, err := chromium.OpenPage(ctx, 9222,
		chromium.WithConnectTimeout(5*time.Second),
		chromium.WithWindowSize("1280,800"), // 视口尺寸决定截图尺寸
	)
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

	// 视口截图，保存到 screenshots 目录（不存在则创建）
	if err := os.MkdirAll("screenshots", 0o755); err != nil {
		log.Fatal(err)
	}
	out := filepath.Join("screenshots", "page.png")
	if err := tab.Screenshot(navCtx, out); err != nil {
		log.Fatal(err)
	}
	log.Printf("已保存视口截图: %s", out)

	// 读取页面尺寸，便于确认截图范围
	if w, err := tab.Eval(navCtx, "window.innerWidth"); err == nil {
		if h, err2 := tab.Eval(navCtx, "window.innerHeight"); err2 == nil {
			log.Printf("视口尺寸: %v x %v", w, h)
		}
	}
}
