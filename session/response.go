package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Response 是一次请求的结果。
//
// 响应体在构造时就已读完并缓存，因此 Text / JSON / Bytes 可以任意多次调用；
// 代价是整个响应体都进内存——抓大文件时请改用 Client() 拿底层 http.Client 自行流式处理，
// 或先用 WithMaxBodySize 设一个上限，避免对端一个「误配成 8G」的响应把进程撑爆。
// 嵌入的 *http.Response 里 Body 已被读取并关闭，不要再去读它。
type Response struct {
	*http.Response
	body []byte
}

// newResponse 读取并缓存响应体，同时关闭底层 Body（避免连接泄漏）。
//
// maxBody 是响应体上限（字节），<= 0 表示不限。判超限的方式是「多读一个字节」：
// 读到 maxBody+1 说明必然超限，此时立刻返回错误，不把剩余内容拉进内存。
func newResponse(resp *http.Response, maxBody int64) (*Response, error) {
	if resp == nil {
		return nil, ErrNilResponse
	}
	defer resp.Body.Close()

	var (
		body []byte
		err  error
	)
	if maxBody > 0 {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}
	if maxBody > 0 && int64(len(body)) > maxBody {
		return nil, fmt.Errorf("%w（超过 %d 字节，Content-Type %s）",
			ErrBodyTooLarge, maxBody, contentTypeOf(resp))
	}
	return &Response{Response: resp, body: body}, nil
}

// Bytes 返回原始响应体。
func (r *Response) Bytes() []byte {
	if r == nil {
		return nil
	}
	return r.body
}

// Text 按字符串返回响应体。
func (r *Response) Text() string {
	if r == nil {
		return ""
	}
	return string(r.body)
}

// JSON 把响应体解析到 v。响应体为空时返回 ErrEmptyBody，避免把空串当成合法 JSON 静默放过。
func (r *Response) JSON(v any) error {
	if r == nil {
		return ErrNilResponse
	}
	if len(bytes.TrimSpace(r.body)) == 0 {
		return fmt.Errorf("%w（状态码 %d，Content-Type %s）", ErrEmptyBody, r.StatusCode, r.ContentType())
	}
	if err := json.Unmarshal(r.body, v); err != nil {
		return fmt.Errorf("%w（状态码 %d）: %w", ErrInvalidJSON, r.StatusCode, err)
	}
	return nil
}

// OK 判断状态码是否为 2xx。
func (r *Response) OK() bool {
	return r != nil && r.StatusCode >= 200 && r.StatusCode < 300
}

// ContentType 返回响应头的 Content-Type（不含参数部分，如 "; charset=utf-8"）。
func (r *Response) ContentType() string {
	if r == nil {
		return ""
	}
	return contentTypeOf(r.Response)
}

// contentTypeOf 从原始响应里取出不含参数的 Content-Type。
// 抽成独立函数是因为 newResponse 在构造 Response 之前就要用它拼错误信息。
func contentTypeOf(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	ct := resp.Header.Get("Content-Type")
	if before, _, ok := strings.Cut(ct, ";"); ok {
		return strings.TrimSpace(before)
	}
	return ct
}

// SaveFile 把响应体写入文件（父目录自动创建），适合直接下载图片、附件。
//
// 目录 0750、文件 0640：响应体常含用户数据（订单、个人信息、接口返回的凭证），
// 不该按 0755 / 0644 让同机其他用户直接读到。
func (r *Response) SaveFile(path string) error {
	if r == nil {
		return ErrNilResponse
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	return os.WriteFile(path, r.body, 0o640)
}
