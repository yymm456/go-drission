package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// noProxyTransport 是绕过系统代理的 HTTP transport。
//
// 调试端口与 /json 端点都跑在本机回环地址上，必须直连：一旦走了 HTTP_PROXY
// 环境变量指向的代理，探测请求会被转发到外部代理，导致「明明活着却探测失败」，
// 进而每次都去重新启动一个 Chrome。
var noProxyTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	ResponseHeaderTimeout: 3 * time.Second,
}

// IsPortAlive 判断指定端口上是否有一个可用的 Chrome DevTools 端点。
// 探测过程受 ctx 约束：ctx 取消或超时立即返回 false，不会拖慢调用方。
func IsPortAlive(ctx context.Context, port int) bool {
	if err := ctx.Err(); err != nil {
		return false
	}

	// 1. 快速 TCP 探测
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(probeCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	// 探测用的连接，关闭失败无补救手段，也没必要上报
	_ = conn.Close()

	// 2. 确认是 Chrome DevTools 端点，而非其他进程恰好占用端口
	client := &http.Client{Timeout: 3 * time.Second, Transport: noProxyTransport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false
	}
	return v.Browser != ""
}
