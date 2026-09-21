package chromium

// 本文件是门面：把各子包的公开符号按原名转发出去（类型别名 + 包级函数转发）。
//
// 用类型别名而非包装类型：别名（type Option = config.Option）是同一个类型，不做装箱与转换，
// 调用方原有写法、传参、接口实现关系均不变；转发函数保证文档注释只有一个出处。
//
// 约定：实现内部一律直接用子包里的名字（errs.ErrClosed / config.Defaults），本文件只承担对外契约。

import (
	"context"
	"log/slog"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/yymm456/go-drission/chromium/browser"
	"github.com/yymm456/go-drission/chromium/chrome"
	"github.com/yymm456/go-drission/chromium/config"
	"github.com/yymm456/go-drission/chromium/cookie"
	"github.com/yymm456/go-drission/chromium/errs"
	"github.com/yymm456/go-drission/chromium/network"
	"github.com/yymm456/go-drission/chromium/page"
	"github.com/yymm456/go-drission/chromium/profile"
)

// Option 是函数式配置项
type Option = config.Option

// Cookie 描述一个要注入的 Cookie。定义见 cookie，别名与它指向同一类型。
//
// 字段名 / 类型 / JSON tag 不会出现在 go doc 里（别名化所致），改由 api_test.go 的
// TestCookieJSONTagsAreFrozen 在编译运行期钉住。
type Cookie = cookie.Cookie

// CookieSource 是「能导出自己的 Cookie JSON」的类型。
//
// session.Jar 与 *session.Jar 天然满足该接口，因此可从浏览器侧接力，而 chromium 包无需反向依赖 session 包：
//
//	s := session.New()
//	s.PostForm(ctx, loginURL, form)             // 纯 HTTP 登录，拿到 Cookie
//	tab.LoginWithCookies(ctx, homeURL, s.Jar()) // 把登录态交给浏览器
type CookieSource = cookie.CookieSource

// Record 一条完整的请求/响应记录。
//
// 定义见 network；Records() 返回的是它的深拷贝。
type Record = network.Record

// Listener 使用 Network 域被动监听网络请求。
//
// 定义见 network。模式为空表示全部命中；Start(ctx) 的 ctx 必须派生自 tab.Ctx。
type Listener = network.Listener

// 对外暴露的哨兵错误；与 errs 里的是同一个值，因此 errors.Is 在「内部包返回、外部包判断」之间照旧成立。
var (
	// ErrClosed 表示 Browser 已经关闭，不再接受任何操作。
	ErrClosed = errs.ErrClosed

	// ErrNotConnected 表示 Browser 尚未 Connect，常见于漏了 Connect 就直接用 NewTab / Context。
	ErrNotConnected = errs.ErrNotConnected

	// ErrInvalidContext 表示传入的 ctx 不是从 tab.Ctx 派生的，命令无法路由到目标标签页。
	ErrInvalidContext = errs.ErrInvalidContext

	// ErrAlreadyConnected 表示 Browser 已经连接过，重复 Connect 被拒绝。
	ErrAlreadyConnected = errs.ErrAlreadyConnected

	// ErrNoTab 表示当前没有任何可返回的标签页。
	ErrNoTab = errs.ErrNoTab

	// ErrContextClosed 表示指定的隔离上下文已经销毁。
	ErrContextClosed = errs.ErrContextClosed

	// ErrProfileClosed 表示 ProfileManager 已 CloseAll，无法再打开档案。
	ErrProfileClosed = errs.ErrProfileClosed

	// ErrListenerStarted 表示监听器已启动，重复 Start 被拒绝。
	ErrListenerStarted = errs.ErrListenerStarted

	// ErrWaitConditionUnset 表示 WaitBuilder 未指定等待条件。
	ErrWaitConditionUnset = errs.ErrWaitConditionUnset

	// ErrElementNotFound 表示选择器没有匹配到任何元素。
	ErrElementNotFound = errs.ErrElementNotFound

	// ErrFrameNotFound 表示没有找到符合条件的 iframe。
	ErrFrameNotFound = errs.ErrFrameNotFound

	// ErrFrameDetached 表示 Frame 已随页面导航失效且无法重新定位，应重新取一个新的 Frame（详见 errs）。
	ErrFrameDetached = errs.ErrFrameDetached

	// ErrSelectorRequired 表示该操作需要一个非空的选择器 / 元素。
	ErrSelectorRequired = errs.ErrSelectorRequired

	// ErrChromeNotFound 表示未在常见位置找到可用的浏览器可执行文件。
	ErrChromeNotFound = errs.ErrChromeNotFound

	// ErrBrowserMismatch 表示端口上的 Chrome 不是调用方期望的那一个（多账号隔离为何必须拦，详见 errs）。
	// 确认「就是它」请加 WithTrustExistingBrowser()。
	ErrBrowserMismatch = errs.ErrBrowserMismatch

	// ErrInvalidCookie 表示待注入的 Cookie 参数不合法（细目见 errs 与 cookie.ValidateCookie）。
	ErrInvalidCookie = errs.ErrInvalidCookie

	// ErrInvalidCookieJSON 表示 Cookie JSON 不是合法的数组结构。
	ErrInvalidCookieJSON = errs.ErrInvalidCookieJSON

	// ErrEmptyURL 表示传入的地址为空或缺少协议头 / 主机名。
	ErrEmptyURL = errs.ErrEmptyURL

	// ErrNoHistoryEntry 表示已经到达历史记录边界（没有更早或更晚的一条可去）。
	// Back / Forward 在越界时返回它，页面保持原样。用 errors.Is 判断。
	ErrNoHistoryEntry = errs.ErrNoHistoryEntry

	// ErrInvalidWindowState 表示 SetWindowState 的窗口状态不是
	// normal / minimized / maximized / fullscreen 之一。
	ErrInvalidWindowState = errs.ErrInvalidWindowState
)

