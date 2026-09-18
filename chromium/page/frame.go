package page

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/cdpkit"
	"github.com/yymm456/go-drission/chromium/errs"
)

// Frame 代表页面里的一个 iframe（框架）。
//
// 实现：先用 Page.getFrameTree 拿框架树，再用 DOM.getFrameOwner 把框架与 <iframe> 元素对上，
// 最后用 Page.createIsolatedWorld 在该框架建独立 JS 世界，后续操作都在其中用 Runtime.evaluate 完成。
//
// 这样即使跨域也能操作（脚本跑在目标框架的渲染进程里，不受同源策略限制），也无需把 iframe 当独立
// target 去 attach——后者只对跨进程 iframe(OOPIF) 有效，对同进程 iframe 反而拿不到 target。
//
// 生命周期：Frame 绑定在「某一次页面加载」上。顶层页面重新导航（或 iframe 换源）后，原 frameID 与
// isolated world 都会失效；此时按当初的定位依据（选择器 / URL / name）自动重新定位，仍失败才返回
// ErrFrameDetached。框架内元素操作走 FrameElement（由 Frame.Ele* 返回），均为 JS 语义。
type Frame struct {
	id      cdp.FrameID
	url     string
	name    string
	parent  *Tab
	worldID runtime.ExecutionContextID

	// locator 记录「当初是怎么找到这个框架的」，导航后据此重新定位同一个逻辑框架。
	locator frameLocator
}

// frameLocator 是框架的定位依据，三者按优先级依次尝试。
type frameLocator struct {
	selector Selector // 由 Tab.Frame(sel) 定位时记录
	hasSel   bool
	urlSub   string // 由 Tab.FrameByURL 定位时记录
	name     string // 由 Tab.FrameByName 定位时记录
}

// ID 返回该框架的 CDP 框架 ID。
func (f *Frame) ID() cdp.FrameID { return f.id }

// URL 返回该框架当前的地址（构造时的快照，导航后会变，请用 URLNow 重新获取）。
func (f *Frame) URL() string { return f.url }

// Name 返回 <iframe> 的 name 属性（没有则为空）。
func (f *Frame) Name() string { return f.name }

// ---------- 框架树 ----------

type frameInfo struct {
	ID       cdp.FrameID
	URL      string
	Name     string
	ParentID cdp.FrameID
}

func flattenFrames(tree *cdppage.FrameTree, out *[]frameInfo) {
	if tree == nil || tree.Frame == nil {
		return
	}
	*out = append(*out, frameInfo{
		ID:       tree.Frame.ID,
		URL:      tree.Frame.URL,
		Name:     tree.Frame.Name,
		ParentID: tree.Frame.ParentID,
	})
	for _, child := range tree.ChildFrames {
		flattenFrames(child, out)
	}
}

// Frames 返回当前页面里的全部 iframe（不含主框架）。
func (t *Tab) Frames(ctx context.Context) ([]*Frame, error) {
	infos, err := t.frameInfos(ctx)
	if err != nil {
		return nil, err
	}

	var out []*Frame
	for _, info := range infos {
		if info.ID == cdp.FrameID("") {
			continue
		}
		f, err := t.bindFrame(ctx, info, frameLocator{name: info.Name, urlSub: info.URL})
		if err != nil {
			// 单个框架绑定失败（例如正在销毁）不应影响其它框架
			t.log().Warn("绑定 iframe 失败", "url", info.URL, "err", err)
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		// 与 FrameByURL / FrameByName 一致：找不到就报哨兵错误，而非返回空切片让调用方去猜。
		return nil, errs.ErrFrameNotFound
	}
	return out, nil
}

// frameInfos 拉取并摊平框架树。
func (t *Tab) frameInfos(ctx context.Context) ([]frameInfo, error) {
	var tree *cdppage.FrameTree
	err := t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		var e error
		tree, e = cdppage.GetFrameTree().Do(c)
		return e
	}))
	if err != nil {
		return nil, fmt.Errorf("获取框架树失败: %w", err)
	}
	if tree == nil || tree.Frame == nil {
		return nil, nil
	}

	var all []frameInfo
	flattenFrames(tree, &all)
	if len(all) == 0 {
		return nil, nil
	}
	// 树根是主框架，iframe 才有意义
	return all[1:], nil
}

// loc 记录这次定位的依据，供页面导航后重新定位使用。
func (t *Tab) bindFrame(ctx context.Context, info frameInfo, loc frameLocator) (*Frame, error) {
	worldID, err := t.createIsolatedWorld(ctx, info.ID)
	if err != nil {
		return nil, err
	}

	return &Frame{
		id:      info.ID,
		url:     info.URL,
		name:    info.Name,
		parent:  t,
		worldID: worldID,
		locator: loc,
	}, nil
}

