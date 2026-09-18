package chromium

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpnetwork "github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/yymm456/go-drission/chromium/errs"
)

// TestPublicErrorMessagesAreFrozen 锁定 18 个公开哨兵错误的**文案**。
//
// 分包重构之后，这些错误的实体搬到了 errs，本包只保留别名。别名本身
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
// 表里的文案逐字取自 API 基线（.workbuddy/分析情况/api-baseline.sig.txt）。
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

// TestErrorAliasesShareIdentityWithErrs 确认公开别名与 errs 是**同一个值**。
//
// 这条是「内部包返回、外部包判断」能继续工作的唯一支点：Session / Tab 等实现代码
// 从 errs 返回错误，调用方用 chromium.ErrXxx 判断。只要别名断成包装类型
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
// 要能被 session 读入，session 存盘的也要能被浏览器导入。
// 别名化（type Cookie = cookie.Cookie）之后，字段列表不再出现在
// `go doc -all ./chromium` 的输出里，API 闸门看不见它，只能靠这个用例守。
//
// 它守的是「契约」而不是「实现」：cookie 里改个字段名，这里就会红——
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
// 在 network 里改个字段名就是一次源码级破坏，必须在这里拦住。
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

// ---------------------------------------------------------------------------
// 6 个页面对象类型别名化后的冻结
//
// `type Tab = page.Tab` 这类别名不做任何转换（同一底层类型，*Tab 可直接互换），
// 但它会让这 6 个类型的字段与方法从 `go doc -all ./chromium` 的输出里消失，
// 公开 API 闸门（只看签名清单）因此对它们彻底失明。
//
// 所以这里用**方法表达式 var 块**把全部导出方法的签名冻住：签名一旦改动，
// 本包直接编译不过。这比反射用例严格 —— 反射查得到方法存在，查不出签名。
//
// 表里的签名逐字取自 API 基线（.workbuddy/分析情况/api-baseline.sig.txt）。

// Tab 的 35 个导出方法签名。
var (
	_ func(*Tab, context.Context) error                                    = (*Tab).BringToFront
	_ func(*Tab, context.Context, ...string) ([]Cookie, error)             = (*Tab).Cookies
	_ func(*Tab, context.Context) (string, error)                          = (*Tab).CurrentURL
	_ func(*Tab, Selector) *Element                                        = (*Tab).Ele
	_ func(*Tab, string) *Element                                          = (*Tab).EleCSS
	_ func(*Tab, string) *Element                                          = (*Tab).EleID
	_ func(*Tab, string) *Element                                          = (*Tab).EleJS
	_ func(*Tab, string) *Element                                          = (*Tab).EleXPath
	_ func(*Tab, context.Context, string) (any, error)                     = (*Tab).Eval
	_ func(*Tab, context.Context, string, ...string) error                 = (*Tab).ExportCookies
	_ func(*Tab, context.Context, ...string) ([]byte, error)               = (*Tab).ExportCookiesJSON
	_ func(*Tab, context.Context, Selector) (*Frame, error)                = (*Tab).Frame
	_ func(*Tab, context.Context, string) (*Frame, error)                  = (*Tab).FrameByName
	_ func(*Tab, context.Context, string) (*Frame, error)                  = (*Tab).FrameByURL
	_ func(*Tab, context.Context) ([]*Frame, error)                        = (*Tab).Frames
	_ func(*Tab, context.Context, ...string) ([]*cdpnetwork.Cookie, error) = (*Tab).GetCookies
	_ func(*Tab, context.Context) (string, error)                          = (*Tab).HTML
	_ func(*Tab, context.Context, string) error                            = (*Tab).ImportCookies
	_ func(*Tab, context.Context, []byte) (int, error)                     = (*Tab).ImportCookiesJSON
	_ func(*Tab, context.Context, CookieSource) (int, error)               = (*Tab).ImportCookiesSource
	_ func(*Tab, string) *Listener                                         = (*Tab).Listen
	_ func(*Tab, context.Context, string, CookieSource) error              = (*Tab).LoginWithCookies
	_ func(*Tab, context.Context, string, []byte) error                    = (*Tab).LoginWithCookiesJSON
	_ func(*Tab, context.Context, string) error                            = (*Tab).Navigate
	_ func(*Tab, context.Context) error                                    = (*Tab).Reload
	_ func(*Tab, context.Context, string) error                            = (*Tab).Screenshot
	_ func(*Tab, context.Context, Cookie) error                            = (*Tab).SetCookie
	_ func(*Tab, context.Context, []Cookie) error                          = (*Tab).SetCookies
	_ func(*Tab, time.Duration)                                            = (*Tab).SetTimeout
	_ func(*Tab, context.Context) (string, error)                          = (*Tab).Title
	_ func(*Tab) string                                                    = (*Tab).URL
	_ func(*Tab) *WaitBuilder                                              = (*Tab).Wait
	_ func(*Tab, context.Context) error                                    = (*Tab).WaitReady
	_ func(*Tab, context.Context, string) error                            = (*Tab).WaitURL
	_ func(*Tab, context.Context) (int64, error)                           = (*Tab).WindowID
)