// WithChromePath 指定浏览器可执行文件路径（支持 Chrome / Edge / Brave / Chromium）。
// 不指定时会自动在本机常见位置查找。一旦显式指定，路径不存在将直接报错，不会静默回退。
func WithChromePath(path string) Option { return config.WithChromePath(path) }

// WithUserDataDir 指定用户数据目录（持久化登录态）。
//
// 显式指定后，连接阶段会核实端口上已有的 Chrome 是否确实在用这个目录（见 chrome.VerifyPortOwner）；
// 核实不了返回 ErrBrowserMismatch，而非静默接管别人的浏览器。确认无误可用 WithTrustExistingBrowser 跳过。
func WithUserDataDir(dir string) Option { return config.WithUserDataDir(dir) }

// WithTrustExistingBrowser 跳过「端口上已有的 Chrome 是不是目标」这项核实。
//
// 适用于手动启动的 Chrome（无本库写入的档案标记）且确定它就是目标的情况。默认关闭。
func WithTrustExistingBrowser() Option { return config.WithTrustExistingBrowser() }

// WithUserAgent 指定 User-Agent
func WithUserAgent(ua string) Option { return config.WithUserAgent(ua) }

// WithProxy 指定代理，例如 "http://127.0.0.1:7890"
func WithProxy(proxy string) Option { return config.WithProxy(proxy) }

// WithHeadless 是否启用无头模式
func WithHeadless(headless bool) Option { return config.WithHeadless(headless) }

// WithLang 设置浏览器语言（--lang），例如 "zh-CN"、"en-US"。
// 影响请求头 Accept-Language 与 navigator.language。传空字符串则不添加该参数。
func WithLang(lang string) Option { return config.WithLang(lang) }

// WithAntiDetect 是否启用反自动化检测（默认关闭）。
//
// 开启后追加 --disable-blink-features=AutomationControlled 等启动参数，并在每个新建标签页注入
// 初始化脚本把 navigator.webdriver 抹成 undefined。这些处理会改变浏览器指纹特征，
// 因此默认不启用 —— 需要时显式传 true。
func WithAntiDetect(enable bool) Option { return config.WithAntiDetect(enable) }

// WithInsecureTLS 跳过 TLS 证书校验，用于自签证书的内网站点（默认关闭）。
//
// 与 session.WithInsecureTLS 同名同义：那一侧改的是 http.Transport 的 TLSClientConfig，
// 这一侧是给 Chrome 追加 --ignore-certificate-errors 与 --test-type。
//
// 开启后浏览器不再拦任何证书错误 —— 公网使用等于放弃中间人防护，
// 只在明确信任的网络里开。
func WithInsecureTLS() Option { return config.WithInsecureTLS() }