// Frame 按选择器定位页面里的 <iframe> 元素，返回对应框架对象。
//
// 定位原理：先取该元素的 nodeID，再对每个框架调 DOM.getFrameOwner 拿到「拥有该框架的 iframe
// 元素」的 nodeID，两者相等即命中。比 src URL 比对精确（about:blank 或多重嵌套时 URL 会重复）。
func (t *Tab) Frame(ctx context.Context, sel Selector) (*Frame, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	node, err := t.firstNode(ctx, sel)
	if err != nil {
		return nil, err
	}

	infos, err := t.frameInfos(ctx)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("%w: 当前页面没有 iframe", errs.ErrFrameNotFound)
	}

	var target *frameInfo
	err = t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		if e := dom.Enable().Do(c); e != nil {
			return e
		}
		for i := range infos {
			_, ownerID, e := dom.GetFrameOwner(infos[i].ID).Do(c)
			if e != nil {
				continue
			}
			if ownerID == node.NodeID {
				target = &infos[i]
				return nil
			}
		}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: 选择器 %s=%q 未指向任何 iframe", errs.ErrFrameNotFound, sel.Mode(), sel.String())
	}
	return t.bindFrame(ctx, *target, frameLocator{selector: sel, hasSel: true})
}

// findFrame 是「拉框架树 → 遍历找第一个匹配 → bindFrame」骨架的唯一实现。
//
// FrameByURL / FrameByName 只有判据（matcher）与未命中文案（errMsg）不同；骨架收在此处，
// 避免两处各写一遍错误处理导致漂移。
//
// Tab.Frame(sel) 不走这里：它多一步 DOM.getFrameOwner 与 nodeID 比对，判据不能退化成纯函数（见 Frame）。
func (t *Tab) findFrame(ctx context.Context, loc frameLocator, matcher func(frameInfo) bool, errMsg string) (*Frame, error) {
	infos, err := t.frameInfos(ctx)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if matcher(info) {
			return t.bindFrame(ctx, info, loc)
		}
	}
	return nil, fmt.Errorf("%w: %s", errs.ErrFrameNotFound, errMsg)
}

// FrameByURL 按 URL 子串查找框架，取第一个匹配的。
// iframe 没有 id/name 可依赖时（如广告位、第三方嵌入）通常用这种方式。
func (t *Tab) FrameByURL(ctx context.Context, substr string) (*Frame, error) {
	return t.findFrame(ctx,
		frameLocator{urlSub: substr},
		func(info frameInfo) bool { return strings.Contains(info.URL, substr) },
		fmt.Sprintf("没有 URL 包含 %q 的 iframe", substr))
}

// FrameByName 按 <iframe name="..."> 查找框架。
func (t *Tab) FrameByName(ctx context.Context, name string) (*Frame, error) {
	return t.findFrame(ctx,
		frameLocator{name: name},
		func(info frameInfo) bool { return info.Name == name },
		fmt.Sprintf("没有 name=%q 的 iframe", name))
}

// ---------- 框架内操作 ----------

// Eval 在框架内执行 JS 并返回结果。语义与 Tab.Eval 一致（按 JSON 解码返回值）。
func (f *Frame) Eval(ctx context.Context, js string) (any, error) {
	return f.eval(ctx, js)
}

// eval 在框架的 isolated world 内求值。
//
// 只对 world 失效重试：页面导航后旧 world 被销毁，重建一次是必要的；但脚本自身抛异常
// （exception != nil）绝不能重试——那说明脚本已执行并失败，重试会重复副作用（点击、提交、计数）。
func (f *Frame) eval(ctx context.Context, js string) (any, error) {
	var res any
	run := func(worldID runtime.ExecutionContextID) error {
		return f.parent.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			v, exception, err := runtime.Evaluate(js).
				WithContextID(worldID).
				WithReturnByValue(true).
				WithAwaitPromise(true).
				Do(c)
			if err != nil {
				return err
			}
			if exception != nil {
				// 包成 ErrFrameScript，供上层据此拒绝重试（理由见 eval 注释）。
				return fmt.Errorf("%w: %s", errs.ErrFrameScript, cdpkit.ExceptionText(exception))
			}
			res = cdpkit.DecodeRemoteValue(v)
			return nil
		}))
	}

	err := run(f.worldID)
	if err == nil {
		return res, nil
	}
	if errors.Is(err, errs.ErrFrameScript) {
		return nil, err // 脚本自身出错，重试无意义且会重复副作用
	}
	// world 失效情形一：框架还在，只是 execution context 被换了
	if newID, e := f.recreateWorld(ctx); e == nil {
		f.worldID = newID
		return res, run(f.worldID)
	}
	// 情形二：顶层页面重新导航，整个 iframe 被重建，老 frameID 已不存在；按当初定位依据重新找一次再建 world。
	if e := f.relocate(ctx); e == nil {
		return res, run(f.worldID)
	}
	return nil, fmt.Errorf("%w（原始错误：%w）", errs.ErrFrameDetached, err)
}