// Element 的 14 个导出方法签名。
var (
	_ func(*Element, context.Context, string) (string, error) = (*Element).Attribute
	_ func(*Element, context.Context) error                   = (*Element).Click
	_ func(*Element, context.Context) error                   = (*Element).ClickJS
	_ func(*Element, context.Context) (int, error)            = (*Element).Count
	_ func(*Element, context.Context, string) (any, error)    = (*Element).Eval
	_ func(*Element) Selector                                 = (*Element).Selector
	_ func(*Element, context.Context, string) error           = (*Element).SendKeys
	_ func(*Element, context.Context, string) error           = (*Element).SetValue
	_ func(*Element) string                                   = (*Element).String
	_ func(*Element) *Tab                                     = (*Element).Tab
	_ func(*Element, context.Context) (string, error)         = (*Element).Text
	_ func(*Element) *WaitBuilder                             = (*Element).Wait
	_ func(*Element, context.Context, string) error           = (*Element).WaitText
	_ func(*Element, context.Context) error                   = (*Element).WaitVisible
)

// Frame 的 12 个导出方法签名。
var (
	_ func(*Frame, Selector) *FrameElement               = (*Frame).Ele
	_ func(*Frame, string) *FrameElement                 = (*Frame).EleCSS
	_ func(*Frame, string) *FrameElement                 = (*Frame).EleID
	_ func(*Frame, string) *FrameElement                 = (*Frame).EleJS
	_ func(*Frame, string) *FrameElement                 = (*Frame).EleXPath
	_ func(*Frame, context.Context, string) (any, error) = (*Frame).Eval
	_ func(*Frame, context.Context) (string, error)      = (*Frame).HTML
	_ func(*Frame) cdp.FrameID                           = (*Frame).ID
	_ func(*Frame) string                                = (*Frame).Name
	_ func(*Frame, context.Context, string) error        = (*Frame).Navigate
	_ func(*Frame) string                                = (*Frame).URL
	_ func(*Frame, context.Context) (string, error)      = (*Frame).URLNow
)

// FrameElement 的 7 个导出方法签名。
var (
	_ func(*FrameElement, context.Context) error           = (*FrameElement).Click
	_ func(*FrameElement, context.Context) (int, error)    = (*FrameElement).Count
	_ func(*FrameElement) *Frame                           = (*FrameElement).Frame
	_ func(*FrameElement) Selector                         = (*FrameElement).Selector
	_ func(*FrameElement, context.Context, string) error   = (*FrameElement).SetValue
	_ func(*FrameElement) string                           = (*FrameElement).String
	_ func(*FrameElement, context.Context) (string, error) = (*FrameElement).Text
)

// Selector 的 3 个导出方法签名。
var (
	_ func(Selector) bool   = (Selector).Empty
	_ func(Selector) string = (Selector).Mode
	_ func(Selector) string = (Selector).String
)

// WaitBuilder 的 9 个导出方法签名。
var (
	_ func(*WaitBuilder, int) *WaitBuilder           = (*WaitBuilder).Count
	_ func(*WaitBuilder, context.Context) error      = (*WaitBuilder).Do
	_ func(*WaitBuilder, *Element) *WaitBuilder      = (*WaitBuilder).Element
	_ func(*WaitBuilder) *WaitBuilder                = (*WaitBuilder).Present
	_ func(*WaitBuilder) *WaitBuilder                = (*WaitBuilder).Ready
	_ func(*WaitBuilder, string) *WaitBuilder        = (*WaitBuilder).Text
	_ func(*WaitBuilder, time.Duration) *WaitBuilder = (*WaitBuilder).Timeout
	_ func(*WaitBuilder, string) *WaitBuilder        = (*WaitBuilder).URL
	_ func(*WaitBuilder) *WaitBuilder                = (*WaitBuilder).Visible
)

