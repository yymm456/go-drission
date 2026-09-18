// example/session 演示纯 HTTP 会话：发请求、带 Cookie、把登录态存盘复用。
//
// 典型组合拳：用浏览器登录一次 → 导出 Cookie → Session 载入后批量抓接口，
// 既有登录态又没有浏览器开销。本例不依赖浏览器，可直接运行。
//
//	go run ./example/session
package main

import (
	"context"
	"log"
	"time"

	"github.com/yymm456/go-drission/session"
)

func main() {
	ctx := context.Background()

	// 1) 建一个会话：自带 Cookie 容器、30s 超时、Chrome UA
	s := session.New(
		session.WithTimeout(10*time.Second),
		session.WithHeader("Accept-Language", "zh-CN,zh"),
	)

	// 2) 普通 GET
	resp, err := s.Get(ctx, "https://httpbin.org/get")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("GET 状态=%d 类型=%s 长度=%d", resp.StatusCode, resp.ContentType(), len(resp.Bytes()))

	var payload struct {
		Headers map[string]string `json:"headers"`
		Origin  string            `json:"origin"`
	}
	if err := resp.JSON(&payload); err != nil {
		log.Printf("解析 JSON 失败: %v", err)
	} else {
		log.Printf("出口 IP=%s", payload.Origin)
	}

	// 3) POST JSON
	resp2, err := s.PostJSON(ctx, "https://httpbin.org/post", map[string]any{"name": "go-drission"})
	if err != nil {
		log.Printf("POST 失败: %v", err)
	} else {
		var out struct {
			JSON map[string]any `json:"json"`
		}
		if err := resp2.JSON(&out); err == nil {
			log.Printf("POST 回显=%v", out.JSON)
		}
	}

	// 4) 表单提交
	if _, err := s.PostForm(ctx, "https://httpbin.org/post", map[string][]string{"k": {"v"}}); err != nil {
		log.Printf("表单提交失败: %v", err)
	}

	// 5) Cookie 存盘 —— 登录态可以留给下次或交给另一个 Session
	log.Printf("当前 Cookie 数=%d", len(s.Cookies()))
	if err := s.SaveCookies("cookies.json"); err != nil {
		log.Printf("保存 Cookie 失败: %v", err)
	} else {
		log.Println("已保存 Cookie 到 cookies.json")
	}

	// 6) 新会话复用登录态
	s2 := session.New()
	if err := s2.LoadCookies("cookies.json"); err != nil {
		log.Printf("载入 Cookie 失败: %v", err)
	} else {
		log.Printf("新会话载入后 Cookie 数=%d", len(s2.Cookies()))
	}

	// 7) 手动塞一条 Cookie（例如从浏览器那边搬过来的）
	s2.SetCookie(session.CookieItem{
		Name:   "token",
		Value:  "abc123",
		Domain: "httpbin.org",
		Path:   "/",
	})
	log.Printf("手动写入后 Cookie 数=%d", len(s2.Cookies()))
}
