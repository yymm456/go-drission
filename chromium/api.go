package chromium

// 本文件是门面的第一部分（S1）：把已经下沉到 internal 的公开符号按原名转发出去。
//
// 为什么用「类型别名 + 转发函数」而不是包装类型：
//   - 类型别名（type Option = config.Option）是**同一个类型**，不做任何装箱与转换，
//     调用方原有的写法、传参、接口实现关系全部保持不变；
//   - 转发函数保证文档注释只有一个出处（就是这里），且不存在可被外部改写的函数变量。
//
// 约定：实现内部一律直接使用 internal 包里的名字（errs.ErrClosed / config.Defaults），
// 本文件里的别名与转发只是「对外契约」，不承担逻辑。

import (
	"log/slog"
	"time"

	"github.com/yymm456/go-drission/chromium/internal/config"
	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// Option 是函数式配置项
type Option = config.Option

// 对外暴露的哨兵错误；与 internal/errs 里的是同一个值（同一指针），
// 因此 errors.Is 在「内部包返回、外部包判断」之间照旧成立。
var (
	// ErrClosed 表示 Browser 已经关闭，不再接受任何操作。
	ErrClosed = errs.ErrClosed

	// ErrNotConnected 表示 Browser 尚未 Connect，rootCtx 还不存在。
	// 常见于 NewBrowser 之后直接调用 NewTab / Context 而漏了 Connect。
	ErrNotConnected = errs.ErrNotConnected

	// ErrInvalidContext 表示传入的 ctx 不含会话路由信息。
	// 传给 Tab / Listener 的 ctx 必须派生自 tab.Ctx，否则命令落不到目标标签页上。
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

	// ErrFrameDetached 表示 Frame 已随着页面导航 / iframe 重建而失效，
	// 且按最初的定位依据也无法重新找到它。调用方应重新用 Tab.Frame /
	// FrameByURL / FrameByName 取一个新的 Frame 对象。
	ErrFrameDetached = errs.ErrFrameDetached

	// ErrSelectorRequired 表示该操作需要一个选择器 / 元素，但没有提供。
	// 例如 tab.EleCSS("") 或对空选择器构造的 Element 调 Click。
	ErrSelectorRequired = errs.ErrSelectorRequired

	// ErrChromeNotFound 表示未在常见位置找到可用的浏览器可执行文件。
	ErrChromeNotFound = errs.ErrChromeNotFound

	// ErrBrowserMismatch 表示端口上确实有一个 Chrome，但它不是调用方期望的那一个。
	//
	// 典型场景：调用方显式指定了 WithUserDataDir("./profiles/a") 且端口上已有
	// 别的 Chrome 在跑。若直接接管，你以为在用档案 A，实际用的是别人的浏览器
	// （别的用户数据目录、别人的登录态），Cookie 隔离与多账号方案全部静默失效。
	// 确认「就是它」请加 WithTrustExistingBrowser()。
	ErrBrowserMismatch = errs.ErrBrowserMismatch

	// ErrInvalidCookie 表示待注入的 Cookie 参数不合法。
	// 包括：缺 Name / 缺 Domain / SameSite=None 却没有 Secure。
	ErrInvalidCookie = errs.ErrInvalidCookie

	// ErrInvalidCookieJSON 表示 Cookie JSON 不是合法的数组结构。
	ErrInvalidCookieJSON = errs.ErrInvalidCookieJSON

	// ErrEmptyURL 表示传入的地址为空或缺少协议头 / 主机名。
	// LoginWithCookies 这类「先注入再打开」的接口需要可用的绝对地址。
	ErrEmptyURL = errs.ErrEmptyURL
)

// WithChromePath 指定浏览器可执行文件路径（支持 Chrome / Edge / Brave / Chromium）。
// 不指定时会自动在本机常见位置查找。一旦显式指定，路径不存在将直接报错，不会静默回退。
func WithChromePath(path string) Option { return config.WithChromePath(path) }

// WithUserDataDir 指定用户数据目录（持久化登录态）。
//
// 显式指定后，连接阶段会核实端口上已有的 Chrome 是否确实在使用这个目录
// （见 verifyPortOwner）；核实不了会返回 ErrBrowserMismatch，而不是安静地接管
// 别人的浏览器——那会让多账号隔离无声失效。确认无误可用 WithTrustExistingBrowser 跳过。
func WithUserDataDir(dir string) Option { return config.WithUserDataDir(dir) }

// WithTrustExistingBrowser 跳过「端口上已有的 Chrome 是不是我要的」这项核实。
//
// 适用场景：Chrome 是你手动启动的（因此不会有本库写入的档案标记），
// 而你确定它就是你要接管的那一个。默认关闭。
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

// WithAntiDetect 是否启用反自动化检测（默认开启）。
//
// 开启后会追加 --disable-blink-features=AutomationControlled、--excludeSwitches=enable-automation
// 等启动参数，并在每个新建标签页注入初始化脚本把 navigator.webdriver 抹成 undefined。
// 这些处理会略微改变浏览器指纹特征，若你要的是「干净原生 Chrome」（如做指纹对比实验），
// 传 false 关闭。
func WithAntiDetect(enable bool) Option { return config.WithAntiDetect(enable) }

// WithDefaultTimeout 设置 Tab 操作的默认超时（默认 30s）。
//
// 作用范围：调用 Tab 方法时若传入的 ctx 没有 deadline（例如直接传 tab.Ctx 或
// context.Background()），库会自动套用该超时，避免出现「Chrome 卡死导致永久阻塞」。
// 若调用方传入的 ctx 已有 deadline，则始终以调用方为准——这条规则不会被覆盖。
// 传 0 表示关闭内置超时（退化为旧行为，完全由调用方自己管）。
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
