package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Response 是一次请求的结果。
//
// 响应体在构造时已读完并缓存，Text / JSON / Bytes 可任意多次调用；代价是整个响应体进内存。
// 抓大文件请改用 Client() 流式处理，或先用 WithMaxBodySize 设上限。嵌入的 *http.Response 的 Body 已读取并关闭。
type Response struct {
	*http.Response
	body []byte
}

// newResponse 读取并缓存响应体，同时关闭底层 Body（避免连接泄漏）。
//
// maxBody 是响应体上限（字节），<= 0 表示不限。判超限采用「读满上限后再探一个字节」：
// 只有正好读满时才额外读 1 字节，读得到就说明被截断。
//
// 不写成 io.LimitReader(body, maxBody+1)：maxBody 为 math.MaxInt64 时 +1 会回绕成负数，
// LimitReader 遇负数立即返回 EOF，正文被吞掉且不报错（BUG-08）。
func newResponse(resp *http.Response, maxBody int64) (*Response, error) {
	if resp == nil {
		return nil, ErrNilResponse
	}
	defer resp.Body.Close()

	var (
		body     []byte
		err      error
		tooLarge bool
	)
	if maxBody > 0 {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err == nil && int64(len(body)) == maxBody {
			var probe [1]byte
			n, perr := resp.Body.Read(probe[:])
			switch {
			case n > 0:
				tooLarge = true
			case perr != nil && !errors.Is(perr, io.EOF):
				err = perr
			}
		}
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}
	if tooLarge {
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

// SaveFile 把响应体写入文件（父目录自动创建），适合下载图片、附件。
//
// 目录 0750、文件 0640：响应体常含用户数据，不应让同机其他用户直接读到。
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