// relocate 按当初的定位依据重新定位框架，成功后把新框架的 id / url / worldID 吸收进来。
//
// 优先级：选择器 → URL 子串 → name → frameID（同文档内导航时 frameID 不变）。
// 全部失败返回 ErrFrameDetached。
func (f *Frame) relocate(ctx context.Context) error {
	if f.parent == nil {
		return errs.ErrFrameDetached
	}

	newFrame := func() (*Frame, error) {
		switch {
		case f.locator.hasSel:
			return f.parent.Frame(ctx, f.locator.selector)
		case f.locator.urlSub != "":
			return f.parent.FrameByURL(ctx, f.locator.urlSub)
		case f.locator.name != "":
			return f.parent.FrameByName(ctx, f.locator.name)
		}
		// 没有定位依据：在当前框架树里按 frameID 找（frame 本身没换源的情形）
		infos, err := f.parent.frameInfos(ctx)
		if err != nil {
			return nil, err
		}
		for _, info := range infos {
			if info.ID == f.id {
				return f.parent.bindFrame(ctx, info, f.locator)
			}
		}
		return nil, fmt.Errorf("%w: frameID=%s 已不在框架树中", errs.ErrFrameNotFound, f.id)
	}

	nf, err := newFrame()
	if err != nil {
		return err
	}
	f.id = nf.id
	f.url = nf.url
	f.name = nf.name
	f.worldID = nf.worldID
	return nil
}

// recreateWorld 为该框架重建 isolated world，返回新的 contextID。
func (f *Frame) recreateWorld(ctx context.Context) (runtime.ExecutionContextID, error) {
	return f.parent.createIsolatedWorld(ctx, f.id)
}

// createIsolatedWorld 在指定框架里建立一个 isolated world，返回其 execution context ID。
// bindFrame 与 recreateWorld 共用同一段 CDP 调用（同一个操作的两种时机）。
func (t *Tab) createIsolatedWorld(ctx context.Context, id cdp.FrameID) (runtime.ExecutionContextID, error) {
	var worldID runtime.ExecutionContextID
	err := t.run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		res, e := cdppage.CreateIsolatedWorld(id).
			WithGrantUniveralAccess(true).
			Do(c)
		if e != nil {
			return e
		}
		worldID = res
		return nil
	}))
	return worldID, err
}

// evalString 在框架内求值并返回字符串结果。
func (f *Frame) evalString(ctx context.Context, js string) (string, error) {
	v, err := f.eval(ctx, js)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return fmt.Sprint(v), nil
}

// URLNow 返回框架当前的实时地址（与构造时的 URL 快照区分开）。
func (f *Frame) URLNow(ctx context.Context) (string, error) {
	return f.evalString(ctx, "location.href")
}

// HTML 返回框架内完整的 HTML。
func (f *Frame) HTML(ctx context.Context) (string, error) {
	return f.evalString(ctx, "document.documentElement.outerHTML")
}

// requireExpr 校验选择器并生成框架内可用的 JS 表达式。
func (f *Frame) requireExpr(sel Selector) (string, error) {
	if err := sel.validate(); err != nil {
		return "", err
	}
	expr := sel.jsExpr()
	if expr == "" {
		return "", fmt.Errorf("%w: %s=%q", errs.ErrSelectorRequired, sel.Mode(), sel.String())
	}
	return expr, nil
}

// frameOpResult 把框架内脚本的返回值翻译成 Go 错误：
// 脚本约定返回 'ok' 或 'not found'，只有 'ok' 才算成功。
func frameOpResult(v any, action string, sel Selector) error {
	if s, ok := v.(string); ok && s == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %s %s=%q（框架内脚本返回 %v）",
		errs.ErrElementNotFound, action, sel.Mode(), sel.String(), v)
}

// Navigate 让框架跳转到指定地址。
func (f *Frame) Navigate(ctx context.Context, url string) error {
	_, err := f.eval(ctx, fmt.Sprintf("location.href = %q", url))
	return err
}
