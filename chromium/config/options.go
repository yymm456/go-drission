// Package config 保存 Browser 的可配置项与默认值。
//
// 分层的意义：Options 需要被多个实现包读取——启动器要启动参数、Browser 要运行期行为、
// 档案归属校验要用户数据目录与接管策略。把它们放在这里，这些读者就都能向下依赖它，
// 而 Options 类型本身不对外暴露（门面只转发 Option 与 WithXxx），
// 调用方依旧无法自行构造配置。
package config

import (
	"io"
	"log/slog"
	"strings"
	"time"
)

// DefaultTabTimeout 是 Tab 级操作的默认超时。
// 只在调用方传入的 ctx 没有 deadline 时生效，作为「Chrome 卡死也不会永久阻塞」的兜底。
// 可用 WithDefaultTimeout 全局调整，或用 Tab.SetTimeout 单独调整某个标签页。
const DefaultTabTimeout = 30 * time.Second

// Options 保存 Browser 的可配置项。
//
// 字段导出是为了让内部各实现包都能读取；类型本身不导出，因此调用方无法直接构造或修改它，
// 只能通过 WithXxx 声明式配置 —— 与「字段不导出」时的外部行为完全一致。
type Options struct {
	ChromePath    string
	ChromePathSet bool // 是否由 WithChromePath 显式指定（区分「未指定」与「指定了空值」）
	UserDataDir   string
	// UserDataDirSet 区分「调用方显式指定了档案目录」与「库按端口生成的默认临时目录」。
	// 接管已有 Chrome 时，前者必须核实端口上的浏览器确实在用这个目录（见 VerifyPortOwner）。
	UserDataDirSet bool
	UserAgent      string
	Proxy          string
	Lang           string // --lang，影响 Accept-Language 与 navigator.language
	Headless       bool
	WindowSize     string
	AntiDetect     bool          // 是否启用反自动化检测（默认开）
	DefaultTimeout time.Duration // Tab 操作的默认超时，0 表示不自动加超时
	ConnectTimeout time.Duration
	ExtraFlags     []FlagPair   // 自定义启动参数
	Logger         *slog.Logger // 库内部日志；默认静默，由 WithLogger 开启

	// TrustExistingBrowser 跳过「端口上的浏览器是不是我要的」核实。
	// 仅供「我自己手动起的 Chrome，我确定就是它」这类场景显式声明。
	TrustExistingBrowser bool
}

// FlagPair 表示一个 Chrome 启动参数。
type FlagPair struct {
	Name  string
	Value string
}

// Option 是函数式配置项。
type Option func(*Options)

// Defaults 返回默认配置。
//   - ChromePath 留空：由启动器在真正启动前自动发现本机浏览器；
//     若调用方用 WithChromePath 显式指定，则以指定值为准且不做回退。
//   - UserDataDir 留空：由 NewBrowser 在端口确定后按端口生成默认路径。
func Defaults() *Options {
	return &Options{
		Headless:   false,
		WindowSize: "1920,1080",
		// 默认中文环境：影响 Accept-Language 与 navigator.language
		Lang:           "zh-CN",
		AntiDetect:     true,
		DefaultTimeout: DefaultTabTimeout,
		ConnectTimeout: 10 * time.Second,
		// 默认丢弃所有日志：库不应擅自向 stdout/stderr 打印，避免污染调用方输出
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// WithChromePath 指定浏览器可执行文件路径（内部实现，公开文档见门面 chromium）。
func WithChromePath(path string) Option {
	return func(o *Options) {
		o.ChromePath = strings.TrimSpace(path)
		o.ChromePathSet = o.ChromePath != ""
	}
}

// WithUserDataDir 指定用户数据目录（内部实现，公开文档见门面 chromium）。
func WithUserDataDir(dir string) Option {
	return func(o *Options) {
		o.UserDataDir = dir
		o.UserDataDirSet = strings.TrimSpace(dir) != ""
	}
}

// WithTrustExistingBrowser 跳过接管前的档案归属核实（内部实现）。
func WithTrustExistingBrowser() Option {
	return func(o *Options) {
		o.TrustExistingBrowser = true
	}
}

// WithUserAgent 指定 User-Agent（内部实现）。
func WithUserAgent(ua string) Option {
	return func(o *Options) {
		o.UserAgent = ua
	}
}

// WithProxy 指定代理（内部实现）。
func WithProxy(proxy string) Option {
	return func(o *Options) {
		o.Proxy = proxy
	}
}

// WithHeadless 是否启用无头模式（内部实现）。
func WithHeadless(headless bool) Option {
	return func(o *Options) {
		o.Headless = headless
	}
}

// WithLang 设置浏览器语言（内部实现）。
func WithLang(lang string) Option {
	return func(o *Options) {
		o.Lang = strings.TrimSpace(lang)
	}
}

// WithAntiDetect 是否启用反自动化检测（内部实现）。
func WithAntiDetect(enable bool) Option {
	return func(o *Options) {
		o.AntiDetect = enable
	}
}

// WithDefaultTimeout 设置 Tab 操作的默认超时（内部实现）。
func WithDefaultTimeout(d time.Duration) Option {
	return func(o *Options) {
		if d < 0 {
			d = 0
		}
		o.DefaultTimeout = d
	}
}

// WithWindowSize 指定窗口大小（内部实现）。
func WithWindowSize(size string) Option {
	return func(o *Options) {
		o.WindowSize = size
	}
}

// WithConnectTimeout 设置连接 Chrome 的超时时间（内部实现）。
func WithConnectTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.ConnectTimeout = d
	}
}

// WithFlag 添加自定义 Chrome 启动参数（内部实现）。
func WithFlag(name, value string) Option {
	return func(o *Options) {
		o.ExtraFlags = append(o.ExtraFlags, FlagPair{Name: name, Value: value})
	}
}

// WithLogger 设置库内部日志输出（内部实现）。
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		if l != nil {
			o.Logger = l
		}
	}
}
