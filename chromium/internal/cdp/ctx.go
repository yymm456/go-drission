// Package cdp 收敛「发 CDP 命令时的上下文纪律」与 CDP 返回值解码这两类基础设施。
//
// 它不依赖本库任何其它包，可以被所有实现包引用；门面 chromium 不转发它，
// 因此使用者在外部看不到这些符号。
//
// 这里放的都是「跨包共用的规则」，而不是杂物间：每一条都有明确的历史缺陷作为背景
// （尤其是 RunAbandonable，见其注释）。
package cdp

import (
	"context"
	"time"
)

// RunAbandonable 在 runCtx 上执行 fn，同时允许调用方通过 watchCtx 提前「放弃等待」。
//
// 存在的意义：有些 CDP 调用（尤其是浏览器连接的首次 Run）必须作用在长生命周期的
// rootCtx 上——把连接绑到一个可取消的超时子 ctx 上，超时一到连接就被切断。
// 但我们又不希望调用方因此被无限期阻塞。于是让执行继续留在 runCtx 上跑，
// 调用方只是在 watchCtx 到期时放弃等待并返回错误：连接不受影响，超时也有交代。
//
// 代价是超时后会残留一个仍在执行的 goroutine，直到 fn 自行返回；
// 这类调用本身都有 CDP 层面的应答，不会真的悬挂。
func RunAbandonable(watchCtx, runCtx context.Context, fn func(context.Context) error) error {
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

// NoopCancel 供「没有派生新上下文」的分支返回，让调用方可以无条件 defer cancel()，
// 不必先判断自己走的是哪条路径。
func NoopCancel() {}

// WithDefaultTimeout 在 ctx 没有 deadline 时套用默认超时 d。
//
// 用于给「调用方没设超时」的 CDP 调用兜底，避免 Chrome 无响应时永久阻塞。
// 调用方已设 deadline 时原样返回（超时决策权始终归调用方）。
// d <= 0 表示不启用兜底。返回的 cancel 无论是否真的套了超时都必须调用。
func WithDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 || ctx == nil {
		return ctx, NoopCancel
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, NoopCancel
	}
	return context.WithTimeout(ctx, d)
}
