// Package errs 是本库全部哨兵错误的唯一定义处。
//
// 错误值被每一个实现包（browser / page / network / cookie / profile / chrome）引用，放在任何上层包里
// 都会形成环，因此压到最底层，由门面 chromium 按原名转发，errors.Is 判断与错误文案不受影响。
package errs

import "errors"

// 本库对外暴露的固定错误，便于调用方用 errors.Is 判断，而不是比对错误字符串。
// 需要携带动态信息（如索引、名字）的错误仍用 fmt.Errorf 包装这些哨兵值。
var (
	// ErrClosed 表示 Browser 已经关闭，不再接受任何操作。
	ErrClosed = errors.New("chromium: 浏览器连接已关闭")

	// ErrNotConnected 表示 Browser 尚未 Connect，rootCtx 还不存在。
	// 常见于 NewBrowser 之后直接调用 NewTab / Context 而漏了 Connect。
	ErrNotConnected = errors.New("chromium: 浏览器尚未连接，请先调用 Connect 或 OpenPage")

	// ErrInvalidContext 表示传入的 ctx 不含会话路由信息。
	// 传给 Tab / Listener 的 ctx 必须派生自 tab.Ctx，否则命令落不到目标标签页上，
	// 底层 chromedp 会直接 panic；这里提前拦住并转成可读的错误。
	ErrInvalidContext = errors.New("chromium: ctx 必须从 tab.Ctx 派生，否则命令无法路由到目标标签页")

	// ErrAlreadyConnected 表示 Browser 已经连接过，重复 Connect 被拒绝。
	// 重复连接会覆盖并泄漏上一条 allocator 与浏览器连接，因此显式报错。
	ErrAlreadyConnected = errors.New("chromium: 浏览器已连接，请勿重复 Connect")

	// ErrNoTab 表示当前没有任何可返回的标签页。
	ErrNoTab = errors.New("chromium: 当前没有标签页")

	// ErrContextClosed 表示指定的隔离上下文已经销毁。
	ErrContextClosed = errors.New("chromium: 隔离上下文已关闭")

	// ErrProfileClosed 表示 ProfileManager 已 CloseAll，无法再打开档案。
	ErrProfileClosed = errors.New("chromium: ProfileManager 已关闭")

	// ErrListenerStarted 表示监听器已启动，重复 Start 被拒绝。
	ErrListenerStarted = errors.New("chromium: 监听器已启动")

	// ErrWaitConditionUnset 表示 WaitBuilder 未指定等待条件。
	ErrWaitConditionUnset = errors.New("chromium: 未指定等待条件（Visible/Present/Text/Count/URL/Ready 之一）")

	// ErrElementNotFound 表示选择器没有匹配到任何元素。
	ErrElementNotFound = errors.New("chromium: 没有匹配到元素")

	// ErrFrameNotFound 表示没有找到符合条件的 iframe。
	ErrFrameNotFound = errors.New("chromium: 没有找到 iframe")

	// ErrFrameDetached 表示 Frame 已随着页面导航 / iframe 重建而失效，
	// 且按最初的定位依据也无法重新找到它。调用方应重新用 Tab.Frame /
	// FrameByURL / FrameByName 取一个新的 Frame 对象。
	ErrFrameDetached = errors.New("chromium: iframe 已随页面导航失效")

	// ErrSelectorRequired 表示该操作需要一个选择器 / 元素，但没有提供。
	// 例如 tab.EleCSS("") 或对空选择器构造的 Element 调 Click。
	ErrSelectorRequired = errors.New("chromium: 该操作需要一个非空的选择器")

	// ErrChromeNotFound 表示未在常见位置找到可用的浏览器可执行文件。
	ErrChromeNotFound = errors.New("chromium: 未找到可用的浏览器可执行文件")

	// ErrBrowserMismatch 表示端口上确实有一个 Chrome，但它不是调用方期望的那一个。
	//
	// 典型场景：显式指定了 WithUserDataDir("./profiles/a") 但端口上已有别的 Chrome 在跑。
	// 若直接接管，实际用的是别人的浏览器（别的用户数据目录与登录态），Cookie 隔离与多账号方案
	// 会静默失效。确认无误请加 WithTrustExistingBrowser()。
	ErrBrowserMismatch = errors.New("chromium: 端口上的浏览器与期望的用户数据目录不一致")

	// ErrInvalidCookie 表示待注入的 Cookie 参数不合法。
	// 包括：缺 Name / 缺 Domain / SameSite=None 却没有 Secure。
	// CDP 在这些情况下只会回报一句 "Invalid cookie fields"，
	// 提前拦下能直接指出是哪一条、缺了什么。
	ErrInvalidCookie = errors.New("chromium: Cookie 参数不合法")

	// ErrInvalidCookieJSON 表示 Cookie JSON 不是合法的数组结构。
	ErrInvalidCookieJSON = errors.New("chromium: 解析 Cookie JSON 失败")

	// ErrEmptyURL 表示传入的地址为空或缺少协议头 / 主机名。
	// LoginWithCookies 这类「先注入再打开」的接口需要可用的绝对地址。
	ErrEmptyURL = errors.New("chromium: 地址为空或格式非法")
)

// ErrFrameScript 表示「iframe 内的脚本自身抛出异常」。
//
// 内部使用，门面不转发：Frame.eval 依赖它区分「脚本报错」与「world 失效」——前者绝不重试
// （重试会重复点击、提交等副作用），后者必须重试（导航后旧 execution context 会被销毁）。
var ErrFrameScript = errors.New("iframe 内执行 JS 失败")
