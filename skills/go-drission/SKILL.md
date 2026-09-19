---
name: go-drission
description: 在 go-drission 仓库（Go 浏览器自动化库）里干活时使用——包括调用它的 API 写脚本，以及为它本身加功能/修 bug/补测试。涵盖 Browser / Tab / Element / Frame / Selector / Wait / Listener / BrowserContext / Profile / Session / Cookie 接力的真实 API、必须遵守的 context 与生命周期约束、哨兵错误的判定方式、测试分层与 API 冻结规则。任务涉及浏览器自动化、页面抓取、免登录、Cookie 接力、网络监听、多账户隔离，或需要理解/修改本仓库代码时加载本 skill。
---

# go-drission

Go 的浏览器自动化库：**用 `chromium` 包驱动真实 Chrome（走 CDP），用 `session` 包做纯 HTTP 抓取，两者通过 Cookie 互相接力。**

风格上受 Python DrissionPage 启发（「浏览器模式 + 请求模式 + 登录态互通」这个思路），但**不是它的翻译**，也不是 chromedp 的薄包装：它在 chromedp 之上重新组织了页面对象模型（Tab / Element / Frame / Wait）、多账户隔离（BrowserContext / ProfileManager）、网络监听（Listener）与错误体系。**不要按 Python DrissionPage 的 API 去猜这里的 API。**

```
chromium/        门面（api.go 只有类型别名 + 转发函数，实现 0 行）
├── browser/     Browser / BrowserContext / target 同步
├── page/        Tab / Element / Frame / FrameElement / Selector / WaitBuilder
├── chrome/      Chrome 进程、端口探测、数据目录、排他锁、可执行文件查找
├── network/     Listener / Record / 事件处理
├── profile/     Profile / ProfileManager
├── cookie/      Cookie 纯逻辑（解析、校验、CDP 参数映射）
├── config/      Options / Defaults / WithXxx
├── cdpkit/      CDP 上下文纪律与返回值解码
└── errs/        哨兵错误（唯一定义处）
session/         纯 HTTP：Session / Jar / Response（不 import chromium）
smoke/           端到端测试（//go:build smoke，依赖真实 Chrome）
```

依赖方向严格单向、无环（可用 `go list -deps ./chromium` 验证）：`errs/cdpkit/config → chrome/cookie → network → page → browser → profile → 门面`。

---

## 1. 核心抽象（先建立正确的心理模型）

| 类型 | 是什么 | 关键约束 |
|---|---|---|
| `Browser` | 一个 Chrome 进程 + 一条共享的 chromedp 连接 | `Close()` 会杀进程树并释放数据目录锁 |
| `Tab` | 一个被托管的标签页（= 一个 CDP target） | **`Tab.Ctx` 承载 target 路由**，所有操作都要用它派生 ctx |
| `Element` | **定位器，不是 DOM 节点** | 只存选择器，每次操作都重新查询（源：`page/element.go` 类型注释） |
| `FrameElement` | iframe 内的元素，镜像 `Element` | 走 JS 求值（不经 CDP DOM 域），**故没有 `SendKeys` / `Attribute` / `Eval` / `WaitVisible`** |
| `Frame` | 页面内一个 iframe | 建在 isolated world 里，跨域 iframe 也能读写；导航后失效（`ErrFrameDetached`） |
| `Selector` | 「怎么定位一个元素」 | 用 `CSS/XPath/ID/JS` 构造，别自己构造结构体 |
| `WaitBuilder` | 链式等待条件 | 必须指定条件后再 `Do(ctx)`，否则 `ErrWaitConditionUnset` |
| `Listener` | Network 域被动监听，产出 `*Record` | `Start(ctx)` 的 ctx 必须派生自 `tab.Ctx` |
| `BrowserContext` | 单浏览器内的隔离上下文（类无痕） | 上下文之间 Cookie / 存储完全隔离 |
| `ProfileManager` | 多进程档案隔离（每档案一个 Chrome） | `CloseAll()` 是**终态** |
| `session.Session` | 纯 HTTP 会话，带 Cookie Jar | 与 `chromium` 互不 import，靠 `CookieSource` 对接 |

