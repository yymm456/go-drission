package network

import (
	"strings"
)

// isBodyMethod 判断是否可能有请求体的 HTTP 方法
func isBodyMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH"
}

// matchURL 采用子串包含匹配：pattern 为空则全部命中，否则 URL 包含 pattern 即命中。
func (l *Listener) matchURL(u string) bool {
	if l.pattern == "" {
		return true
	}
	return strings.Contains(u, l.pattern)
}
