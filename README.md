# go-drission

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.0-00ADD8.svg)](go.mod)

用 **Go 标准写法**实现的浏览器自动化库，功能参考 Python 的 DrissionPage，但遵循 Go 的惯用法：
`context.Context` 贯穿所有 I/O、错误如实返回不吞掉、超时与取消由调用方掌控。

底层基于 [chromedp](https://github.com/chromedp/chromedp)（Chrome DevTools Protocol）。

---

## 安装

需要 **Go 1.26**。本项目使用该版本。

```bash
go get github.com/yymm456/go-drission@latest
```

---

## 目录结构

```
go-drission/
├── go.mod
├── .golangci.yml  静态检查配置（golangci-lint v2）
├── chromium/              库主体：浏览器自动化
│   ├── browser.go       Browser：连接、标签页管理、OpenPage
│   ├── context.go       BrowserContext：单浏览器内多账户隔离上下文
│   ├── profile.go       ProfileManager：多账户命名档案（独立进程）隔离
│   ├── profile_marker.go 档案标记文件：固定端口接管前校验「这浏览器是不是我的」
│   ├── tab.go           Tab：导航、截图、Eval、标签页与窗口管理
│   ├── element.go       Element：查询与操作分离（Ele*/EleCSS/... → Click/SendKeys/...）
│   ├── wait.go          WaitBuilder：链式等待（页面级 tab.Wait() / 元素级 el.Wait()）
│   ├── launch.go        端口探测、Chrome 启动
│   ├── process.go       子进程管理（Chrome PID、优雅退出）
│   ├── chrome_path.go   浏览器可执行文件自动发现（分平台）
│   ├── anti_detect.go   反自动化检测：启动参数 + 页面注入脚本
│   ├── selector.go      Selector：CSS / XPath / ID / JS 四种构造器
│   ├── frame.go         Frame：iframe 定位与跨域操作
│   ├── errors.go        哨兵错误（ErrClosed / ErrNotConnected / ErrBrowserMismatch 等）
│   ├── targets.go       HTTP /json 查询与标签页同步
│   ├── options.go       函数式配置项 WithXxx（含 WithLogger）
│   ├── cookies.go       Cookie 注入 / 读取 / 导入导出
│   ├── listen.go        Network 域被动监听（Listener / Record）
│   ├── lock_windows.go / lock_other.go  跨进程 user-data-dir 排他锁
│   ├── port.go          空闲端口分配
│   └── util.go          writeFile、contains、超时兜底、CDP 值解码助手
├── session/               纯 HTTP 模式（对标 SessionPage）
│   ├── session.go      Session：net/http 封装，Get/Post/PostForm/PostJSON/Do
│   ├── jar.go          自研 CookieJar：并发安全，全量导出 / 导入，支持 CHIPS 分区 Cookie
│   ├── jar_file.go     Cookie 存盘 / 读盘，Session 的 SaveCookies / LoadCookies
│   ├── response.go     Response：body 已缓存（带大小上限），可反复读 Bytes/Text/JSON
│   └── errors.go       哨兵错误（ErrBodyTooLarge 等）
├── cmd/demo/              本地综合调试入口（`go run ./cmd/demo`，需真实站点与账户）
├── smoke/                 端到端冒烟测试（`go test -tags smoke ./smoke/...`，需真实 Chrome）
└── example/               可运行示例（go run ./example/xxx）
    ├── basic/             连接、导航、读取、截图
    ├── wait/              链式 Wait Builder
    ├── selector/          Element 定位入口 + 四种选择器 + Shadow DOM 穿透
    ├── iframe/            iframe 定位、跨域读写、点击与填表
    ├── session/           纯 HTTP 请求 / Cookie 复用 / 存盘
    ├── screenshot/        页面截图
    ├── listen/            网络监听
    ├── context/           单浏览器多上下文 Cookie 隔离（含同窗口多标签共享会话证明）
    ├── profile/           多进程浏览器隔离（ProfileManager）
    ├── concurrent_tabs/   同浏览器多标签并发任务 + 标签切换
    ├── cookie/            Cookie 导入导出 / 注入
    └── cookie_relay/      免登录：Session 纯 HTTP 登录 → 浏览器直接接管
```

> 模块根目录**没有** `package main`：`go install github.com/yymm456/go-drission@latest` 不会
> 装出一个会去真实站点登录的 demo 程序。调试入口单独放在 `cmd/demo/`。


---

## 选择器 Selector

所有定位类 API 收的都不是裸字符串，而是 `chromium.Selector`。四个构造器覆盖四种定位方式：

| 构造器 | 底层 | 说明 |
|---|---|---|
| `chromium.CSS(sel)` | `chromedp.ByQuery` | CSS 选择器，默认方式 |
| `chromium.XPath(expr)` | `chromedp.BySearch` | XPath 表达式 |
| `chromium.ID(id)` | `chromedp.ByID` | 按元素 id |
| `chromium.JS(expr)` | `chromedp.ByJSPath` | **一段返回元素的 JS 表达式**（不是 DOM 树路径） |

每种选择器都有对应的 `Tab` 快捷入口，返回一个 `*Element`（详见下一节）：

| 入口 | 等价于 |
|---|---|
| `tab.EleCSS("#submit")` | `tab.Ele(chromium.CSS("#submit"))` |
| `tab.EleID("kw")` | `tab.Ele(chromium.ID("kw"))` |
| `tab.EleXPath("//a")` | `tab.Ele(chromium.XPath("//a"))` |
| `tab.EleJS("document.querySelector(...)")` | `tab.Ele(chromium.JS(...))` |
| `tab.Ele(sel)` | 显式传入一个 `chromium.Selector` |

```go
tab.EleCSS("#submit").Click(ctx)
tab.EleXPath("//div[@class='item']/span").Text(ctx)
tab.EleID("kw").SetValue(ctx, "golang")
```

`JS()` 是穿透 Shadow DOM、取匿名嵌套节点这类「选择器表达不了」场景的逃生口，
表达式的求值结果必须是一个 DOM 元素：

```go
// 穿透 shadow root 取内部按钮
tab.EleJS(`document.querySelector('my-widget').shadowRoot.querySelector('button')`).Click(ctx)
```

> `JS()` 的表达式会被原样交给 `Runtime.evaluate` 执行（**不做任何转义**），只能传可信内容，
> 不要把未净化的外部输入拼进去；而且它只返回**单个元素**，要取多个请改用 CSS + 遍历。
>
> 传空选择器（`tab.EleCSS("")`，或只有空白的 `tab.EleCSS("   ")`）会返回 `ErrSelectorRequired`，
> 既不会静默定位到 `<html>`，也不会把底层那句 `'   ' is not a valid selector` 抛给你。
> 找不到元素统一返回 `ErrElementNotFound`（用 `errors.Is` 判断），不会抛裸的
> `context deadline exceeded`。

---

## 元素 Element：查询与操作分离

`Element` 是「一个待操作的页面元素」= 选择器 + 所属标签页。设计上把「怎么找」和「做什么」
拆成两步，选择器只出现一次，读起来就是「找到什么 → 对它做什么」：

```go
tab.EleID("username").SendKeys(ctx, "xxx")
tab.EleID("password").SendKeys(ctx, "xxx")
tab.EleCSS(".p-button-label").Click(ctx)
title, _ := tab.EleCSS("h1").Text(ctx)
```

### Element 操作

| 方法 | 说明 |
|---|---|
| `Click(ctx) error` | 点击（真实鼠标事件，含可见性检查） |
| `ClickJS(ctx) error` | 用 JS 触发 `el.click()`，绕过可见性检查 |
| `SendKeys(ctx, text) error` | 输入文本（先清空），走真实键盘事件 |
| `SetValue(ctx, value) error` | 直接设值并触发 input/change，适配 React/Vue |
| `Text(ctx) (string, error)` | 元素文本（innerText） |
| `Attribute(ctx, name) (string, error)` | 属性值，如 `Attribute(ctx, "href")` |
| `Count(ctx) (int, error)` | 该选择器匹配到的元素数量（0 不是错误） |
| `Eval(ctx, fn) (interface{}, error)` | 在该元素上执行 JS，函数体内 `this` 即该元素 |
| `WaitVisible(ctx) error` | 等待元素可见 |
| `WaitText(ctx, substr) error` | 轮询等待元素文本包含 substr |
| `Wait() *WaitBuilder` | 元素级链式等待，见下文 |
| `Selector() / Tab() / String()` | 元信息：选择器 / 所属标签页 / 可读描述 |

### 不缓存 DOM 节点

`Element` 只保存选择器，**每次操作都重新查询一遍**。这不是偷懒，是 SPA 场景下的硬性要求：
Vue/React 重渲染会把旧节点整批换成新节点，缓存的 nodeID 会变成 detached 节点——此时
读属性拿到的是旧值，点击会静默无效，而且完全不报错。

所以复用应当发生在 `*Element` 这一层（它就是选择器 + tab，很轻），而不是去缓存 DOM 节点：

```go
save := tab.EleCSS("#save")   // 存 Element，不是存 node
for i := 0; i < 3; i++ {
    save.Click(ctx)           // 每次都重新查，SPA 重建也不怕
}
```

### Eval 的三种写法

`Eval` 会自动识别传入的 JS 片段，省掉手写 `function` 包装的噪音：

```go
el.Eval(ctx, "this.innerText")                    // 单表达式 → return (this.innerText)
el.Eval(ctx, "return this.dataset.id")            // 函数体   → function(){ ... }
el.Eval(ctx, `function(){ return this.tagName }`) // 完整函数 → 原样使用
```

---

## 核心约定：ctx 必须从 `tab.Ctx` 派生 ⚠️

chromedp 有硬约束——**传给 Tab 方法的 `ctx` 必须派生自 `tab.Ctx`**，否则丢失 target
路由信息，会报 `invalid context`。超时 / 取消通过派生 ctx 控制：

```go
navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
defer cancel()
if err := tab.Navigate(navCtx, "https://example.com"); err != nil {
    log.Fatal(err)
}
```

- 想要超时：`context.WithTimeout(tab.Ctx, d)`
- 想要可取消：`context.WithCancel(tab.Ctx)`
- 忘了设超时也不会挂死：库内置 **30s 默认超时**，只在传入的 ctx **没有 deadline** 时兜底生效。
  调用方自己设了 deadline，就完全听调用方的，不会被覆盖。
  全局调整用 `WithDefaultTimeout(d)`，单个标签页用 `tab.SetTimeout(d)`，传 `0` 关闭。
- `Browser.Close()` 是 teardown 操作，遵循 `io.Closer` 惯例，不接受 ctx。

> 误传「裸 context」也不会出错：`Tab` 方法检测到 ctx 不含路由信息时会自动回退到该标签页自身的
> 会话上下文，而不是让命令落到 chromedp 临时拉起的浏览器上。
> 但 `Listener.Start` 要求 ctx 必须携带路由（它没法替你猜目标标签页），传错会返回
> `ErrInvalidContext` 而不是 panic。

---

## 快速开始

```go
package main

import (
    "context"
    "log"
    "time"
    "github.com/yymm456/go-drission/chromium"
)

func main() {
    ctx := context.Background()

    // port 传 0 = 随机端口；传具体值 = 固定端口。
    // 端口活着就直接连，没活就自动启动 Chrome。
    // 接管已有浏览器前会校验归属：一旦显式指定了 userDataDir，而端口上那个浏览器
    // 不是从该目录启动的，就返回 ErrBrowserMismatch，不会静默接管别人的登录态。
    browser, tab, err := chromium.OpenPage(ctx, 9222,
        chromium.WithConnectTimeout(5*time.Second),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer browser.Close()

    navCtx, cancel := context.WithTimeout(tab.Ctx, 30*time.Second)
    defer cancel()
    if err := tab.Navigate(navCtx, "https://www.bilibili.com"); err != nil {
        log.Fatal(err)
    }

    readyCtx, cancel2 := context.WithTimeout(tab.Ctx, 15*time.Second)
    defer cancel2()
    _ = tab.WaitReady(readyCtx)

    title, err := tab.Title(readyCtx)
    if err != nil {
        log.Fatal(err)
    }
    log.Println("页面标题:", title)
}
```

---

## 示例

`example/` 下每个子目录都是一个可直接运行的独立示例（需本机有 Chrome）：

| 示例 | 运行 | 演示内容 |
|---|---|---|
| basic | `go run ./example/basic` | 连接、导航、等待就绪、读标题/地址、截图 |
| wait | `go run ./example/wait` | 链式 Wait Builder 的各种条件与校验 |
| selector | `go run ./example/selector` | `Element` 的四种定位入口：EleCSS/EleXPath/EleID/EleJS，含 Shadow DOM 穿透 |
| iframe | `go run ./example/iframe` | iframe 枚举、按选择器/URL/name 定位、跨域读写与填表 |
| session | `go run ./example/session` | 纯 HTTP 模式：GET/POST、Cookie 自动回传、跨 Session 复用、存盘 |
| screenshot | `go run ./example/screenshot` | 视口截图，输出到 `screenshots/` |
| listen | `go run ./example/listen` | 被动监听网络请求/响应 |
| context | `go run ./example/context` | 单浏览器多上下文 Cookie 隔离：上下文间 Cookie 互不可见、同上下文多标签共享会话、窗口语义证明 |
| profile | `go run ./example/profile` | 多进程浏览器隔离：多档案并发任务、PID 证明、同名复用与关闭 |
| concurrent_tabs | `go run ./example/concurrent_tabs` | 同浏览器多标签页 goroutine 并发执行任务（互不干扰）+ BringToFront 切换 |
| cookie | `go run ./example/cookie` | Cookie 注入 / 读取 / 导出 / 导入 |
| cookie_relay | `go run ./example/cookie_relay` | **免登录**：Session 纯 HTTP 登录 → 浏览器直接接管（含隔离上下文与反向接力） |

> 示例运行产生的截图、`cookies.json`、`profiles/` 等生成物已在 `.gitignore` 中忽略。

---

## API 一览

### 顶层

| 函数 | 说明 |
|---|---|
| `OpenPage(ctx, port, opts...) (*Browser, *Tab, error)` | 连接（或启动）浏览器并返回一个可用标签页 |
| `NewBrowser(port, opts...) *Browser` | 仅创建 Browser，需自行 `Connect` |

### Browser

| 方法 | 说明 |
|---|---|
| `Connect(ctx) error` | 探测端口：活着就连，空闲就启动 Chrome。接管前校验端口上的浏览器是否属于本 Browser 的 userDataDir，不符返回 `ErrBrowserMismatch`（见「固定端口接管校验」）。ctx 控制整个握手阶段（含等待端口就绪），ctx 无 deadline 时用 `WithConnectTimeout` |
| `Port() int` | 当前调试端口 |
| `Tabs(ctx) ([]*Tab, error)` | 所有 page 类型标签页 |
| `NewTab(ctx) (*Tab, error)` | 新建标签页 |
| `GetTab(ctx, index) (*Tab, error)` | 按索引取标签页 |
| `GetTabByURL(ctx, substr) (*Tab, error)` | 按 URL 子串查找 |
| `LatestTab(ctx) (*Tab, error)` | 最后打开的标签页 |
| `CloseTab(ctx, tab)` | 关闭指定标签页（真正关 Chrome 里的 target） |
| `Close()` | 断开连接；若是自启的 Chrome 则杀进程 |
| `Context(ctx, name, opts...) (*BrowserContext, error)` | 按名字取/建单浏览器内的隔离上下文（多账户） |
| `Contexts() []string` | 已创建的隔离上下文名字 |
| `PID() int` | 自启 Chrome 的主进程 PID（接管已有 Chrome 时为 0） |

### Tab —— 定位入口（返回 `*Element`）

| 方法 | 说明 |
|---|---|
| `Ele(sel Selector) *Element` | 用显式选择器定位，请配合 `chromium.CSS/XPath/ID/JS` 构造器 |
| `EleCSS(sel string) *Element` | CSS 选择器定位 |
| `EleID(id string) *Element` | 按元素 id 定位（不需要写 `#` 前缀） |
| `EleXPath(expr string) *Element` | XPath 表达式定位 |
| `EleJS(expr string) *Element` | JS 表达式定位（Shadow DOM 穿透） |

真正的操作在返回的 `*Element` 上，见上文「元素 Element：查询与操作分离」。

### Tab —— 导航 / 读取

| 方法 | 说明 |
|---|---|
| `Navigate(ctx, url) error` | 导航并等待 load，超时/失败如实返回 error |
| `Reload(ctx) error` | 重新加载 |
| `Title(ctx) (string, error)` | 页面标题 |
| `CurrentURL(ctx) (string, error)` | 当前地址（走一次 CDP，实时值） |
| `URL() string` | 最近一次同步到的地址快照，并发安全；可能滞后，实时值请用 `CurrentURL` |
| `HTML(ctx) (string, error)` | 完整 HTML |
| `Eval(ctx, js) (interface{}, error)` | 执行 JS，按 JSON 语义返回原始值 |
| `Screenshot(ctx, path) error` | 截图保存 |
| `Frames(ctx) ([]*Frame, error)` | 页面内所有 iframe（含嵌套），见「iframe」 |
| `Frame(ctx, sel) (*Frame, error)` | 按选择器定位 iframe 元素 |
| `FrameByURL(ctx, substr) (*Frame, error)` | 按 URL 子串定位 iframe |
| `FrameByName(ctx, name) (*Frame, error)` | 按 name 属性定位 iframe |

### Tab —— 等待（超时统一由 ctx 控制）

| 方法 | 说明 |
|---|---|
| `WaitReady(ctx) error` | 等待 body 就绪 |
| `WaitURL(ctx, substr) error` | 轮询等待 URL 包含子串 |
| `Wait() *WaitBuilder` | 页面级链式等待（URL / Ready）；元素级条件走 `el.Wait()` |

元素级的 `WaitVisible` / `WaitText` / `WaitCount` 已移到 `Element` 上（语义不变），
见上文「元素 Element：查询与操作分离」。

#### 链式 Wait Builder

等待有两个入口，对应两种作用域：**元素级用 `el.Wait()`，页面级用 `tab.Wait()`**。

```go
// 等元素可见，最多 10s（Timeout 在传入 ctx 之上再派生超时）
if err := tab.EleCSS("#submit").Wait().Visible().Timeout(10 * time.Second).Do(ctx); err != nil {
    log.Fatal(err)
}

// 等列表项 >= 5 个
tab.EleCSS("li.item").Wait().Count(5).Do(ctx)

// 等状态文本包含「完成」
tab.EleID("status").Wait().Text("完成").Do(ctx)

// 用 XPath 等待
tab.EleXPath("//button[text()='提交']").Wait().Visible().Do(ctx)

// 等 URL 跳转到含 dashboard 的地址（页面级，无需元素）
tab.Wait().URL("dashboard").Timeout(15 * time.Second).Do(ctx)

// 等页面就绪（页面级）
tab.Wait().Ready().Do(ctx)
```

| 方法 | 说明 |
|---|---|
| `Wait() *WaitBuilder` | 开始构建一个等待（`Tab.Wait()` 页面级 / `Element.Wait()` 元素级） |
| `Element(el *Element)` | 从 Tab 侧补上目标元素，等价于 `el.Wait()`（逃生口，日常用不到） |
| `Visible()` | 元素可见（元素级） |
| `Present()` | 元素存在于 DOM，数量 ≥ 1（元素级） |
| `Text(substr)` | 元素文本包含 substr（元素级） |
| `Count(n)` | 匹配元素数量 ≥ n（元素级） |
| `URL(substr)` | 当前地址包含 substr（页面级） |
| `Ready()` | 页面 body 就绪（页面级） |
| `Timeout(d)` | 设定整体超时；不调用则沿用 ctx 的 deadline |
| `Do(ctx) error` | 执行等待，超时/不满足如实返回 error |

> 元素级条件必须先有元素：在 `tab.Wait()` 上直接调 `Visible()` / `Present()` / `Text()` / `Count()`，
> `Do` 会返回 `ErrSelectorRequired`，而不是含糊地超时。

### Tab —— 窗口 / 其它

| 方法 | 说明 |
|---|---|
| `BringToFront(ctx) error` | 标签页激活置前（后台窗口节流会丢输入，交互前先调用） |
| `WindowID(ctx) (int64, error)` | 标签页所属 OS 窗口编号（同窗口多标签验证） |
| `SetTimeout(d)` | 调整该标签页的内置默认超时；只对「调用方未设 deadline」的调用生效，传 0 关闭 |
| `Listen(pattern) *Listener` | 网络监听，见「网络监听 Listener」 |

> 点击 / 输入 / 设值 / 读文本属性这些操作全部落在 `Element` 上：`tab.EleCSS("#btn").Click(ctx)`。
> `Tab` 上不再有 `Click(ctx, sel)`、`SendKeys(ctx, sel, text)` 这种「选择器随调用传入」的形态。

### Tab —— Cookie

| 方法 | 说明 |
|---|---|
| `SetCookie(ctx, cookie) error` | 注入单个 Cookie（缺 `name` / `domain` 时返回 `ErrInvalidCookie`） |
| `SetCookies(ctx, cookies) error` | 批量注入 |
| `GetCookies(ctx, urls...) ([]*network.Cookie, error)` | 读取 CDP 原生 Cookie |
| `Cookies(ctx, urls...) ([]Cookie, error)` | 读取 Cookie（已转成可序列化结构） |
| `ExportCookiesJSON(ctx, urls...) ([]byte, error)` | 导出为 JSON 字节 |
| `ExportCookies(ctx, path, urls...) error` | 导出到文件（自动建父目录，权限 0600） |
| `ImportCookies(ctx, path) error` | 从文件导入 |
| `ImportCookiesJSON(ctx, data) (int, error)` | 从 JSON 字节导入，返回条数 |
| `ImportCookiesSource(ctx, src) (int, error)` | 从任意 `CookieSource` 导入（见下文「免登录」） |
| `LoginWithCookies(ctx, url, src) error` | **免登录**：注入 Cookie 后直接打开目标页 |
| `LoginWithCookiesJSON(ctx, url, data) error` | 同上，直接吃 JSON 字节 |

> 作用域：Cookie 写进当前标签页所属的**隔离上下文**，不会漏到其它上下文或默认上下文。
> 时序：`SetCookies` 在 `about:blank` 上就能生效，因此 `LoginWithCookies` 采用
> 「先注入再导航」，目标页面的**首个请求**就已经带上登录态。

### 配置项 Option

| 函数 | 说明 |
|---|---|
| `WithChromePath(path)` | 浏览器可执行文件路径；不指定时自动在本机查找（见「浏览器自动发现」） |
| `WithUserDataDir(dir)` | 用户数据目录（持久化登录态）。显式指定后，接管固定端口时会校验归属（见「固定端口接管校验」） |
| `WithTrustExistingBrowser()` | 关闭固定端口的接管校验，无条件接管端口上的浏览器 |
| `WithUserAgent(ua)` | User-Agent |
| `WithProxy(proxy)` | 代理，如 `http://127.0.0.1:7890` |
| `WithHeadless(bool)` | 无头模式 |
| `WithWindowSize(size)` | 窗口大小，如 `1920,1080` |
| `WithConnectTimeout(d)` | 连接握手超时（ctx 无 deadline 时的默认值，默认 10s） |
| `WithDefaultTimeout(d)` | Tab 操作的默认超时（默认 30s，传 0 关闭）；仅在调用方 ctx 无 deadline 时兜底 |
| `WithAntiDetect(bool)` | 反自动化检测（默认开），见「反检测」 |
| `WithLang(lang)` | 浏览器语言 `--lang`（默认 `zh-CN`），传空则不添加 |
| `WithFlag(name, value)` | 自定义 Chrome 启动参数 |
| `WithLogger(l)` | 库内部日志（`*slog.Logger`）；默认静默，传入即输出连接/启动等信息 |

---

## 网络监听 Listener

被动监听 Network 域事件，捕获匹配 URL 的请求 / 响应完整信息。

```go
// 监听生命周期用独立可取消 ctx（从 tab.Ctx 派生）
listenCtx, stop := context.WithCancel(tab.Ctx)
defer stop()

// pattern 为「子串包含」匹配：URL 含该子串即命中（自动兼容带 query 的请求）
listener := tab.Listen("cm.bilibili.com/cm/api/receive/content/pc")
if err := listener.Start(listenCtx); err != nil {
    log.Fatal(err)
}
defer listener.Stop()

// ... 触发导航 / 交互 ...

listener.WaitIdle() // 等待异步 GetResponseBody 回填完成
for _, rec := range listener.Records() {
    fmt.Println(rec.Method, rec.URL, rec.Status)
    fmt.Println("请求体:", rec.RequestBody)
    fmt.Println("响应体:", rec.ResponseBody)
}
```

### Listener 方法

| 方法 | 说明 |
|---|---|
| `tab.Listen(pattern) *Listener` | 创建监听器，pattern 为子串匹配 |
| `Start(ctx) error` | 启动监听（非阻塞），ctx 控制生命周期 |
| `WaitIdle()` | 等待已入队的后台 CDP 调用完成，但不停止监听 |
| `Records() []*Record` | 返回按到达顺序的当前记录（深拷贝，可安全并发读取） |
| `Len() int` | 当前保留的记录条数 |
| `MaxRecords(n) *Listener` | 设置记录保留上限（默认 `1000`），超出按 FIFO 淘汰；传 `0` 关闭上限 |
| `Clear()` | 丢弃已捕获的全部记录，但不停监听 |
| `Stop()` | 停止监听并等待后台任务收尾 |

`MaxRecords` 返回 `*Listener` 本身，可以链式书写：

```go
listener := tab.Listen("/api/").MaxRecords(200)
```

> 监听常开是常见用法（爬虫挂一整天），记录只增不减会让内存随请求数线性上涨。
> 因此默认保留最近 **1000** 条，超出后淘汰最旧的。需要完整全量就在循环里
> 「`Records()` 取走 → `Clear()` 清空」，或把上限调到你实际能承载的量级。

### Record 字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `RequestID` | `string` | CDP 请求 ID |
| `URL` | `string` | 请求地址 |
| `Method` | `string` | HTTP 方法 |
| `RequestHeaders` | `map[string]string` | 请求头 |
| `RequestBody` | `string` | 请求体（POST/PUT/PATCH） |
| `Status` | `int64` | 响应状态码 |
| `ResponseHeaders` | `map[string]string` | 响应头 |
| `ResponseBody` | `string` | 响应体 |

> **注意**：`WaitIdle()` 只等待「已入队」的后台任务，不会等待未来才发生的请求。
> 导航后应先 `WaitReady` / 适当 `Sleep`，再 `WaitIdle()`，才能抓到异步 XHR / 上报类请求。

---

## 多账户隔离：两种方案

库提供两种多账户隔离方式，按场景自选：

| 维度 | 方案一：`BrowserContext`（单浏览器内隔离） | 方案二：`ProfileManager`（独立进程） |
|---|---|---|
| 原理 | 同一 Chrome 内建多个 CDP BrowserContext | 每个档案一个独立 Chrome 实例 |
| Cookie/存储隔离 | ✅ | ✅ |
| 代理隔离 | ✅ `WithContextProxy` | ✅ `WithProxy` |
| UA 隔离 | ✅ | ✅ |
| 登录态持久化（重启保留） | ❌ 内存态（可配合 Cookie 导入导出） | ✅ 自动落盘 |
| 资源占用 | ✅ 轻（一个进程带 N 账户） | ❌ 重（N 个进程） |
| 崩溃隔离 | ❌ 一崩全崩 | ✅ 互不影响 |
| 适用场景 | 账户多、同机、轻量、临时会话 | 账户少、要持久登录、要崩溃隔离 |

---

## 单浏览器隔离上下文 Context

`browser.Context(name)` 在**同一个 Chrome 实例内**创建隔离上下文（类似无痕窗口，但可并存多个），
每个上下文拥有独立的 Cookie / 存储，并可选独立代理；同名复用。适合账户数量多、追求轻量的场景。

```go
// 账户 A：独立上下文 + 独立代理
ctxA, err := browser.Context(ctx, "account_001",
    chromium.WithContextProxy("http://127.0.0.1:7891"),
)
if err != nil {
    log.Fatal(err)
}
tabA, err := ctxA.NewTab(ctx) // 首个标签页
if err != nil {
    log.Fatal(err)
}
tabA2, _ := ctxA.NewTab(ctx)  // 同一上下文内再开一个，与 tabA 共享登录态

// 账户 B：与 A 完全隔离（不同 Cookie，不同代理）
ctxB, _ := browser.Context(ctx, "account_002",
    chromium.WithContextProxy("http://127.0.0.1:7892"),
)
tabB, _ := ctxB.NewTab(ctx)

// tab 用法与 Browser 上的完全一致（ctx 仍需从 tab.Ctx 派生）
navCtx, cancel := context.WithTimeout(tabA.Ctx, 30*time.Second)
defer cancel()
_ = tabA.Navigate(navCtx, "https://example.com")

// 销毁上下文：关闭其下所有标签页并清除 Cookie/存储
ctxA.Close()
```

| 方法 | 说明 |
|---|---|
| `browser.Context(ctx, name, opts...) (*BrowserContext, error)` | 按名字取/建隔离上下文；同名复用 |
| `browser.Contexts() []string` | 已创建的上下文名字 |
| `bc.NewTab(ctx) (*Tab, error)` | 在该上下文内新建标签页（共享登录态） |
| `bc.Tabs() []*Tab` | 该上下文内当前所有标签页 |
| `bc.CloseTab(ctx, tab)` | 关闭指定标签页（上下文仍存活） |
| `bc.Close()` | 销毁整个上下文：关所有标签页 + 清 Cookie/存储 |
| `bc.Name() string` | 上下文名字 |
| `WithContextProxy(proxy) ContextOption` | 为该上下文设独立代理（仅首次创建生效） |

> **持久化提示**：BrowserContext 是内存态，Chrome 关闭后登录态丢失。若需跨重启保留，
> 可在 `Close` 前用 `tab.ExportCookies(ctx, path)` 导出，下次新建同名上下文后用
> `tab.ImportCookies(ctx, path)` 导入。要“开箱即持久”则直接用下面的 `ProfileManager`。

---

## 多账户 Profile

`ProfileManager` 为每个命名档案分配**独立的用户数据目录 + 独立端口**，因此各账户的
Cookie / 登录态 / 代理 / UA 完全隔离；登录态持久化到磁盘，进程重启后用同名档案打开即可
恢复登录，适合多账户并行运营。

```go
pm := chromium.NewProfileManager("./profiles", 9300,
    chromium.WithHeadless(true),
    chromium.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))),
)
defer pm.CloseAll()

// 账户 A、B 各自独立的浏览器，互不共享登录态
_, tabA, err := pm.Open(ctx, "account_001")
if err != nil {
    log.Fatal(err)
}
_, tabB, err := pm.Open(ctx, "account_002")
if err != nil {
    log.Fatal(err)
}

// 同名再次 Open 直接复用已打开的浏览器
_, tabA2, _ := pm.Open(ctx, "account_001") // tabA2 与 tabA 属于同一浏览器
```

| 方法 | 说明 |
|---|---|
| `NewProfileManager(baseDir, basePort, opts...) *ProfileManager` | 创建管理器；basePort≤0 时默认 9300 |
| `Open(ctx, name) (*Browser, *Tab, error)` | 打开（或复用）命名档案，返回其浏览器与一个可用标签页 |
| `Get(name) (*Profile, error)` | 取已打开的档案（未打开则报错，不会自动打开） |
| `Names() []string` | 当前已打开的所有档案名 |
| `Close(name)` | 关闭指定档案浏览器；磁盘数据保留 |
| `CloseAll()` | 关闭所有档案浏览器；磁盘数据全部保留 |

> **隔离原理**：每个档案 = 一个独立 Chrome 实例（独立 `--user-data-dir` 与端口），
> 因此隔离最彻底（含代理、UA），且登录态落盘持久。端口按打开顺序从 `basePort` 递增分配，
> 同一 manager 内保证唯一。

---

## 浏览器自动发现

不指定 `WithChromePath` 时，库会按平台依次查找已安装的浏览器，找到第一个可用的就用：

| 平台 | 查找位置 |
|---|---|
| Windows | `%LOCALAPPDATA%` / `%PROGRAMFILES%` / `%PROGRAMFILES(X86)%` 下的 Chrome、Chrome SxS(Canary)、Edge、Brave、Chromium；再用注册表 `App Paths` 兜底 |
| macOS | `~/Applications` 与 `/Applications` 下的 Chrome、Chrome Canary、Edge、Brave、Chromium |
| Linux | `PATH` 中的 google-chrome、chromium、microsoft-edge、brave-browser 等 |

查找结果在进程内缓存。想看它到底找过哪些位置（排查「找不到浏览器」时很有用）：

```go
paths := chromium.SearchedChromePaths() // 候选清单，会一并打印在错误里
chromium.RefreshChromePath()            // 安装位置变了，强制重新查找
```

一个都没找到时不会抛含糊的 `file does not exist`，而是返回 `ErrChromeNotFound` 并列出全部搜索位置。
显式传了 `WithChromePath` 却路径不存在，则**直接报错、不做回退**——避免拼写错误被静默掩盖。

---

## 固定端口接管校验

「端口活着就连」这条规则有个坑：端口上活着的可能是**另一个** Chrome——别人的 user-data-dir、
别人的登录态。旧行为会安静地接管它，你以为在用档案 A，实际用的是别的东西，
Cookie 隔离和多账号方案全部失效，而且一点错都不报。

现在 `Connect` 接管前会校验归属。判定依据是启动时写在 user-data-dir 里的标记文件
`go-drission.profile.json`（记录端口与 PID）：

| 情况 | 行为 |
|---|---|
| 标记文件的端口 == 当前端口 | 认为是自己的浏览器，正常接管 |
| 标记文件存在但端口不符 | 返回 `ErrBrowserMismatch` |
| 没有标记文件，且**显式指定了** `WithUserDataDir` | 返回 `ErrBrowserMismatch`（多半是手动起的 Chrome 或别的库占着端口） |
| 没有标记文件，用的是默认临时目录 | 打一条 warning 后照常接管 |

最后一行是为了保住「连我自己手动开的 9222」这个常见用法——那时没有标记文件是正常的。
如果你明确知道端口上是什么、就是要接管它，可以关掉校验：

```go
chromium.OpenPage(ctx, 9222, chromium.WithTrustExistingBrowser())
```

之所以用本地标记文件而不是读 `chrome://version` 的 `Profile Path`：后者要开标签页、
解析 DOM、再关掉，一次接管多出好几个 CDP 往返，还会在调用方的浏览器里留下痕迹。
标记文件是一次写、一次读，且**手动启动的 Chrome 天然没有它**，正好能区分「我启的」和「别人启的」。

---

## 反检测

默认开启（`WithAntiDetect(true)`），做两件事：

1. **启动参数**：追加 `--disable-blink-features=AutomationControlled`、`--excludeSwitches=enable-automation`、
   `--disable-infobars`、`--mute-audio`，去掉「正受到自动测试软件的控制」提示条及其来源。
2. **页面注入**：每个新建标签页注册 `Page.addScriptToEvaluateOnNewDocument`，把
   `navigator.webdriver` 抹成 `undefined`，并补上无头环境常缺失的 `window.chrome`、
   `navigator.plugins`、`navigator.languages`。注入幂等，重复获取同一标签页不会叠加脚本。

```go
// 关掉反检测（比如要做指纹对比实验，需要干净原生 Chrome）
chromium.OpenPage(ctx, 9222, chromium.WithAntiDetect(false))
```

只做「低风险高收益」的部分：WebGL / Canvas / 字体指纹伪造副作用大、容易误伤正常页面，
有需要请自行用 `Tab.Eval` 注入更完整的 stealth 脚本。

---

## iframe

`Tab.Frames()` 列出页面内所有 iframe（含嵌套，**不含主框架**），定位后拿到 `*Frame`。
`Frame` 的操作同样是 JS 语义（跑在框架的 isolated world 里），但 API 形态与 `Tab` 完全对称：
`Frame` 上的 `Ele*` 返回 `*FrameElement`，选择器只写一次。

```go
// 1) 按选择器定位（选择器命中的必须是 iframe/frame 元素）
f, err := tab.Frame(ctx, chromium.CSS("iframe#login"))
if err != nil {
    log.Fatal(err)
}

// 2) 按 URL 子串 / name 定位
f, err = tab.FrameByURL(ctx, "/login")
f, err = tab.FrameByName(ctx, "loginFrame")

// 3) 遍历全部框架
frames, err := tab.Frames(ctx)
for _, fr := range frames {
    fmt.Println(fr.Name(), fr.URL())
}

// 在框架内读写
text, _ := f.EleCSS("h1").Text(ctx)
n, _ := f.EleCSS("tr").Count(ctx)
_ = f.EleID("user").SetValue(ctx, "admin")
_ = f.EleCSS("button[type=submit]").Click(ctx)
html, _ := f.HTML(ctx)
v, _ := f.Eval(ctx, "document.title")
```

| 方法 | 说明 |
|---|---|
| `Frames(ctx) ([]*Frame, error)` | 所有 iframe（含嵌套，不含主框架）；一个都没有时返回 `ErrFrameNotFound` |
| `Frame(ctx, sel) (*Frame, error)` | 用选择器定位 iframe 元素 |
| `FrameByURL(ctx, substr) (*Frame, error)` | 按 URL 子串定位（首个命中） |
| `FrameByName(ctx, name) (*Frame, error)` | 按 name 属性定位（空 name 自动跳过主框架） |
| `Frame.ID() / .URL() / .Name()` | 框架标识信息 |
| `Frame.Eval(ctx, js) (any, error)` | 在框架内执行 JS |
| `Frame.HTML(ctx)` | 读框架完整 HTML |
| `Frame.Navigate(ctx, url) / .URLNow(ctx)` | 框架内导航 / 读当前地址 |
| `Frame.Ele / EleCSS / EleID / EleXPath / EleJS` | 框架内元素查询入口，返回 `*FrameElement`（与 `Tab` 侧同名同形） |
| `FrameElement.Click(ctx) / .SetValue(ctx, v)` | JS 点击 / 赋值（不做可见性检查，不产生键盘事件） |
| `FrameElement.Text(ctx) (string, error)` | 读元素 innerText；元素不存在返回 `ErrElementNotFound` |
| `FrameElement.Count(ctx) (int, error)` | 匹配元素数量，四种选择器都支持（0 是正常返回值） |
| `FrameElement.Selector() / .Frame() / .String()` | 元信息：选择器 / 所属框架 / 可读描述 |

> **跨域 iframe 也能操作**：库通过 `Page.createIsolatedWorld` 在目标框架里建独立执行环境
> （带通用访问权限），再用 `Runtime.evaluate` 在框架内读写。相比 attach 到独立 target 的做法，
> 同进程 iframe 同样适用，且不受同源策略限制——因此**不需要**先 `switch_to_frame` 再操作。

---

## Session：纯 HTTP 模式

`session` 包是 `net/http` 的薄封装，对标 DrissionPage 的 SessionPage：不带浏览器、
直接收发 HTTP，自带 Cookie 管理。适合爬接口、下文件、跑无需 JS 的页面，速度比浏览器快两个数量级。

```go
import "github.com/yymm456/go-drission/session"

s := session.New(
    session.WithTimeout(15*time.Second),
    session.WithUserAgent("Mozilla/5.0 ..."),
    session.WithProxy("http://127.0.0.1:7890"),
)

// GET
resp, err := s.Get(ctx, "https://httpbin.org/get")
if err != nil {
    log.Fatal(err)
}
fmt.Println(resp.StatusCode, resp.Text())

// POST JSON
resp, _ = s.PostJSON(ctx, "https://httpbin.org/post", map[string]any{"name": "go"})
var out map[string]any
resp.JSON(&out)

// 表单
resp, _ = s.PostForm(ctx, "https://httpbin.org/post", url.Values{"a": {"1"}})

// 自定义请求
resp, _ = s.Request(ctx, http.MethodPut, "https://httpbin.org/put", strings.NewReader("x"))
```

### Response

body 已一次性读入内存并自动替换 `resp.Body`，**可反复读取**：

| 方法 / 字段 | 说明 |
|---|---|
| `Bytes() []byte` | 原始字节 |
| `Text() string` | 按 UTF-8 转字符串 |
| `JSON(v any) error` | 解析 JSON |
| `OK() bool` | 状态码为 2xx |
| `ContentType() string` | 响应类型（已去掉参数） |
| `SaveFile(path) error` | 直接存盘（父目录自动创建，目录 0750 / 文件 0640） |
| 内嵌 `*http.Response` | `StatusCode` / `Header` / `Request` 等原样可用 |

**body 有大小上限**（默认 32MB，`session.WithMaxBodySize(n)` 可调，传 0 关闭）。
超限返回 `ErrBodyTooLarge`，理由是「抓到一个 `Content-Length: 8G` 的误配置接口」不该
直接把进程 OOM 掉。需要流式处理大响应（下载文件之类）请走 `Client()` 拿底层
`http.Client` 自己发请求——那时 body 由你控制，不经过 `Response`。

```go
s := session.New(session.WithMaxBodySize(8 << 20)) // 8MB
_, err := s.Get(ctx, url)
if errors.Is(err, session.ErrBodyTooLarge) {
    // 换 Client() 走流式下载
}
```

### Cookie：可导出、可复用

自研 `Jar` 实现 `http.CookieJar`，额外支持**全量导出 / 导入**，这是标准库 `cookiejar` 做不到的：

```go
// 登录，Cookie 自动进 Jar
s.PostJSON(ctx, "https://example.com/login", creds)

// 存盘
_ = s.SaveCookies("./cookies.json")

// 另一个 Session（或程序重启后）直接吃同一份 Cookie
s2 := session.New()
_ = s2.LoadCookies("./cookies.json")
resp, _ := s2.Get(ctx, "https://example.com/profile") // 已登录态

// 也可以把浏览器里的 Cookie 搬过来，或反向搬过去
s.SetCookie(session.CookieItem{Name: "sid", Value: "xxx", Domain: "example.com", Path: "/"})
items := s.Cookies() // []CookieItem，字段与 chromium.Cookie 对齐
```

| API | 说明 |
|---|---|
| `session.NewJar() *Jar` | 创建 Jar（并发安全） |
| `Jar.All() []CookieItem` | 全量导出（**含已过期项**，含 Domain/Path/Expires/HttpOnly/Secure/SameSite/Partitioned） |
| `Jar.Valid() []CookieItem` | 只导出**未过期**的 Cookie |
| `Jar.Load([]CookieItem)` | 全量导入（覆盖式） |
| `Jar.ExportJSON() / .ImportJSON([]byte)` | JSON 互转 |
| `Jar.SaveFile(path) / .LoadFile(path)` | 文件读写 |
| `Jar.Clear() / .Len()` | 清空 / 计数 |
| `Jar.CookiesFor(url) ([]CookieItem, error)` | **按 URL 过滤**导出（只给该地址真会携带的 Cookie） |
| `Jar.ExportJSONFor(url) / .SaveFileFor(url, path)` | 按 URL 过滤的 JSON / 文件导出 |
| `Session.SaveCookies(path) / .LoadCookies(path)` | Session 级存盘入口 |
| `Session.SaveCookiesFor(url, path)` | 按 URL 过滤后存盘 |
| `Session.Cookies() / .CookiesFor(url)` | 导出**未过期**的 Cookie（按 URL 过滤时只给该地址真会携带的那些） |
| `Session.AllCookies() / .AllCookiesFor(url)` | 同上，但**含已过期项**（排查问题、完整备份时用） |
| `Session.SetCookie(item) / .SetCookies(items) / .ClearCookies()` | 写入（`SetCookies` 返回条数） / 清空 |

> 安全：`Domain` 为**公共后缀**（`com`、`co.uk`、`github.io` ...）的 Cookie 会被直接拒收，
> 与浏览器行为一致——否则任意 `example.com` 都能给所有 `.com` 域投毒。

> `SameSite=None` 但没带 `Secure` 的 Cookie 同样会被拒收：浏览器本来就会拒绝这种组合，
> 照单全收只会制造「本地能过、真浏览器不行」的假象，让问题跑到后面才炸。
> 导入 JSON 时也一样会被过滤掉。

> `CookieItem` 的 `Expires` 为 **Unix 秒**（`float64`，0 表示会话 Cookie），
> 字段名与 `chromium.Cookie` 对齐，两套之间可直接搬运。

**分区 Cookie（CHIPS）**：`Partitioned` / `PartitionKey` 两个字段用于承载
`Partitioned`（`Secure` + `SameSite=None` + `Partitioned`）Cookie 的分区属性。
这类 Cookie 由 `Secure` 的**顶层站点**隔离，常见于嵌入式第三方内容；
导出再导入时如果不带上分区属性，原本互不可见的两份 Cookie 会互相覆盖。

### 免登录：Session 抓包 → 浏览器直接接管

纯 HTTP 登录拿到 Cookie 后，把登录态交给浏览器，跳过登录表单直接进业务页。

两个包不互相 import：`chromium.CookieSource` 只要求实现
`ExportJSON() ([]byte, error)`，而 `session.Jar` 恰好满足它：

```go
// 1) 纯 HTTP 登录（快、稳、不需要浏览器）
sess := session.New()
sess.PostForm(ctx, "https://site.com/login", url.Values{
    "user": {"alice"}, "pass": {"secret"},
})

// 2) 交给浏览器，直接打开业务页 —— 浏览器从没见过登录表单
tab, _ := browser.NewTab(ctx)
if err := tab.LoginWithCookies(ctx, "https://site.com/home", sess.Jar()); err != nil {
    log.Fatal(err)
}
```

需要在某个隔离上下文里登录（多账号）时，用 `bc.NewTab(ctx)` 拿到的标签页调用同一个方法即可，
Cookie 只会落在该上下文内。

也可以走文件接力（跨进程、跨语言都适用）：

```go
sess.SaveCookiesFor("https://site.com/home", "./sid.json") // 只导出该站点用得到的 Cookie
// 另一个进程：
data, _ := os.ReadFile("./sid.json")
tab.LoginWithCookiesJSON(ctx, "https://site.com/home", data)
```

反方向同样成立：浏览器登录完把 Cookie 导出，喂给 Session 做批量抓取。

```go
_ = tab.ExportCookies(ctx, "./cookies.json", "https://site.com") // 0600 + 自动建目录
s := session.New()
_ = s.LoadCookies("./cookies.json")
```

### Session 方法 / 选项

| 方法 | 说明 |
|---|---|
| `New(opts...) *Session` | 创建；默认 30s 超时、自动跟随重定向、自带 Jar |
| `Get / Post / PostForm / PostJSON / Request / Do` | 请求方法，全部收 `ctx` |
| `SetHeader(k, v) / SetHeaders(map) / DelHeader(k) / Headers()` | 默认请求头管理 |
| `SetTimeout(d) / SetProxy(proxy)` | 运行期调整超时 / 代理（内部「复制-替换」，不会与其他设置互相踩踏） |
| `MaxBodySize() / SetMaxBodySize(n)` | 读 / 改响应体大小上限（0 表示不限制） |
| `Jar() *Jar` | 取出 Jar 做导出 |
| `Client() *http.Client` | 逃逸口，需要完全自定义时直接拿底层 client。**返回的是副本**，改它不会影响 Session 内部的并发安全 |

| 选项 | 说明 |
|---|---|
| `WithTimeout(d)` | 整体超时（默认 30s） |
| `WithMaxBodySize(n)` | 响应体大小上限（默认 32MB，传 0 关闭） |
| `WithProxy(proxy)` | HTTP/SOCKS5 代理 |
| `WithHeader(k, v)` / `WithUserAgent(ua)` | 默认请求头 |
| `WithJar(j)` | 复用已有 Jar（多 Session 共享 Cookie） |
| `WithTransport(t)` | 自定义 RoundTripper |
| `WithInsecureTLS()` | 跳过证书校验 |
| `WithNoRedirect()` | 不跟随 3xx |

> `WithProxy` 与 `WithInsecureTLS` **互相独立、与书写顺序无关**：两者都只改
> 克隆后 `Transport` 的对应字段，不会把对方的配置或标准库默认值（HTTP/2、TLS 握手超时等）覆盖掉。

---

## 测试

```bash
# 单元测试：不依赖浏览器，秒级跑完，CI 默认执行
go test -race ./...

# 静态检查（配置见 .golangci.yml，含 gofmt / goimports 格式检查）
golangci-lint run

# 端到端冒烟测试：起真实 Chrome，验证选择器 / iframe / Session 的真实行为
go test -tags smoke -timeout 5m ./smoke/...
```

本地没装 golangci-lint 时，用与 go.mod 一致的工具链自建一个（二进制必须是拿
不低于 `go.mod` 声明版本的 Go 编译的，否则会直接报「language version lower than targeted」）：

```bash
GOBIN="$HOME/.workbuddy/binaries/golangci-lint" \
  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

冒烟测试用 build tag 隔离，原因是它必须真起一个 Chrome（冷启动 1~2 秒），
不该拖慢日常 `go test`。本机找不到浏览器时全部用例自动 skip，不会误报失败。

覆盖范围：四种选择器定位与未命中行为、Shadow DOM 穿透、同域与**跨域** iframe 读写、
Session 的请求方法与 Cookie 跨实例复用、反检测生效、网络监听、隔离上下文、截图落盘。

---

## 已知边界

- 响应头取自 CDP `responseReceived` 事件，为初步头；完整头（含部分 `Set-Cookie`）在
  `responseReceivedExtraInfo` 事件，当前未合并。
- `Eval` 返回值按 JSON 解码：`string` / `float64` / `map[string]any` /
  `[]any` / `bool` / `nil`。
- `Listener.Records()` 返回的是**深拷贝**：监听进行中也能安全读取，但请不要依赖
  「改返回值能影响内部状态」这种行为。记录默认只保留最近 **1000** 条
  （`MaxRecords(n)` 可调、传 0 关闭上限），更早的会被 FIFO 淘汰——
  需要完整全量就调大上限，或「`Records()` 取走 → `Clear()` 清空」分批处理。
- **默认 user-data-dir 在系统临时目录**（Windows 即 `%TEMP%`）。清理工具、磁盘紧张、
  系统重置都可能把它清掉，登录态随之丢失。要长期保存登录态就显式指定
  `WithUserDataDir`（`ProfileManager` 已自动落在 `profiles/<name>`）。
- `Browser.Tabs()` 返回的 `*Tab` 上，`URL()` 是**最近一次同步到的地址快照**，可能滞后；
  要「此刻浏览器里真实显示的地址」请用 `CurrentURL(ctx)`（走一次 CDP 拿实时值）。
- 元素对象是 `Element`（查询与操作分离），但它**不持有 DOM 节点**：每次操作都重新查询，
  SPA 重渲染不会失效，代价是每次多一次 CDP 往返。要重复操作请复用 `*Element`。
- `Frame` 上的元素操作与 `Tab` 刻意镜像，同样走「查询与操作分离」：`f.EleCSS("h1").Text(ctx)`
  返回 `*FrameElement`（不再有 `f.Text(ctx, sel)` 这种选择器随调用传入的写法）。差别只在底层通道：
  `FrameElement` 在框架 isolated world 内用 JS 完成（故能进跨域 iframe），Click 不做可见性检查、
  SetValue 不产生键盘事件；要真实鼠标 / 键盘事件请用 `Tab` 侧的 `Element`（代价是进不去跨域 iframe）。
- `Frame` 目前不支持截图（CDP 对非主框架截图需走 `Page.captureScreenshot` 的 clip 方案，未实现）。
- Session 不解析 HTML，需要 DOM 操作请配合浏览器模式，或自行接入 goquery。

---

## 错误处理

固定的错误都用哨兵值导出，可以用 `errors.Is` 判断，不必比对字符串：

```go
if errors.Is(err, chromium.ErrClosed) {
    // 浏览器已关闭，重建连接
}
```

| 哨兵错误 | 含义 |
|---|---|
| `ErrClosed` | Browser 已关闭 |
| `ErrNotConnected` | 还没 `Connect` 就开始用（常见于 `NewBrowser` 后直接 `NewTab`） |
| `ErrAlreadyConnected` | 重复 `Connect`（会泄漏上一条连接，已显式拒绝） |
| `ErrInvalidContext` | 传入的 ctx 不是从 `tab.Ctx` 派生的 |
| `ErrNoTab` | 当前没有标签页 |
| `ErrContextClosed` | 隔离上下文已销毁 |
| `ErrProfileClosed` | ProfileManager 已 `CloseAll` |
| `ErrListenerStarted` | 监听器重复 `Start` |
| `ErrChromeNotFound` | 未找到可用浏览器 |
| `ErrBrowserMismatch` | 固定端口上的浏览器与期望的 user-data-dir 不一致，拒绝接管（见「固定端口接管校验」） |
| `ErrElementNotFound` | 选择器没匹配到任何元素（含超时仍未出现的情况） |
| `ErrFrameNotFound` | 页面内找不到匹配的 iframe |
| `ErrWaitConditionUnset` / `ErrSelectorRequired` | WaitBuilder 参数不全 / 选择器为空 |
| `ErrFrameDetached` | Frame 随页面导航失效且无法按原定位依据找回，需重新取 Frame |
| `ErrInvalidCookie` / `ErrInvalidCookieJSON` | 注入的 Cookie 字段不全 / JSON 结构非法 |
| `ErrEmptyURL` | 传入的地址为空或不是绝对 http(s) 地址 |

`session` 包同样导出一组哨兵值：

| 哨兵错误 | 含义 |
|---|---|
| `ErrEmptyURL` / `ErrInvalidURL` / `ErrUnsupportedProtocol` | 地址为空 / 非法 / 非 http(s) |
| `ErrInvalidProxy` | 代理地址无法解析或缺协议头 |
| `ErrEmptyBody` / `ErrInvalidJSON` | 响应体为空 / 不是合法 JSON |
| `ErrCookieFileRead` / `ErrCookieFileWrite` / `ErrInvalidCookieJSON` | Cookie 文件读写 / 结构异常 |
| `ErrNilRequest` / `ErrNilResponse` | 传入 nil 请求 / 底层返回 nil 响应 |
| `ErrBodyTooLarge` | 响应体超过 `WithMaxBodySize` 上限（默认 32MB），已中止读取，避免 OOM |

另有 `session.IsRetryable(err)` 用于粗粒度区分「重试有意义」与「重试多少次都一样」。

---

## 许可证

本项目基于 [MIT License](LICENSE) 开源，可自由用于个人与商业项目。
