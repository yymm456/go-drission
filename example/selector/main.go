// example/selector 演示四种元素定位方式：CSS / XPath / ID / JS 表达式。
//
// 定位与操作分成两步：tab.EleXxx(选择器) 拿到 Element，再对它做事。
// 如果更想显式声明定位方式（例如在配置里存选择器），也可以用
// tab.Ele(chromium.CSS("#id")) 这类构造器写法，二者完全等价。
//
//	go run ./example/selector
package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/yymm456/go-drission/chromium"
)

func main() {
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
	if err := tab.Navigate(navCtx, "https://example.com"); err != nil {
		log.Fatal(err)
	}
	if err := tab.Wait().Ready().Timeout(15 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待就绪超时: %v", err)
	}

	// 1) CSS 选择器
	if text, err := tab.EleCSS("h1").Text(navCtx); err == nil {
		log.Printf("CSS  h1            -> %s", text)
	}

	// 2) XPath 表达式
	if text, err := tab.EleXPath("//h1").Text(navCtx); err == nil {
		log.Printf("XPath //h1         -> %s", text)
	}

	// 3) 统计数量（Count 为 0 是正常返回，不是错误）
	if count, err := tab.EleCSS("p").Count(navCtx); err == nil {
		log.Printf("CSS  段落数量       -> %d", count)
	}

	// 4) JS 表达式：直接执行一段返回元素的脚本。
	//    最大用途是穿透 Shadow DOM——CSS / XPath 都够不着里面的节点。
	if text, err := tab.EleJS("document.querySelector('h1')").Text(navCtx); err == nil {
		log.Printf("JS   h1            -> %s", text)
	}

	// 5) 等价写法：用构造器显式声明定位方式，再交给 tab.Ele
	if text, err := tab.Ele(chromium.ID("")).Text(navCtx); err != nil {
		// 空选择器会在发请求前被拦下，可用 errors.Is 判断
		if errors.Is(err, chromium.ErrSelectorRequired) {
			log.Printf("空选择器被拦下: %v", err)
		}
	} else {
		log.Printf("ID 定位 -> %s", text)
	}

	// 链式等待同样走 Element
	if err := tab.EleCSS("h1").Wait().Visible().Timeout(10 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待 h1 可见失败: %v", err)
	}
	if err := tab.EleXPath("//p").Wait().Count(1).Timeout(10 * time.Second).Do(tab.Ctx); err != nil {
		log.Printf("等待段落出现失败: %v", err)
	}

	// 找不到元素时会返回带定位信息的错误，可用 errors.Is(err, chromium.ErrElementNotFound) 判断
	if err := tab.EleCSS("#这个元素不存在").ClickJS(navCtx); err != nil {
		log.Printf("预期中的定位失败: %v", err)
	}
}
