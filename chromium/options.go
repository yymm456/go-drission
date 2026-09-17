package chromium

import "time"

// options 保存 Browser 的可配置项
type options struct {
	chromePath     string
	userDataDir    string
	userAgent      string
	proxy          string
	headless       bool
	windowSize     string
	connectTimeout time.Duration
	extraFlags     []flagPair // 自定义启动参数
}

// flagPair 表示一个 Chrome 启动参数
type flagPair struct {
	name  string
	value string
}

// Option 是函数式配置项
type Option func(*options)

// defaultOptions 返回默认配置
// 注意：userDataDir 留空，由 NewBrowser 在端口确定后按端口生成默认路径；
// 若调用方使用 WithUserDataDir 显式指定，则以指定值为准。
func defaultOptions() *options {
	return &options{
		chromePath:     defaultChromePath(),
		headless:       false,
		windowSize:     "1280,800",
		connectTimeout: 10 * time.Second,
	}
}

// WithChromePath 指定 Chrome 可执行文件路径
func WithChromePath(path string) Option {
	return func(o *options) {
		o.chromePath = path
	}
}

// WithUserDataDir 指定用户数据目录（持久化登录态）
func WithUserDataDir(dir string) Option {
	return func(o *options) {
		o.userDataDir = dir
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
