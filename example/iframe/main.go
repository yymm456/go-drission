// example/iframe 演示如何操作页面里的 iframe（框架）。
//
// 三种定位方式：按 <iframe> 元素的选择器定位、按框架 URL 子串、按 name。
// 定位到之后，框架内元素操作走 frame.EleCSS(...) 系列，形状与主页面的 tab.EleCSS(...) 一致。
//
//	go run ./example/iframe
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

// startSite 起一个本地站点：主页面里嵌一个 iframe，省去依赖外网。
func startSite() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>
<h1>主页面</h1>
<iframe id="inner" name="innerFrame" src="/frame" width="400" height="200"></iframe>
</body></html>`))
	})
	mux.HandleFunc("/frame", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>
<h2 id="title">我是框架里的标题</h2>
<input id="kw" value="">
<button id="go" onclick="document.getElementById('title').innerText='点过了'">点我</button>
</body></html>`))
	})
	return httptest.NewServer(mux)
}

func main() {
	site := startSite()
	defer site.Close()

	ctx := context.Background()

	browser, tab, err := chromium.OpenPage(ctx, 9222,
		chromium.WithConnectTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
	defer cancel()
	if err := tab.Navigate(navCtx, site.URL); err != nil {
		log.Fatal(err)
	}
	if err := tab.Wait().Ready().Timeout(15 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待就绪超时: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // 等 iframe 加载完成

	// 方式一：列出所有框架
	frames, err := tab.Frames(navCtx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("页面里共 %d 个 iframe", len(frames))

	// 方式二：按 <iframe> 元素的选择器定位（最精确，用元素与框架的 owner 关系匹配）
	frame, err := tab.Frame(navCtx, chromium.ID("inner"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("已定位框架: url=%s name=%s", frame.URL(), frame.Name())

	// 框架内读文本
	if title, err := frame.EleCSS("#title").Text(navCtx); err == nil {
		log.Printf("框架内标题: %s", title)
	}

	// 框架内输入
	if err := frame.EleCSS("#kw").SetValue(navCtx, "搜索词"); err != nil {
		log.Printf("框架内赋值失败: %v", err)
	}

	// 框架内点击
	if err := frame.EleCSS("#go").Click(navCtx); err != nil {
		log.Printf("框架内点击失败: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if title, err := frame.EleCSS("#title").Text(navCtx); err == nil {
		log.Printf("点击后标题: %s", title)
	}

	// 框架内取完整 HTML
	if html, err := frame.HTML(navCtx); err == nil {
		log.Printf("框架 HTML 长度: %d", len(html))
	}

	// 方式三：按 URL 子串 / 按 name 定位
	if f, err := tab.FrameByURL(navCtx, "/frame"); err == nil {
		log.Printf("按 URL 定位成功: %s", f.URL())
	}
	if f, err := tab.FrameByName(navCtx, "innerFrame"); err == nil {
		log.Printf("按 name 定位成功: %s", f.URL())
	}
}