**`Element` 不缓存节点**这一点决定了很多写法：SPA 重渲染不会让它失效（每次重新查），但也因此别指望「拿到句柄就等于锁定了那个节点」。

---

## 2. 最重要的一条硬约束：ctx 必须从 `Tab.Ctx` 派生

所有 I/O 方法的第一个参数都是 `ctx`，**凡是作用在某个标签页上的调用，这个 ctx 必须派生自那个 tab 的 `Ctx`**：

```go
ctx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
defer cancel()
if err := tab.Navigate(ctx, url); err != nil { ... }
```

原因：chromedp 把 target 路由信息挂在 ctx 链上。传裸 `context.Background()` 会让 chromedp 另起一个临时浏览器，命令落不到目标标签页。库内部有兜底（`page/tab.go` 的 `safeCtx` 会检测裸 ctx 并回退到 `t.Ctx`），并为此定义了 `errs.ErrInvalidContext`，但**不要依赖兜底**。

**同一条约束延伸到 Listener**：`listener.Start(ctx)` 的 ctx 也要派生自 `tab.Ctx`，否则命令同样路由不到（库会返回 `ErrInvalidContext`）。

超时规则（`cdpkit.WithDefaultTimeout` + `Tab.applyTimeout`）：
1. 调用方 ctx **有 deadline → 以调用方为准**，库不覆盖；
2. 没有 deadline → 套用 `WithDefaultTimeout`（默认 30s）；
3. `Tab.SetTimeout(0)` 可关掉内置兜底。

**不要为同一件事再加一套 timeout 机制**，直接传 ctx。

---

## 3. 快速上手（全部是真实 API）

```go
import "github.com/yymm456/go-drission/chromium"

// 一步到位：连上（必要时启动）Chrome 并拿到首个标签页
b, tab, err := chromium.OpenPage(ctx, 0,
    chromium.WithHeadless(true),
    chromium.WithUserDataDir("./profiles/demo"), // 不指定会在 %TEMP% 建目录且不自动清
    chromium.WithDefaultTimeout(20*time.Second),
)
if err != nil { return err }
defer b.Close() // 关浏览器（含进程树与数据目录锁）

// 导航（等 load）
if err := tab.Navigate(ctx, "https://example.com"); err != nil { return err }

// 定位 + 操作（选择器只写一次）
if err := tab.EleCSS("#username").SendKeys(ctx, "alice"); err != nil { return err }
if err := tab.EleID("password").SetValue(ctx, "secret"); err != nil { return err }
if err := tab.EleXPath("//button[text()='登录']").Click(ctx); err != nil { return err }

// 页面信息
title, _ := tab.Title(ctx)
url, _ := tab.CurrentURL(ctx)   // 实时值；tab.URL() 是可能滞后的快照
html, _ := tab.HTML(ctx)
n, _ := tab.EleCSS(".item").Count(ctx)  // 未命中返回 0，不是错误

// 执行 JS
v, _ := tab.Eval(ctx, `document.querySelectorAll('a').length`)

// 等待
if err := tab.Wait().URL("/dashboard").Timeout(10 * time.Second).Do(ctx); err != nil { return err }
```

**多标签与隔离上下文的区别**（别搞混，源：`browser/context.go`）：

```go
t2, _ := b.NewTab(ctx)          // 同窗口/同 Cookie 会话，只是另一个标签页
bc, _ := b.Context(ctx, "acct2") // 隔离上下文：独立 Cookie/存储，可并行多账户
t3, _ := bc.NewTab(ctx)
```

---

## 4. 网络监听