// WithDefaultTimeout 设置 Tab 操作的默认超时（默认 30s）。
//
// 调用 Tab 方法时若传入的 ctx 没有 deadline，库自动套用该超时，避免 Chrome 卡死导致永久阻塞。
// 若 ctx 已有 deadline 则以调用方为准。传 0 表示关闭内置超时。
func WithDefaultTimeout(d time.Duration) Option { return config.WithDefaultTimeout(d) }

// WithWindowSize 指定窗口大小，例如 "1920,1080"
func WithWindowSize(size string) Option { return config.WithWindowSize(size) }

// WithConnectTimeout 设置连接 Chrome 的超时时间
func WithConnectTimeout(d time.Duration) Option { return config.WithConnectTimeout(d) }

// WithFlag 添加自定义 Chrome 启动参数
func WithFlag(name, value string) Option { return config.WithFlag(name, value) }

// WithLogger 设置库内部日志输出。默认静默（丢弃所有日志）；
// 传入自定义 *slog.Logger 即可观察连接、启动等过程，例如：
//
//	chromium.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
func WithLogger(l *slog.Logger) Option { return config.WithLogger(l) }

// 以下是 chrome 相关、需由门面转发的公开 API（实现见 chrome/path.go）。

// SearchedChromePaths 返回最近一次自动查找时枚举过的全部路径。
// 主要用于排查「找不到浏览器」：错误里会列出这些位置，便于确认真实安装路径。
func SearchedChromePaths() []string { return chrome.SearchedChromePaths() }

// RefreshChromePath 丢弃缓存的浏览器路径，下次查找时重新枚举。
// 场景：进程启动后用户才安装浏览器，或安装位置发生了变化。
func RefreshChromePath() { chrome.RefreshChromePath() }

// 以下是 cookie 相关、需由门面转发的公开 API。

// ParseCookiesJSON 解析 Cookie JSON（chrome 扩展导出 / 本包与 session 包导出的格式均可）。
// 顶层必须是数组；空内容返回空切片而不是错误。
func ParseCookiesJSON(data []byte) ([]Cookie, error) { return cookie.ParseCookiesJSON(data) }

// 以下是 page 相关、需由门面转发的公开 API。
//
// Go 不允许给非本包定义的类型添加方法，因此 Tab / Element / Frame / FrameElement / Selector /
// WaitBuilder 的全部方法都定义在 page 包里，门面只做别名转发。别名化后这 6 个类型的字段
// 与方法会从 go doc 输出里消失，因此由 api_test.go 做编译期冻结（方法表达式 var 块）+ 反射冻结字段。

// Tab 是一个被托管的标签页。
//
// 定义见 page。所有 I/O 方法都接受调用方传入的 ctx（应从 Ctx 派生），
// 由调用方控制超时与取消。
type Tab = page.Tab

// Element 是选择器定位到的单个元素。
//
// 定义见 page。Element 不缓存 DOM 节点，每次操作都重新定位。
type Element = page.Element

// Frame 表示页面内的一个 iframe。
//
// 定义见 page。导航或 iframe 重建后 Frame 会失效（ErrFrameDetached），
// 需要重新用 Tab.Frame / FrameByURL / FrameByName 取。
type Frame = page.Frame

// FrameElement 是在 iframe 内部定位到的元素。
//
// 定义见 page。
type FrameElement = page.FrameElement

// Selector 描述「怎么定位一个元素」。
//
// 定义见 page。用 CSS / XPath / ID / JS 构造，不要直接构造结构体。
type Selector = page.Selector

// WaitBuilder 是链式等待条件构造器，由 Tab.Wait / Element.Wait 返回。
//
// 定义见 page。必须指定条件后再 Do，否则返回 ErrWaitConditionUnset。
type WaitBuilder = page.WaitBuilder

// CSS 按 CSS 选择器定位（chromedp.ByQuery）。
//
//	chromium.CSS("div.item > a")
//	chromium.CSS("#login")
func CSS(sel string) Selector { return page.CSS(sel) }

// XPath 按 XPath 表达式定位（chromedp.BySearch）。
//
//	chromium.XPath("//div[@class='item']/a")
//	chromium.XPath("//button[text()='登录']")
func XPath(expr string) Selector { return page.XPath(expr) }

