package chromium

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
)

// runAbandonable 在 runCtx 上执行 fn，同时允许调用方通过 watchCtx 提前「放弃等待」。
//
// 存在的意义：有些 CDP 调用（尤其是浏览器连接的首次 Run）必须作用在长生命周期的
// rootCtx 上——把连接绑到一个可取消的超时子 ctx 上，超时一到连接就被切断。
// 但我们又不希望调用方因此被无限期阻塞。于是让执行继续留在 runCtx 上跑，
// 调用方只是在 watchCtx 到期时放弃等待并返回错误：连接不受影响，超时也有交代。
//
// 代价是超时后会残留一个仍在执行的 goroutine，直到 fn 自行返回；
// 这类调用本身都有 CDP 层面的应答，不会真的悬挂。
func runAbandonable(watchCtx, runCtx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn(runCtx)
	}()
	select {
	case err := <-done:
		return err
	case <-watchCtx.Done():
		return watchCtx.Err()
	}
}

// noopCancel 供「没有派生新上下文」的分支返回，让调用方可以无条件 defer cancel()，
// 不必先判断自己走的是哪条路径。
func noopCancel() {}

// withDefaultTimeout 在 ctx 没有 deadline 时套用默认超时 d。
//
// 用于给「调用方没设超时」的 CDP 调用兜底，避免 Chrome 无响应时永久阻塞。
// 调用方已设 deadline 时原样返回（超时决策权始终归调用方）。
// d <= 0 表示不启用兜底。返回的 cancel 无论是否真的套了超时都必须调用。
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 || ctx == nil {
		return ctx, noopCancel
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, noopCancel
	}
	return context.WithTimeout(ctx, d)
}

// decodeRemoteValue 把 CDP Runtime.RemoteObject.Value 转成普通 Go 值。
//
// 坑点：cdproto 里 RemoteObject.Value 声明为 json.RawMessage（字节切片），
// 直接断言成 string 会失败、直接 fmt.Sprint 会拿到带引号的 JSON 文本
// （例如 "框架标题" 会变成 "\"\u6846\u67b6\u6807\u9898\""）。
// 必须显式按 JSON 解一次，才能与 chromedp.Evaluate 的返回值语义一致。
func decodeRemoteValue(v *runtime.RemoteObject) any {
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

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
