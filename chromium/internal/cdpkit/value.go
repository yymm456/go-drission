package cdpkit

import (
	"encoding/json"
	"reflect"

	"github.com/chromedp/cdproto/runtime"
)

// DecodeRemoteValue 把 CDP Runtime.RemoteObject.Value 转成普通 Go 值。
//
// 坑点：上游 cdproto/runtime 里 RemoteObject.Value 声明为 json.RawMessage（字节切片），
// 直接断言成 string 会失败、直接 fmt.Sprint 会拿到带引号的 JSON 文本
// （例如 "框架标题" 会变成 "\"\u6846\u67b6\u6807\u9898\""）。
// 必须显式按 JSON 解一次，才能与 chromedp.Evaluate 的返回值语义一致。
func DecodeRemoteValue(v *runtime.RemoteObject) any {
	if v == nil || v.Value == nil {
		return nil
	}

	// 用反射兜住 []byte / json.RawMessage / easyjson.RawMessage 这几种等价形态
	rv := reflect.ValueOf(v.Value)
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		raw := rv.Bytes()
		if len(raw) == 0 {
			return nil
		}
		var out any
		if err := json.Unmarshal(raw, &out); err == nil {
			return out
		}
		// 不是合法 JSON（理论上不会），退回原始文本，至少不会丢内容
		return string(raw)
	}
	return v.Value
}

// ExceptionText 把 CDP 异常详情转成可读字符串。
func ExceptionText(e *runtime.ExceptionDetails) string {
	if e == nil {
		return ""
	}
	if e.Exception != nil && e.Exception.Description != "" {
		return e.Exception.Description
	}
	if e.Text != "" {
		return e.Text
	}
	return "未知异常"
}