// ID 按元素 id 定位（chromedp.ByID），不需要写 '#' 前缀。
//
//	chromium.ID("username")
func ID(id string) Selector { return page.ID(id) }

// JS 直接用一段「返回 DOM 元素的 JS 表达式」定位（chromedp.ByJSPath）。
//
// 表达式交给 Runtime.evaluate 执行，必须是可信内容（不做转义）。主要用于拿其他三种方式
// 够不到的元素，典型是 Shadow DOM：
//
//	chromium.JS(`document.querySelector('#host').shadowRoot.querySelector('#inner')`)
//
// 只支持返回单个元素；要取多个请改用 CSS + 遍历。
func JS(expr string) Selector { return page.JS(expr) }

// 以下是 browser 相关、需由门面转发的公开 API（受与 page 一节相同的约束）。

// ContextOption 是创建隔离上下文时的可选项。
//
// 它是上游 chromedp.CreateBrowserContextOption 的别名，与 browser 里的同名声明指向同一类型
// （两处各声明一次，使门面与实现包的签名各自可读写）。
type ContextOption = chromedp.CreateBrowserContextOption

// Browser 是一个被托管的浏览器实例：一个 Chrome 进程 + 一个共享的 chromedp 连接。
//
// 定义见 browser。典型用法是 OpenPage 一次拿到 Browser 与首个 Tab，
// 之后用 NewTab / GetTab / LatestTab 取标签页，用完 Close。
type Browser = browser.Browser

// BrowserContext 是一个命名的隔离上下文（CDP 的 BrowserContext）。
//
// 定义见 browser。同一个 Browser 下的多个上下文之间 Cookie / 存储完全隔离，
// 比各起一个 Chrome 进程轻量得多，适合多账户场景。
type BrowserContext = browser.BrowserContext

// NewBrowser 创建一个 Browser，但**不立即连接/启动**浏览器。
//
// port 为调试端口；opts 为配置项（见各 WithXxx）。随后调用 Connect 真正建连。
func NewBrowser(port int, opts ...Option) *Browser { return browser.NewBrowser(port, opts...) }

// OpenPage 一步到位：连上（必要时启动）指定端口的浏览器，并返回首个可用标签页。
//
// 这是最常用的入口。返回的 Browser 与 Tab 都由调用方负责关闭（defer b.Close()）。
func OpenPage(ctx context.Context, port int, opts ...Option) (*Browser, *Tab, error) {
	return browser.OpenPage(ctx, port, opts...)
}

// WithContextProxy 为隔离上下文指定独立代理，仅在上下文首次创建时生效。
//
//	chromium.WithContextProxy("http://127.0.0.1:7891")
func WithContextProxy(proxy string) ContextOption { return browser.WithContextProxy(proxy) }

// 以下是 profile 相关、需由门面转发的公开 API（受与 page 一节相同的约束）。
// 依赖方向：profile -> browser。

// Profile 代表一个命名的浏览器档案：独立的用户数据目录 + 独立端口，
// 因此各 Profile 之间的 Cookie / 登录态 / 代理 / UA 完全隔离，且登录态持久化到磁盘。
//
// 定义见 profile。Name / Dir / Port 是只读元信息，
// 已打开的浏览器用 Browser() 取（可能为 nil，表示尚未 Open）。
type Profile = profile.Profile

// ProfileManager 管理多个命名 Profile，实现多账户隔离。
//
// 定义见 profile。同名 Profile 复用同一个 Browser，首次打开时才真正
// 连接/启动 Chrome（懒加载）；并发模型是「同名串行、异名并行」。
type ProfileManager = profile.ProfileManager

// NewProfileManager 创建一个 Profile 管理器。
//   - baseDir：所有 Profile 用户数据目录的根目录，每个 Profile 落在 baseDir/<name>。
//   - basePort：起始调试端口，按打开顺序递增分配（basePort、basePort+1 ...）。
//     传 0 则使用默认起始端口 9300。
//   - opts：应用于每个 Profile 的公共配置项（如 WithHeadless / WithProxy / WithLogger）。
//     注意：不要在此传 WithUserDataDir，它会被 Profile 各自的目录覆盖。
func NewProfileManager(baseDir string, basePort int, opts ...Option) *ProfileManager {
	return profile.NewProfileManager(baseDir, basePort, opts...)
}