// TestPageFieldsAreFrozen 钉住 6 个页面对象类型的**导出字段**。
//
// 与 TestCookieJSONTagsAreFrozen / TestRecordFieldsAreFrozen 同因：别名化之后
// 字段列表不再出现在 go doc 输出里。Tab 有 2 个导出字段（用户直接读 tab.ID / tab.Ctx）；
// 其余 5 个类型**一个导出字段都不该有** —— 它们的内部状态（元素的选择器、框架的
// isolated world 等）一旦变成导出字段，就成了事实上的公开契约，将来改不动。
func TestPageFieldsAreFrozen(t *testing.T) {
	typ := reflect.TypeFor[Tab]()

	// 只数**导出**字段：reflect 的 NumField 把私有字段也算进去（Tab 一共有 9 个，
	// 其中 cancel / mu / url / timeout / antiMu / antiDetected / logger 都是内部状态）。
	exported := 0
	for f := range typ.Fields() {
		if f.IsExported() {
			exported++
		}
	}
	if exported != 2 {
		t.Fatalf("Tab 的**导出**字段数变了：期望 2（ID / Ctx），实际 %d", exported)
	}

	// 类型要比到具体类型，不能只比 Kind：target.ID 与 string 的 Kind 都是 string，
	// 只比 Kind 的话「把 ID 换成裸 string」这种破坏会被静默放过。
	want := map[string]reflect.Type{
		"ID":  reflect.TypeFor[target.ID](),
		"Ctx": reflect.TypeFor[context.Context](),
	}
	for name, exp := range want {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("Tab 缺少导出字段 %s", name)
			continue
		}
		if f.Type != exp {
			t.Errorf("Tab.%s 的类型 = %s，期望 %s", name, f.Type, exp)
		}
	}

	// 其余 5 个类型必须没有导出字段
	others := map[string]reflect.Type{
		"Element":      reflect.TypeFor[Element](),
		"Frame":        reflect.TypeFor[Frame](),
		"FrameElement": reflect.TypeFor[FrameElement](),
		"Selector":     reflect.TypeFor[Selector](),
		"WaitBuilder":  reflect.TypeFor[WaitBuilder](),
	}
	for name, rt := range others {
		for f := range rt.Fields() {
			if f.IsExported() {
				t.Errorf("%s 多了一个导出字段 %s：内部状态不该进入公开契约", name, f.Name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Browser / BrowserContext 别名化后的冻结
//
// 病因同 page 别名：`type Browser = browser.Browser` 之后，这两个类型的字段与
// 全部方法从 `go doc -all ./chromium` 里消失，公开 API 闸门看不见它们。
//
// 方法表达式 var 块把 17 个导出方法的签名冻在**编译期**：签名一改，本包就编译不过。
// 这比反射严格 —— 反射查得到方法存在，查不出签名。
//
// 签名逐字取自 API 基线（.workbuddy/分析情况/api-baseline.sig.txt）。
//
// 注意 ContextOption：它在门面（本文件所在包）与 browser 各声明一次，
// 都是 chromedp.CreateBrowserContextOption 的别名，因此是同一个类型，
// 方法签名里写 ContextOption 与写全名完全等价。

// Browser 的 12 个导出方法签名。
var (
	_ func(*Browser)                                                                     = (*Browser).Close
	_ func(*Browser, context.Context, *Tab)                                              = (*Browser).CloseTab
	_ func(*Browser, context.Context) error                                              = (*Browser).Connect
	_ func(*Browser, context.Context, string, ...ContextOption) (*BrowserContext, error) = (*Browser).Context
	_ func(*Browser) []string                                                            = (*Browser).Contexts
	_ func(*Browser, context.Context, int) (*Tab, error)                                 = (*Browser).GetTab
	_ func(*Browser, context.Context, string) (*Tab, error)                              = (*Browser).GetTabByURL
	_ func(*Browser, context.Context) (*Tab, error)                                      = (*Browser).LatestTab
	_ func(*Browser, context.Context) (*Tab, error)                                      = (*Browser).NewTab
	_ func(*Browser) int                                                                 = (*Browser).PID
	_ func(*Browser) int                                                                 = (*Browser).Port
	_ func(*Browser, context.Context) ([]*Tab, error)                                    = (*Browser).Tabs
)

// BrowserContext 的 5 个导出方法签名。
var (
	_ func(*BrowserContext)                                = (*BrowserContext).Close
	_ func(*BrowserContext, context.Context, *Tab)         = (*BrowserContext).CloseTab
	_ func(*BrowserContext) string                         = (*BrowserContext).Name
	_ func(*BrowserContext, context.Context) (*Tab, error) = (*BrowserContext).NewTab
	_ func(*BrowserContext) []*Tab                         = (*BrowserContext).Tabs
)

// TestBrowserFieldsAreFrozen 钉住 Browser / BrowserContext 的**导出字段数：必须是 0**。
//
// 这两个类型是「浏览器进程 + chromedp 连接」的门面，内部有 port / opts / rootCtx /
// tabs / 三把锁等一大堆状态。任何一个变成导出字段，就等于把「连接怎么建、锁怎么加」
// 变成了对外契约，将来想改并发模型就改不动了 —— 所以这里必须钉死 0。
//
// 反射的 NumField() 会把私有字段一起数进去，所以只数 IsExported() 的。
func TestBrowserFieldsAreFrozen(t *testing.T) {
	for name, rt := range map[string]reflect.Type{
		"Browser":        reflect.TypeFor[Browser](),
		"BrowserContext": reflect.TypeFor[BrowserContext](),
	} {
		for f := range rt.Fields() {
			if f.IsExported() {
				t.Errorf("%s 多了一个导出字段 %s（%s）：浏览器内部状态不该进入公开契约",
					name, f.Name, f.Type)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Profile / ProfileManager 别名化后的冻结
//
// 病因同 page / browser 别名：`type Profile = profile.Profile` 之后，这两个类型的字段与
// 全部方法从 `go doc -all ./chromium` 里消失，公开 API 闸门看不见它们。
//
// 方法表达式 var 块把 6 个导出方法的签名冻在**编译期**：签名一改，本包就编译不过。
// 这比反射严格 —— 反射查得到方法存在，查不出签名。
//
// 签名逐字取自 API 基线（.workbuddy/分析情况/api-baseline.sig.txt）。

// Profile 的 1 个导出方法签名。
var (
	_ func(*Profile) *Browser = (*Profile).Browser
)

// ProfileManager 的 5 个导出方法签名。
var (
	_ func(*ProfileManager, string)                                          = (*ProfileManager).Close
	_ func(*ProfileManager)                                                  = (*ProfileManager).CloseAll
	_ func(*ProfileManager, string) (*Profile, error)                        = (*ProfileManager).Get
	_ func(*ProfileManager) []string                                         = (*ProfileManager).Names
	_ func(*ProfileManager, context.Context, string) (*Browser, *Tab, error) = (*ProfileManager).Open
)

// TestProfileFieldsAreFrozen 钉住 Profile / ProfileManager 的**导出字段**。
//
// Profile 恰好有 3 个导出字段（Name / Dir / Port），它们是调用方要读的只读元信息，
// 类型也必须比到具体类型而不是 Kind；ProfileManager **必须 0 个** ——
// 它的 baseDir / basePort / opts / 端口池 / per-name 锁全是内部状态，
// 任何一个变成导出字段就等于把「档案目录怎么算、端口怎么分」写进对外契约。
//
// 反射的 NumField() 会把私有字段一起数进去，所以只数 IsExported() 的。
func TestProfileFieldsAreFrozen(t *testing.T) {
	rt := reflect.TypeFor[Profile]()
	exported := 0
	for f := range rt.Fields() {
		if f.IsExported() {
			exported++
		}
	}
	if exported != 3 {
		t.Fatalf("Profile 的**导出**字段数变了：期望 3（Name / Dir / Port），实际 %d", exported)
	}
	want := map[string]reflect.Type{
		"Name": reflect.TypeFor[string](),
		"Dir":  reflect.TypeFor[string](),
		"Port": reflect.TypeFor[int](),
	}
	for name, exp := range want {
		f, ok := rt.FieldByName(name)
		if !ok {
			t.Errorf("Profile 缺少导出字段 %s", name)
			continue
		}
		if f.Type != exp {
			t.Errorf("Profile.%s 的类型 = %s，期望 %s", name, f.Type, exp)
		}
	}

	for f := range reflect.TypeFor[ProfileManager]().Fields() {
		if f.IsExported() {
			t.Errorf("ProfileManager 多了一个导出字段 %s（%s）：档案内部状态不该进入公开契约",
				f.Name, f.Type)
		}
	}
}
