package chromium

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// TestPublicErrorMessagesAreFrozen 锁定 18 个公开哨兵错误的**文案**。
//
// 分包重构之后，这些错误的实体搬到了 internal/errs，本包只保留别名。别名本身
// 不改变语义（同一指针，errors.Is 双向成立），但它会让 `go doc -all` 的输出由
//
//	ErrClosed = errors.New("chromium: 浏览器连接已关闭")
//
// 退化成
//
//	ErrClosed = errs.ErrClosed
//
// 也就是说，文案从公开签名里「看不见」了。而文案是对外契约的一部分：调用方会打
// 进日志、会拿它做人工排障、偶尔会直接比对字符串。一次「只为分包、不改行为」的
// 重构不该让它们静默漂移——所以把重构前的原文固化在这里当回归网。
//
// 表里的文案逐字取自 S0 基线（.workbuddy/分析情况/api-baseline.sig.txt）。
// 新增哨兵错误时同步加进来；**修改已有文案要有明确理由**，改不动就是这里的意义。
func TestPublicErrorMessagesAreFrozen(t *testing.T) {
	want := []struct {
		name string
		err  error
		msg  string
	}{
		{"ErrAlreadyConnected", ErrAlreadyConnected, "chromium: 浏览器已连接，请勿重复 Connect"},
		{"ErrBrowserMismatch", ErrBrowserMismatch, "chromium: 端口上的浏览器与期望的用户数据目录不一致"},
		{"ErrChromeNotFound", ErrChromeNotFound, "chromium: 未找到可用的浏览器可执行文件"},
		{"ErrClosed", ErrClosed, "chromium: 浏览器连接已关闭"},
		{"ErrContextClosed", ErrContextClosed, "chromium: 隔离上下文已关闭"},
		{"ErrElementNotFound", ErrElementNotFound, "chromium: 没有匹配到元素"},
		{"ErrEmptyURL", ErrEmptyURL, "chromium: 地址为空或格式非法"},
		{"ErrFrameDetached", ErrFrameDetached, "chromium: iframe 已随页面导航失效"},
		{"ErrFrameNotFound", ErrFrameNotFound, "chromium: 没有找到 iframe"},
		{"ErrInvalidContext", ErrInvalidContext, "chromium: ctx 必须从 tab.Ctx 派生，否则命令无法路由到目标标签页"},
		{"ErrInvalidCookie", ErrInvalidCookie, "chromium: Cookie 参数不合法"},
		{"ErrInvalidCookieJSON", ErrInvalidCookieJSON, "chromium: 解析 Cookie JSON 失败"},
		{"ErrListenerStarted", ErrListenerStarted, "chromium: 监听器已启动"},
		{"ErrNoTab", ErrNoTab, "chromium: 当前没有标签页"},
		{"ErrNotConnected", ErrNotConnected, "chromium: 浏览器尚未连接，请先调用 Connect 或 OpenPage"},
		{"ErrProfileClosed", ErrProfileClosed, "chromium: ProfileManager 已关闭"},
		{"ErrSelectorRequired", ErrSelectorRequired, "chromium: 该操作需要一个非空的选择器"},
		{"ErrWaitConditionUnset", ErrWaitConditionUnset, "chromium: 未指定等待条件（Visible/Present/Text/Count/URL/Ready 之一）"},
	}

	if len(want) != 18 {
		t.Fatalf("公开哨兵错误应有 18 个，表里有 %d 个——新增/删除错误时请同步本表", len(want))
	}

	for _, tc := range want {
		if tc.err == nil {
			t.Errorf("%s 为 nil", tc.name)
			continue
		}
		if got := tc.err.Error(); got != tc.msg {
			t.Errorf("%s 文案漂移：\n  期望 %q\n  实际 %q", tc.name, tc.msg, got)
		}
	}
}

// TestErrorAliasesShareIdentityWithErrs 确认公开别名与 internal/errs 是**同一个值**。
//
// 这条是「内部包返回、外部包判断」能继续工作的唯一支点：Session / Tab 等实现代码
// 从 internal/errs 返回错误，调用方用 chromium.ErrXxx 判断。只要别名断成包装类型
// （例如写成 fmt.Errorf("%w", errs.ErrClosed) 或自定义 error 实现），errors.Is 就会
// 在调用方那一侧静默失效——编译期完全看不出来，所以必须钉在这里。
//
// 断言用双向 errors.Is 而不是直接比接口值：前者恰好覆盖三种可能的破坏方式，
// 且与调用方实际用法一致（调用方只会用 errors.Is）。
//
//   - 换成包装 fmt.Errorf("%w", errs.ErrClosed)：反向 Is 失败；
//   - 换成同文案的新错误 errors.New("chromium: 浏览器连接已关闭")：双向都失败；
//   - 换成本包自定义 error 类型：双向都失败。
//
// 直接比接口值表达式更直白，但会被 errorlint 判为「比较会漏掉包装错误」，
// 而它的表达能力已被双向 Is 完全覆盖，所以不引入 nolint。
func TestErrorAliasesShareIdentityWithErrs(t *testing.T) {
	pairs := []struct {
		name  string
		pub   error
		inner error
	}{
		{"ErrAlreadyConnected", ErrAlreadyConnected, errs.ErrAlreadyConnected},
		{"ErrBrowserMismatch", ErrBrowserMismatch, errs.ErrBrowserMismatch},
		{"ErrChromeNotFound", ErrChromeNotFound, errs.ErrChromeNotFound},
		{"ErrClosed", ErrClosed, errs.ErrClosed},
		{"ErrContextClosed", ErrContextClosed, errs.ErrContextClosed},
		{"ErrElementNotFound", ErrElementNotFound, errs.ErrElementNotFound},
		{"ErrEmptyURL", ErrEmptyURL, errs.ErrEmptyURL},
		{"ErrFrameDetached", ErrFrameDetached, errs.ErrFrameDetached},
		{"ErrFrameNotFound", ErrFrameNotFound, errs.ErrFrameNotFound},
		{"ErrInvalidContext", ErrInvalidContext, errs.ErrInvalidContext},
		{"ErrInvalidCookie", ErrInvalidCookie, errs.ErrInvalidCookie},
		{"ErrInvalidCookieJSON", ErrInvalidCookieJSON, errs.ErrInvalidCookieJSON},
		{"ErrListenerStarted", ErrListenerStarted, errs.ErrListenerStarted},
		{"ErrNoTab", ErrNoTab, errs.ErrNoTab},
		{"ErrNotConnected", ErrNotConnected, errs.ErrNotConnected},
		{"ErrProfileClosed", ErrProfileClosed, errs.ErrProfileClosed},
		{"ErrSelectorRequired", ErrSelectorRequired, errs.ErrSelectorRequired},
		{"ErrWaitConditionUnset", ErrWaitConditionUnset, errs.ErrWaitConditionUnset},
	}

	for _, tc := range pairs {
		// 同一个指针必然双向成立；任一方向失败都说明别名被换成了包装 / 副本 / 新类型。
		if !errors.Is(tc.inner, tc.pub) {
			t.Errorf("errors.Is(%s 内部值, 公开别名) 为 false——别名不再是同一个值", tc.name)
		}
		if !errors.Is(tc.pub, tc.inner) {
			t.Errorf("errors.Is(公开别名, %s 内部值) 为 false——别名不再是同一个值", tc.name)
		}
	}
}