```go
l := tab.Listen("/api/")            // 空串表示全部命中
l.MaxRecords(200)                    // 上限（默认 1000，FIFO 淘汰）；传 0 关闭上限
if err := l.Start(ctx); err != nil { return err }   // ctx 必须派生自 tab.Ctx
defer l.Stop()

// ... 触发请求 ...
l.WaitIdle()                        // 等「已入队」的后台取体任务收尾

for _, rec := range l.Records() {   // 返回深拷贝，监听进行中读也安全
    fmt.Println(rec.URL, rec.Status, rec.ResponseBody)
    fmt.Println(rec.SetCookies)     // 全部 Set-Cookie（见下方「已知不一致」）
}
```

要点（源：`network/listener.go`、`network/events.go`）：
- `WaitIdle()` **只等进入等待这一刻已入队的任务**，等待期间新到的事件不等它 —— 导航后应先 `WaitReady` 再 `WaitIdle`。
- 响应体是**异步**补的（`GetResponseBody`），所以顺序务必是「`WaitIdle` → `Records`」。
- `Record.ResponseHeaders` 合并了 `responseReceived` 与 `responseReceivedExtraInfo` 两个事件；`Set-Cookie` **只出现在后者**，且该事件并非每个响应都有 → `Record.SetCookies` 可能为 `nil`，取前判空。

---

## 5. Cookie 与 Session

### 浏览器侧

```go
tab.EleID("x")                       // 定位
tab.SetCookie(ctx, chromium.Cookie{Name: "sid", Value: "abc", Domain: "example.com", Path: "/"})
list, _ := tab.Cookies(ctx)           // 读当前 Cookie（[]chromium.Cookie）
data, _ := tab.ExportCookiesJSON(ctx) // 导出（可交给 session）
n, _    := tab.ImportCookiesJSON(ctx, data)
```

### 纯 HTTP 侧

```go
s := session.New(
    session.WithTimeout(10*time.Second),
    session.WithProxy("http://127.0.0.1:7890"),
    session.WithInsecureTLS(),          // 自签证书站点
    session.WithNoRedirect(),
)
resp, err := s.PostForm(ctx, url, map[string][]string{"user": {"alice"}})
resp.Text(); resp.JSON(&v); resp.OK(); resp.SaveFile("a.png")
s.Cookies()                    // []session.CookieItem
s.SaveCookies("cookies.json")  // 落盘（父目录自动创建，0600）
s.LoadCookies("cookies.json")
```

### Cookie 接力（本项目的核心卖点）

`chromium.CookieSource` 只要 `ExportJSON() ([]byte, error)`，而 `*session.Jar` 天然满足 —— **这是两个包唯一允许的耦合方式，它们不互相 import**：

```go
// HTTP 登录 → 交给浏览器免登录
sess := session.New()
sess.PostForm(ctx, site+"/login", form)          // 纯 HTTP 登录，快
tab, _ := b.NewTab(ctx)
if err := tab.LoginWithCookies(ctx, site+"/home", sess.Jar()); err != nil { ... }
// 也支持：tab.LoginWithCookiesJSON(ctx, url, jsonBytes)

// 反向：浏览器登录 → 交给 HTTP 批量抓
data, _ := tab.ExportCookiesJSON(ctx, site)      // 可带 urls 过滤
jar := session.NewJar()
_ = jar.ImportJSON(data)
s2 := session.New(session.WithJar(jar))
```

`CookiesFor(rawURL)` / `ExportJSONFor(rawURL)` 是按目标 URL 过滤的版本 —— **把登录态交给别的站点时优先用它**，全量导出会把 A 站的 Cookie 塞进 B 站。

---

## 6. 历史导航（Back / Forward）

```go
if err := tab.Back(ctx); err != nil {
    if errors.Is(err, chromium.ErrNoHistoryEntry) {
        // 已经到第一条了，页面保持原样
    } else {
        return err
    }
}
tab.Forward(ctx)
```

**语义（源：`page/tab.go` 的 `navigateHistory`，README「已知边界」）：**
- 到达历史边界返回 `ErrNoHistoryEntry`，**不静默 no-op**（否则调用方分不清「退了」和「没得退」）。
- 超时/取消与 `Navigate` 一致（走同一套 `t.run` → `safeCtx`）。
- **判定「完成」用地址变化，不是 load 事件**：历史导航（从缓存恢复）不会再触发 `Page.loadEventFired`，等 load 会一路卡到 ctx 超时（实测正常历史导航都会被拖满 30s）。所以退到慢页面时它会在地址确认后立即返回，页面是否加载完由调用方按需 `WaitReady`。

