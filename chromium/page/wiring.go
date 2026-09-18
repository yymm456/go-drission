package page

import (
	"context"

	"github.com/chromedp/cdproto/target"
	"github.com/yymm456/go-drission/chromium/config"
)

// 本文件是「门面包 chromium」与「实现包 page」之间的接线面。
//
// 门面包还需要对标签页做四件只有 page 包内才能做的事：构造 Tab（它有三个私有字段）、
// 记录地址快照、释放 chromedp 会话、注入反检测脚本。Go 的包封装规则要求跨包访问
// 必须走导出符号，所以这里提供四个导出函数。
//
// 注意：它们是**内部接线口，不是对外 API**。门面 chromium/api.go 刻意不转发这四个名字，
// 因此包外拿不到它们 —— 这也正是「门面保留什么就等于对外契约什么」这条规则的用法。

// NewTabHandle 组装一个受 Browser 托管的 *Tab，统一 timeout / logger 的来源。
//
// 调用方随后按需 SetTabURL。集中在这里，避免「新增字段只在某个构造点补上」。
// ctx 放第一位，符合本库「与上下文相关的方法 ctx 在前」的约定（revive context-as-argument）。
func NewTabHandle(ctx context.Context, id target.ID, cancel context.CancelFunc, o *config.Options) *Tab {
	return &Tab{
		ID:      id,
		Ctx:     ctx,
		cancel:  cancel,
		timeout: o.DefaultTimeout,
		logger:  o.Logger,
	}
}

// SetTabURL 记录地址快照。
//
// 为什么不直接导出 Tab.setURL：setURL 必须在持有 mu 的前提下调用，
// 导出它就等于把这个约束暴露给包外，迟早有人绕过。这里给一个受控入口。
func SetTabURL(t *Tab, u string) { t.setURL(u) }

// ReleaseTab 释放标签页的 chromedp 会话（含 nil 判空）。
//
// 调用方原来各自写 `if t.cancel != nil { t.cancel() }`，共 6 处；
// 集中在这里可以少一处漏判 —— 漏判的后果是 chromedp 上下文泄漏，
// 而 -race 与常规用例都抓不到它。
func ReleaseTab(t *Tab) {
	if t.cancel != nil {
		t.cancel()
	}
}

// InjectAntiDetect 幂等地注入反检测初始化脚本；失败只告警，不影响标签页可用性。
//
// ctx 放第一位：这是**包级函数**不是方法（别名类型不能加方法），
// 按本库的约定与 revive 的 context-as-argument，自由函数的 ctx 必须在最前。
// 设计文档 §18.3 写的是 `(t *Tab, ctx, o)`，那是在它还被当成方法的假设下写的，
// 以可编译、过 lint 的签名为准。
func InjectAntiDetect(ctx context.Context, t *Tab, o *config.Options) error {
	return t.ensureAntiDetect(ctx, o)
}
