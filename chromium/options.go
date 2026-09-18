package chromium

import (
	"io"
	"log/slog"
	"strings"
	"time"
)

// options 保存 Browser 的可配置项
type options struct {
	chromePath    string
	chromePathSet bool // 是否由 WithChromePath 显式指定（区分「未指定」与「指定了空值」）
	userDataDir   string
	// userDataDirSet 区分「调用方显式指定了档案目录」与「库按端口生成的默认临时目录」。
	// 接管已有 Chrome 时，前者必须核实端口上的浏览器确实在用这个目录（见 verifyPortOwner）。
	userDataDirSet bool
	userAgent      string
	proxy          string
	lang           string // --lang，影响 Accept-Language 与 navigator.language
	headless       bool
	windowSize     string
	antiDetect     bool          // 是否启用反自动化检测（默认开）
	defaultTimeout time.Duration // Tab 操作的默认超时，0 表示不自动加超时
	connectTimeout time.Duration
	extraFlags     []flagPair   // 自定义启动参数
	logger         *slog.Logger // 库内部日志；默认静默，由 WithLogger 开启

	// trustExistingBrowser 跳过「端口上的浏览器是不是我要的」核实。
	// 仅供「我自己手动起的 Chrome，我确定就是它」这类场景显式声明。
	trustExistingBrowser bool
}

// flagPair 表示一个 Chrome 启动参数
type flagPair struct {
	name  string
	value string
}

// Option 是函数式配置项
type Option func(*options)

// defaultOptions 返回默认配置
//   - chromePath 留空：由 launchChrome 在真正启动前自动发现本机浏览器；
//     若调用方用 WithChromePath 显式指定，则以指定值为准且不做回退。
//   - userDataDir 留空：由 NewBrowser 在端口确定后按端口生成默认路径。
func defaultOptions() *options {
	return &options{
		headless:   false,
		windowSize: "1920,1080",
		// 默认中文环境：影响 Accept-Language 与 navigator.language
		lang:           "zh-CN",
		antiDetect:     true,
		defaultTimeout: defaultTimeout,
		connectTimeout: 10 * time.Second,
		// 默认丢弃所有日志：库不应擅自向 stdout/stderr 打印，避免污染调用方输出
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// WithChromePath 指定浏览器可执行文件路径（支持 Chrome / Edge / Brave / Chromium）。
// 不指定时会自动在本机常见位置查找。一旦显式指定，路径不存在将直接报错，不会静默回退。
func WithChromePath(path string) Option {
	return func(o *options) {
		o.chromePath = strings.TrimSpace(path)
		o.chromePathSet = o.chromePath != ""
	}
}

// WithUserDataDir 指定用户数据目录（持久化登录态）。
//
// 显式指定后，连接阶段会核实端口上已有的 Chrome 是否确实在使用这个目录
// （见 verifyPortOwner）；核实不了会返回 ErrBrowserMismatch，而不是安静地接管
// 别人的浏览器——那会让多账号隔离无声失效。确认无误可用 WithTrustExistingBrowser 跳过。
func WithUserDataDir(dir string) Option {
	return func(o *options) {
		o.userDataDir = dir
		o.userDataDirSet = strings.TrimSpace(dir) != ""
	}
}

// WithTrustExistingBrowser 跳过「端口上已有的 Chrome 是不是我要的」这项核实。
//
// 适用场景：Chrome 是你手动启动的（因此不会有本库写入的档案标记），
// 而你确定它就是你要接管的那一个。默认关闭。
func WithTrustExistingBrowser() Option {
	return func(o *options) {
		o.trustExistingBrowser = true
	}
}

// WithUserAgent 指定 User-Agent
func WithUserAgent(ua string) Option {
	return func(o *options) {
		o.userAgent = ua
	}
}

// WithProxy 指定代理，例如 "http://127.0.0.1:7890"
func WithProxy(proxy string) Option {
	return func(o *options) {
		o.proxy = proxy
	}
}

// WithHeadless 是否启用无头模式
func WithHeadless(headless bool) Option {
	return func(o *options) {
		o.headless = headless
	}
}

// WithLang 设置浏览器语言（--lang），例如 "zh-CN"、"en-US"。
// 影响请求头 Accept-Language 与 navigator.language。传空字符串则不添加该参数。
func WithLang(lang string) Option {
	return func(o *options) {
		o.lang = strings.TrimSpace(lang)
	}
}

// WithAntiDetect 是否启用反自动化检测（默认开启）。
//
// 开启后会追加 --disable-blink-features=AutomationControlled、--excludeSwitches=enable-automation
// 等启动参数，并在每个新建标签页注入初始化脚本把 navigator.webdriver 抹成 undefined。
// 这些处理会略微改变浏览器指纹特征，若你要的是「干净原生 Chrome」（如做指纹对比实验），
// 传 false 关闭。
func WithAntiDetect(enable bool) Option {
	return func(o *options) {
		o.antiDetect = enable
	}
}

// WithDefaultTimeout 设置 Tab 操作的默认超时（默认 30s）。
//
// 作用范围：调用 Tab 方法时若传入的 ctx 没有 deadline（例如直接传 tab.Ctx 或
// context.Background()），库会自动套用该超时，避免出现「Chrome 卡死导致永久阻塞」。
// 若调用方传入的 ctx 已有 deadline，则始终以调用方为准——这条规则不会被覆盖。
// 传 0 表示关闭内置超时（退化为旧行为，完全由调用方自己管）。
func WithDefaultTimeout(d time.Duration) Option {
	return func(o *options) {
		if d < 0 {
			d = 0
		}
		o.defaultTimeout = d
	}
}

// WithWindowSize 指定窗口大小，例如 "1920,1080"
func WithWindowSize(size string) Option {
	return func(o *options) {
		o.windowSize = size
	}
}

// WithConnectTimeout 设置连接 Chrome 的超时时间
func WithConnectTimeout(d time.Duration) Option {
	return func(o *options) {
		o.connectTimeout = d
	}
}

// WithFlag 添加自定义 Chrome 启动参数
func WithFlag(name, value string) Option {
	return func(o *options) {
		o.extraFlags = append(o.extraFlags, flagPair{name, value})
	}
}

// WithLogger 设置库内部日志输出。默认静默（丢弃所有日志）；
// 传入自定义 *slog.Logger 即可观察连接、启动等过程，例如：
//
//	chromium.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}
