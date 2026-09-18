package session

import "errors"

// 本包对外暴露的固定错误，便于调用方用 errors.Is / errors.As 判断，
// 而不是去比对错误字符串（文案会随版本调整，哨兵值不会）。
//
// 约定：需要携带动态信息（URL、状态码、路径等）的错误一律用 fmt.Errorf 以 %w
// 包装这些哨兵值，例如：
//
//	fmt.Errorf("%w: %s", ErrEmptyURL, rawURL)
//	// 调用方：errors.Is(err, session.ErrEmptyURL) == true
var (
	// ErrNilRequest 表示传给 Do 的 *http.Request 为 nil。
	ErrNilRequest = errors.New("session: req 不能为 nil")

	// ErrNilResponse 表示底层返回了 nil 响应，属于不该出现的状态。
	ErrNilResponse = errors.New("session: 响应为 nil")

	// ErrEmptyURL 表示 URL 为空或只含空白字符。
	ErrEmptyURL = errors.New("session: URL 不能为空")

	// ErrInvalidURL 表示 URL 解析失败，或缺少 scheme / host。
	// 只写 "/path"、"example.com/a" 这类不完整地址会命中它——
	// 底层 http.Client 只会含糊地报 "unsupported protocol scheme"，
	// 提前拦下能明确指出是地址写错了。
	ErrInvalidURL = errors.New("session: URL 格式非法")

	// ErrInvalidProxy 表示代理地址无法解析或缺少协议头。
	ErrInvalidProxy = errors.New("session: 代理地址非法")

	// ErrEmptyBody 表示响应体为空，无法按 JSON 解析。
	ErrEmptyBody = errors.New("session: 响应体为空")

	// ErrInvalidJSON 表示响应体不是合法 JSON。
	ErrInvalidJSON = errors.New("session: 解析 JSON 失败")

	// ErrCookieFileRead 表示读取 Cookie 文件失败（不含「文件不存在」，那属于正常首次运行）。
	ErrCookieFileRead = errors.New("session: 读取 Cookie 文件失败")

	// ErrCookieFileWrite 表示写入 Cookie 文件失败。
	ErrCookieFileWrite = errors.New("session: 写入 Cookie 文件失败")

	// ErrInvalidCookieJSON 表示 Cookie JSON 结构不合法。
	ErrInvalidCookieJSON = errors.New("session: 解析 Cookie JSON 失败")

	// ErrUnsupportedProtocol 表示 URL 使用了本会话不支持的协议（如 file://、ftp://）。
	ErrUnsupportedProtocol = errors.New("session: 不支持的 URL 协议")

	// ErrBodyTooLarge 表示响应体超过 WithMaxBodySize 设定的上限。
	//
	// Response 会把响应体整体读进内存（这是 Text/JSON 可反复读取的前提），
	// 因此没有上限时，一个误配成 Content-Length: 8G 的接口足以把进程撑爆。
	// 设了上限后超限立即中止，不会先把内存吃掉再报错。
	ErrBodyTooLarge = errors.New("session: 响应体超过大小上限")
)

// IsRetryable 判断错误是否值得重试。
//
// 判定依据是错误是否「与请求内容无关」：网络层抖动（连接重置、超时）属于可重试；
// 而 URL 非法、JSON 解析失败、Cookie 文件缺失这类属于调用方问题，重试多少次都一样。
//
// 注意：本函数只做粗粒度分类，不区分 4xx/5xx——状态码需要读 Response，
// 而 Response 存在时请求本身已经成功返回，不属于「传输层失败」。
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrInvalidURL),
		errors.Is(err, ErrEmptyURL),
		errors.Is(err, ErrInvalidProxy),
		errors.Is(err, ErrNilRequest),
		errors.Is(err, ErrEmptyBody),
		errors.Is(err, ErrInvalidJSON),
		errors.Is(err, ErrInvalidCookieJSON),
		errors.Is(err, ErrCookieFileRead),
		errors.Is(err, ErrCookieFileWrite),
		// 超限说明对端真的会发这么大的东西，重试只会再爆一次
		errors.Is(err, ErrBodyTooLarge):
		return false
	}
	// 其余（连接失败、超时、EOF 等传输层错误）视为可重试
	return true
}