**当前三者的完成判据并不一致**（有意保留，别顺手统一）：

| 方法 | 等什么 |
|---|---|
| `Navigate` | 等 load |
| `Back` / `Forward` | 等地址变成目标条目 |
| `Reload` | 只发命令，不等（要等就自己 `WaitReady`） |

---

## 7. 多账户隔离

**两套方案，按隔离强度选**：

```go
// 方案 A：隔离上下文（同一个 Chrome 进程，轻量）
bc, _ := b.Context(ctx, "account_001")
t, _  := bc.NewTab(ctx)
// 可选：为该上下文指定独立代理
bc2, _ := b.Context(ctx, "account_002", chromium.WithContextProxy("http://127.0.0.1:7891"))

// 方案 B：多进程档案（每档案独立 Chrome + 独立数据目录，登录态持久化）
pm := chromium.NewProfileManager("./profiles", 9300, chromium.WithHeadless(true))
defer pm.CloseAll()                     // 终态：之后 Open 返回 ErrProfileClosed
b1, t1, err := pm.Open(ctx, "acct-a")   // 同名复用同一个 Browser（懒加载）
pm.Names(); pm.Get("acct-a")
pm.Close("acct-a")                      // 只关这一个，磁盘目录保留
```

隔离上下文之间、档案之间的 Cookie 与存储完全隔离（smoke 有用例覆盖）。档案用 OS 级排他锁保护数据目录，被别的实例占用时 `Open` 会立刻返回明确错误。

---

## 8. 错误处理

**全部哨兵错误都定义在 `chromium/errs`，由门面 `chromium.ErrXxx` 转发同一个值**，所以「实现包返回、调用方判断」之间 `errors.Is` 照旧成立。判断一律用 `errors.Is`，**不要比对错误字符串**。

常遇到的（源：`chromium/errs/errors.go`）：

| 错误 | 何时出现 |
|---|---|
| `ErrInvalidContext` | 传了裸 ctx（未从 `tab.Ctx` 派生），命令无法路由 |
| `ErrClosed` / `ErrNotConnected` | Browser 已关闭 / 没 `Connect` 就用 |
| `ErrNoTab` | 没有任何标签页可返回 |
| `ErrNoHistoryEntry` | `Back`/`Forward` 已到历史边界 |
| `ErrElementNotFound` / `ErrSelectorRequired` | 选择器没命中 / 选择器为空 |
| `ErrFrameNotFound` / `ErrFrameDetached` | 找不到 iframe / iframe 随导航失效 |
| `ErrListenerStarted` | 重复 `Start` |
| `ErrWaitConditionUnset` | `WaitBuilder` 没设条件就 `Do` |
| `ErrChromeNotFound` | 本机没找到浏览器（**测试里应据此 skip，而不是 fail**） |
| `ErrBrowserMismatch` | 端口上的 Chrome 不是期望的那个（用户数据目录对不上） |
| `ErrInvalidCookie(+JSON)` | Cookie 参数不合法 / JSON 不是数组 |
| `ErrEmptyURL` | 地址为空或缺协议头/主机名 |

**行为约定**：
- 该报错就报错，**不静默成功** —— 例如 `SetCookie` 对 `SameSite=None` 但没 `Secure` 的条目会拒绝并报错，而不是假装写入。
- 超时 / 取消统一由 `ctx` 表达（`context.DeadlineExceeded` / `Canceled`），库不另造超时错误类型。
- 等待类方法在超时错误里会保留「期间最后一次底层错误」（两个 `%w`），排查时记得看完整链条。

---

## 9. 测试

分层与完整基线见仓库根的 **`TESTING.md`**。日常命令：

