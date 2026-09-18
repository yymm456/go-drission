// Package chromium 是一个基于 CDP（Chrome DevTools Protocol）的浏览器自动化库，
// 底层使用 chromedp 建立连接与执行命令。
//
// # 分层
//
// 本包是**门面**：它不含任何实现，只把 internal 各个包里已经写好的东西
// 按原名转发出去（类型别名 + 包级函数转发）。实现按职责分成 7 个内部包，
// 依赖只允许单向：
//
//	chromium（门面）
//	  ├── browser   Browser / BrowserContext / 标签页生命周期
//	  │               └─ 依赖 page / chrome / cdpkit / config / errs
//	  ├── profile   Profile / ProfileManager（多账户命名档案）
//	  │               └─ 依赖 browser / page / config / errs
//	  ├── page      Tab / Element / Frame / FrameElement / Selector / WaitBuilder
//	  │               └─ 依赖 cookie / network / cdpkit / errs
//	  ├── network   Listener / Record（网络被动监听）
//	  ├── cookie    Cookie 的纯逻辑（校验 / 解析 / 参数构造 / 存盘）
//	  ├── chrome    启动、探测、端口、跨进程锁、浏览器路径、档案标记
//	  ├── config    Option 与默认配置
//	  ├── cdpkit    CDP 值与上下文的通用助手
//	  └── errs      全部哨兵错误
//
// 用户只需要 import 本包；`internal/` 由 Go 语言规则保证外部不可见，
// 因此内部怎么拆都不会影响调用方。
//
// # ctx 约定（最重要的一条）
//
// 所有 I/O 方法都要求调用方传入 ctx，并且这个 ctx **必须从 tab.Ctx 派生**，
// 否则命令无法路由到目标标签页（返回 ErrInvalidContext）。
// 超时与取消一律由调用方掌握：
//
//	ctx, cancel := context.WithTimeout(tab.Ctx, 10*time.Second)
//	defer cancel()
//	tab.Navigate(ctx, "https://example.com")
//
// 唯一的例外是监听器（Listener）：它自己持有会话，用 WaitIdle 配合外部 ctx 收敛。
// 详见 Tab.Listen 与 network 包的说明。
//
// # 定位与操作分离
//
// 定位类 API 只有一套，且只返回句柄、不做操作：
//
//	tab.Ele(chromium.CSS("#login"))        // *Element
//	ele.Click(ctx)                         // 操作在句柄上
//
// 不提供 `Tab.Click(ctx, sel)` 这种「操作 + 选择器」一步到位的写法 ——
// 那会让「定位」与「操作」的失败原因混在一起，也无法复用已定位的句柄。
//
// # 两个库，互不 import
//
// 本仓库还有 session 包做纯 HTTP 模式。两个包**不互相 import**，
// 唯一的桥是 CookieSource 接口：只要能 `ExportJSON() ([]byte, error)`
// 就能把登录态从一侧搬到另一侧（见 example/cookie_relay）。
package chromium
