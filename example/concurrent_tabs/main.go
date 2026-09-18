// example/concurrent_tabs 演示「同一个浏览器内多标签页并发执行任务」：
// 同一窗口开三个标签页，各自在独立 goroutine 里并发访问不同站点、互不干扰
// （等价于多线程各跑各的任务），任务完成后按顺序切换前台并截图。
//
// 并发模型要点：
//
//   - 每个 Tab 持有独立的 chromedp 会话（CDP target 路由），goroutine 并行操作各自标签页无需加锁；
//
//   - 操作 ctx 从各自 tab.Ctx 派生，一个 goroutine 一个，不跨标签共享；
//
//   - 导航 / DOM 读写 / 取标题在后台标签不受影响；截图在切到前台后进行（后台截图可能空白）。
//
//     go run ./example/concurrent_tabs
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// 三个并发任务：标签名 + 目标站点（均为公开示例站点）
var sites = []struct {
	name string
	url  string
}{
	{"A(example.com)", "https://example.com"},
	{"B(example.org)", "https://example.org"},
	{"C(example.net)", "https://example.net"},
}

func main() {
	ctx := context.Background()
	if err := os.MkdirAll("screenshots", 0o755); err != nil {
		log.Fatal(err)
	}

	// 一个浏览器进程、一个窗口；标签A 复用首个标签页，B/C 新建
	browser, tabA, err := chromium.OpenPage(ctx, 9240,
		chromium.WithConnectTimeout(15*time.Second),
		chromium.WithUserDataDir("./profiles/concurrent-tabs"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	defer cancelOpen()
	tabs := []*chromium.Tab{tabA}
	for i := 1; i < len(sites); i++ {
		tab, err := browser.NewTab(openCtx)
		if err != nil {
			log.Fatal(err)
		}
		tabs = append(tabs, tab)
	}

	// 并发执行：每个标签一个 goroutine，只操作自己的标签页
	var wg sync.WaitGroup
	results := make([]string, len(sites))
	for i := range sites {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tab := tabs[i]
			taskCtx, cancel := context.WithTimeout(tab.Ctx, 60*time.Second)
			defer cancel()

			if err := tab.Navigate(taskCtx, sites[i].url); err != nil {
				results[i] = fmt.Sprintf("%s: 导航失败 %v", sites[i].name, err)
				return
			}
			_ = tab.Wait().Ready().Timeout(15 * time.Second).Do(taskCtx)
			title, _ := tab.Title(taskCtx)
			nodes, _ := tab.EleCSS("*").Count(taskCtx)
			u, _ := tab.CurrentURL(taskCtx)
			results[i] = fmt.Sprintf("%s: 标题=%q DOM节点=%d 地址=%s", sites[i].name, title, nodes, u)
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		log.Println(r)
	}

	// 标签切换：按 A→B→C 顺序置前并各截一张图
	for i, tab := range tabs {
		swCtx, cancel := context.WithTimeout(tab.Ctx, 15*time.Second)
		if err := tab.BringToFront(swCtx); err != nil {
			log.Printf("切换到标签 %s 失败: %v", sites[i].name, err)
			cancel()
			continue
		}
		time.Sleep(500 * time.Millisecond)
		shot := filepath.Join("screenshots", fmt.Sprintf("concurrent_tab_%d.png", i+1))
		if err := tab.Screenshot(swCtx, shot); err == nil {
			log.Printf("已切到标签 %s 并截图: %s", sites[i].name, shot)
		}
		cancel()
	}
	log.Println("三个标签页在同一浏览器内并发执行、互不干扰，切换正常")
}