// TestCookieJSONTagsAreFrozen 钉住 Cookie 的字段名、JSON tag 与类型。
//
// 这是一条**跨包契约**：session.CookieItem 用同一套字段名，浏览器导出的 cookies.json
// 要能被 session 读入，session 存盘的也要能被浏览器导入（设计文档 §8.2 第 7 条）。
// 别名化（type Cookie = cookie.Cookie）之后，字段列表不再出现在
// `go doc -all ./chromium` 的输出里，API 闸门看不见它，只能靠这个用例守。
//
// 它守的是「契约」而不是「实现」：internal/cookie 里改个字段名，这里就会红——
// 那正是我们要的信号。
func TestCookieJSONTagsAreFrozen(t *testing.T) {
	want := map[string][2]string{
		"Name":     {"name", "string"},
		"Value":    {"value", "string"},
		"Domain":   {"domain", "string"},
		"Path":     {"path", "string"},
		"HTTPOnly": {"http_only", "bool"},
		"Secure":   {"secure", "bool"},
		"SameSite": {"same_site", "string"},
		"Expires":  {"expires", "float64"},
	}

	typ := reflect.TypeFor[Cookie]()
	if n := len(want); typ.NumField() != n {
		t.Fatalf("Cookie 的字段数变了：期望 %d，实际 %d（跨包 JSON 契约可能已被破坏）",
			n, typ.NumField())
	}
	for f := range typ.Fields() {
		exp, ok := want[f.Name]
		if !ok {
			t.Errorf("出现了预期之外的字段 %s", f.Name)
			continue
		}
		if tag := f.Tag.Get("json"); tag != exp[0] {
			t.Errorf("字段 %s 的 json tag = %q，期望 %q", f.Name, tag, exp[0])
		}
		if kind := f.Type.Kind().String(); kind != exp[1] {
			t.Errorf("字段 %s 的类型 = %s，期望 %s", f.Name, kind, exp[1])
		}
	}
}

// Listener 的 7 个导出方法在别名化之后也不再出现在 go doc 输出里。
// 用方法表达式做**编译期**冻结：签名一旦改动就编译不过，零运行成本。
// （这比写反射用例更强——反射只能查到「方法存在」，查不出签名。）
var (
	_ func(*Listener)                        = (*Listener).Clear
	_ func(*Listener) int                    = (*Listener).Len
	_ func(*Listener, int) *Listener         = (*Listener).MaxRecords
	_ func(*Listener) []*Record              = (*Listener).Records
	_ func(*Listener, context.Context) error = (*Listener).Start
	_ func(*Listener)                        = (*Listener).Stop
	_ func(*Listener)                        = (*Listener).WaitIdle
)

// TestRecordFieldsAreFrozen 钉住 Record 的导出字段名与类型。
//
// 与 TestCookieJSONTagsAreFrozen 同因：`type Record = network.Record` 之后，
// 字段列表不再出现在 `go doc -all ./chromium` 的输出里，API 闸门看不见它。
// Record 的字段是用户直接读取的公开数据（rec.Status / rec.URL / ...），
// 在 internal/network 里改个字段名就是一次源码级破坏，必须在这里拦住。
func TestRecordFieldsAreFrozen(t *testing.T) {
	want := map[string]string{
		"RequestID":       "string",
		"URL":             "string",
		"Method":          "string",
		"RequestHeaders":  "map",
		"RequestBody":     "string",
		"Status":          "int64",
		"ResponseHeaders": "map",
		"ResponseBody":    "string",
	}

	typ := reflect.TypeFor[Record]()
	if n := len(want); typ.NumField() != n {
		t.Fatalf("Record 的字段数变了：期望 %d，实际 %d（公开数据契约可能已被破坏）",
			n, typ.NumField())
	}
	for f := range typ.Fields() {
		exp, ok := want[f.Name]
		if !ok {
			t.Errorf("出现了预期之外的字段 %s", f.Name)
			continue
		}
		if kind := f.Type.Kind().String(); kind != exp {
			t.Errorf("字段 %s 的类型 = %s，期望 %s", f.Name, kind, exp)
		}
	}
}