```bash
gofmt -l .                                    # 必须为空
go vet ./...
go build ./...
go test -count=1 ./...                        # 单元测试，约 3 秒，不依赖浏览器
go test -count=1 -race ./...                  # 并发改动必开
go test -tags smoke -count=1 -timeout 20m ./smoke/...   # 端到端，真实 Chrome，约 4 分钟
golangci-lint run ./...
```

长稳定性（轮次由环境变量控制）：

```bash
GO_DRISSION_STABILITY_ROUNDS=100 go test -tags smoke -run TestStability -timeout 40m ./smoke/...
```

**加新功能时必须做的**：
1. 逻辑放单元测试（不依赖浏览器）；只有真实浏览器能回答的才放 `smoke/`；
2. **给默认值和公开契约加"反向用例"** —— 例如 `AntiDetect` 默认是 `false`，`smoke.TestAntiDetectDisabledByDefault` 就钉住了它。默认值改错了不会有编译错误，只能靠反向断言发现；
3. 涉及并发/关闭路径的，跑 `-race`；
4. 新增用例先做负向验证（故意改坏 → 必须转红），否则「一直绿」可能只是它没在工作。

### API 冻结（改公开 API 前必读）

`chromium/api.go` 是门面，`type Tab = page.Tab` 这类别名会让 `go doc` **看不见字段与方法**，所以仓库用两道编译期/运行期闸门兜住（源：`chromium/api_test.go`）：

- **方法签名**：门面包 `_test.go` 里的**方法表达式 var 块**，如
  `_ func(*Tab, context.Context) error = (*Tab).Back`
  —— 签名一改就编译不过。**新增导出方法必须往这里补一行**。
- **结构体字段**：反射用例（`TestCookieJSONTagsAreFrozen` / `TestRecordFieldsAreFrozen` 等）。新增公开字段会**故意**让它们失败，提示「公开契约变了」——更新 `want` 表即可，不要绕过。

---

## 10. DO / DON'T

**DO**
- 改代码前先读对应实现（`chromium/page/*.go`、`chromium/browser/*.go` …），本仓库的注释里写满了「为什么」和 BUG 编号背景。
- 复用现有 API 与抽象；定位一律用 `Ele` 系列（`EleCSS/EleID/EleXPath/EleJS`），不要写「操作 + 选择器」形式。
- 所有 I/O 传 ctx，且派生自目标 `tab.Ctx`；超时用 ctx 表达。
- 用 `errors.Is` 判哨兵错误。
- 需要的功能如果属于别的层（如 HTTP 用 `session`，监听用 `Listener`），就用那一层，别在错误的层硬凑。
- 改完跑 `gofmt` / `go vet` / `go test -race`，涉及浏览器行为的补 smoke 用例。

**DON'T**
- **不要发明 API**。不确定某个方法是否存在，就去源码里 grep；本仓库没有 `Tab.WaitLoad()`、没有 Python DrissionPage 的 `ele()`、也没有 `Browser.Back()`（`Back` 在 `Tab` 上）。
- **不要绕过本库直接用 chromedp**。需要新 CDP 能力时，加到 `page`/`browser` 层并走 `Tab.run`，不要在业务代码里 `chromedp.Run(tab.Ctx, ...)`（会丢掉本库的 ctx 校正与默认超时）。
- **不要缓存 DOM 节点**。`Element` 就是不缓存节点的设计，需要反复操作就复用 `*Element`（它每次重新查），不要自己去拿 nodeID 存着。
- **不要在业务代码里 `defer` 掉 `Browser.Close()` 之后就以为万事大吉** —— 它同时负责杀 Chrome 进程与释放数据目录锁；`ProfileManager` 场景要 `Close(name)` / `CloseAll()`。
- **不要随手 `cancel` 一个 tab 的上下文当成"释放会话"**：`Tab.cancel()` 等价于 `CloseTarget`，会真的把页面关掉。
- **不要为了「更优雅」做大范围重构**。当前架构（含 `page` 是一个大包）是有意为之：文件大不等于要拆包，同包内拆文件可以，拆包不行。
- **不要静默吞错误**，也不要为了测试通过去改产品语义 —— 先判断是「实现错」还是「测试预期错」。
- 测试里**不要用 0 或 9222 当调试端口**：`OpenPage(ctx, 0, ...)` 在自动分配失败时会退回 **9222**（用户最可能自己开着 Chrome 调试的端口），接管上去就不再是干净实例了。smoke 统一走 `nextFreePort()`（40000+ 段）。

