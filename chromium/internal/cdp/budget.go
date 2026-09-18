package cdp

import (
	"context"
	"time"
)

// DefaultCallTimeout 是 CDP 调用在调用方 ctx 未设 deadline 时采用的默认超时，
// 防止 Chrome 无响应导致永久阻塞。
//
// 覆盖面：browser 级调用（创建/销毁隔离上下文、上下文内新建标签页、关闭 target、
// 注入脚本等）与建立标签页会话时的「等待预算」。
const DefaultCallTimeout = 30 * time.Second

// BudgetDuration 是「本轮操作最多等多久」的唯一算法：调用方 ctx 带 deadline 时取
// min(DefaultCallTimeout, 剩余时间)，否则套用 DefaultCallTimeout。
//
// **两个入口共用同一条超时策略**：browser 的 boundedRootCtx（browser 级 CDP 调用）
// 与 tabInitBudget（建立标签页会话的等待预算）。这段计算此前在两处各写一遍，
// 正是「改一处漏一处」的高发区——BUG-07 就是这类漂移的产物（见 tabInitBudget 的说明）。
//
// 只抽「时长」而不抽「上下文」：两个调用方要派生的上下文本来就不同——
// boundedRootCtx 从 rootCtx 派生、并补上调用方取消的传导边；tabInitBudget 直接挂在
// 调用方 ctx 上——但「等多久」这一件事必须完全一致。
func BudgetDuration(ctx context.Context) time.Duration {
	d := DefaultCallTimeout
	if dl, ok := ctx.Deadline(); ok {
		if until := time.Until(dl); until < d {
			d = until
		}
	}
	return d
}
