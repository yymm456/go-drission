// example/profile 演示「多进程浏览器隔离」：用 ProfileManager 启动多个独立 Chrome 实例，
// 每个命名档案拥有独立进程、独立用户数据目录与端口，Cookie/存储完全隔离，且登录态持久化到磁盘。
//
// 特点：进程重启后用同名档案打开即可恢复登录；崩溃互不影响；多档案可并发执行任务。
// 代价：每个账户一个完整 Chrome 进程，资源占用较高。轻量方案见 example/context。
//
//	go run ./example/profile
package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// 两个档案各自的任务站点（公开示例站点）
var tasks = []struct {
	profile string
	url     string
}{
	{"profile_a", "https://example.com"},
	{"profile_b", "https://example.org"},
}

func main() {
	ctx := context.Background()

	// baseDir：所有档案的用户数据根目录；basePort：起始端口，按打开顺序递增分配
	pm := chromium.NewProfileManager("./profiles", 9300,
		chromium.WithConnectTimeout(15*time.Second),
	)
	defer pm.CloseAll()

	// 逐个打开档案（各自启动一个独立 Chrome 进程）
	browsers := make([]*chromium.Browser, len(tasks))
	tabs := make([]*chromium.Tab, len(tasks))
	for i, t := range tasks {
		b, tab, err := pm.Open(ctx, t.profile)
		if err != nil {
			log.Fatal(err)
		}
		browsers[i], tabs[i] = b, tab
	}

	// 多进程证据：两个档案的 Chrome 主进程 PID 不同
	log.Printf("档案 %s PID=%d 端口=%d", tasks[0].profile, browsers[0].PID(), browsers[0].Port())
	log.Printf("档案 %s PID=%d 端口=%d", tasks[1].profile, browsers[1].PID(), browsers[1].Port())

	// 并发执行：每个浏览器一个 goroutine，导航 + 取标题，互不影响
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			tab := tabs[i]
			navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
			defer cancel()
			if err := tab.Navigate(navCtx, url); err != nil {
				log.Printf("%s 导航失败: %v", tasks[i].profile, err)
				return
			}
			title, _ := tab.Title(navCtx)
			log.Printf("%s 标题: %s", tasks[i].profile, title)
		}(i, t.url)
	}
	wg.Wait()

	// 同名再次 Open 直接复用已打开的浏览器（不会重复启动进程）
	b2, _, err := pm.Open(ctx, tasks[0].profile)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("同名复用 %s: 是否同一实例=%v", tasks[0].profile, b2 == browsers[0])

	log.Printf("已打开档案: %v", pm.Names())

	// 关闭单个档案：进程退出但磁盘数据保留，下次同名 Open 仍可恢复登录态
	pm.Close(tasks[1].profile)
	log.Printf("关闭 %s 后剩余档案: %v", tasks[1].profile, pm.Names())
}