---

## 11. 扩展指南（加新功能时的顺序）

1. **先找现有 abstraction**：这个能力属于 `Browser` / `Tab` / `Element` / `Frame` / `Listener` / `Profile` / `Session` 哪一层？
2. **确认它是不是已经存在**（grep 一遍，别猜）。
3. **优先复用**：例如超时用 `cdpkit.WithDefaultTimeout`，轮询等待复用 `pollWait` 骨架，别各写一套。
4. **保持 API 风格**：方法第一个参数是 ctx、返回 `error`、定位与操作分离、不改已有签名。
5. **实现放在正确的包**，需要新 CDP 调用就走 `Tab.run` / `Browser` 的 bounded ctx 路径。
6. **补测试**：单元 + smoke（若依赖浏览器）+ 反向用例（若改了默认值）+ `-race`。
7. **更新文档**：`README.md`（若是对外行为）、`CHANGELOG.md`（必做）、`TESTING.md`（若测试方式变了）；公开 API 变化要同步 `chromium/api_test.go` 的冻结块。
8. **避免不必要的 breaking change**：确实需要时，先列清「当前 API / 问题 / 建议 / 兼容影响 / 理由」，再动手。

---

## 12. 需要注意的不一致

- **`Reload` 不等 load**，与 `Navigate` / `Back` / `Forward` 不一致（见 §6 的表格）。这是有意保留的现状，改动前先确认。
- `Navigate` / `Back` / `Forward` 判定「完成」的依据各不相同（等 load vs 等地址），见 §6。

已对齐、不必再担心的：

- `chromium.Cookie` 与 `session.CookieItem` 的字段**已完全对齐**（两侧均有
  `Partitioned` / `PartitionKey`，分区 Cookie CHIPS 可无损跨包接力）。
  两边的 json tag 逐字相同（含 `,omitempty`），由 `chromium/api_test.go` 的
  `TestCookieJSONTagsAreFrozen` 与 `session` 侧的对应用例一起钉住 —— **改任一侧的 tag 都会让测试红**。
  分区 Cookie 还要求 `Secure`，且 `Partitioned=true` 时必须给 `PartitionKey`（否则注入前会被
  `ValidateCookie` 拦成 `ErrInvalidCookie`，不会静默降级成普通 Cookie）。

---

## 13. 快速索引：想做的事 → 看哪个文件

| 想了解 | 看 |
|---|---|
| 门面到底暴露了什么 | `chromium/api.go` |
| Tab 的全部方法与超时规则 | `chromium/page/tab.go` |
| Element 的定位与操作、为什么每次都重新查 | `chromium/page/element.go` |
| iframe / isolated world / 为什么 FrameElement 方法少 | `chromium/page/frame.go`、`frame_element.go` |
| 选择器如何变成 CDP 调用 | `chromium/page/selector.go` |
| 等待条件的实现与错误分诊 | `chromium/page/wait.go` |
| Cookie 校验规则与 CDP 参数映射 | `chromium/cookie/cookie.go` |
| Browser 生命周期、锁序、target 同步 | `chromium/browser/browser.go`、`targets.go` |
| 隔离上下文 | `chromium/browser/context.go` |
| 档案与排他锁 | `chromium/profile/profile.go`、`chromium/chrome/lock_*.go` |
| 监听与 Record | `chromium/network/listener.go`、`events.go`、`record.go` |
| 哨兵错误 | `chromium/errs/errors.go` |
| 公开契约冻结 | `chromium/api_test.go` |
| 测试分层与稳定基线 | `TESTING.md` |
| 变更历史 | `CHANGELOG.md` |
