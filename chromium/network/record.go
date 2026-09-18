package network

import (
	"maps"
)

// Record 一条完整的请求/响应记录
type Record struct {
	RequestID string
	URL       string
	Method    string

	RequestHeaders map[string]string
	RequestBody    string

	Status          int64
	ResponseHeaders map[string]string
	ResponseBody    string
}

// clone 深拷贝一条记录。
//
// Records() 必须返回拷贝而不是内部指针：后台 goroutine（GetResponseBody /
// GetRequestPostData 的异步回填）会在调用方读取期间继续写 Status / ResponseBody，
// 直接暴露内部指针会构成数据竞争（go test -race 可稳定复现）。
// map 字段同样要复制，否则调用方与后台仍共享同一底层表。
func (r *Record) clone() *Record {
	if r == nil {
		return nil
	}
	c := &Record{
		RequestID:    r.RequestID,
		URL:          r.URL,
		Method:       r.Method,
		RequestBody:  r.RequestBody,
		Status:       r.Status,
		ResponseBody: r.ResponseBody,
	}
	if r.RequestHeaders != nil {
		c.RequestHeaders = make(map[string]string, len(r.RequestHeaders))
		maps.Copy(c.RequestHeaders, r.RequestHeaders)
	}
	if r.ResponseHeaders != nil {
		c.ResponseHeaders = make(map[string]string, len(r.ResponseHeaders))
		maps.Copy(c.ResponseHeaders, r.ResponseHeaders)
	}
	return c
}
