# go-drission

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26.0-00ADD8.svg)](go.mod)

用 **Go 标准写法**实现的浏览器自动化库，功能参考 Python 的 DrissionPage，但遵循 Go 的惯用法：
`context.Context` 贯穿所有 I/O、错误如实返回不吞掉、超时与取消由调用方掌控。

底层基于 [chromedp](https://github.com/chromedp/chromedp)（Chrome DevTools Protocol）。

---

## 安装

```bash
go get github.com/yymm456/go-drission@latest
```

---

## 目录结构

```
go-drission/
├── go.mod
├── main.go        示例 / 调试入口
├── chromium/
│   ├── browser.go       Browser：连接、标签页管理、OpenPage
│   ├── context.go       BrowserContext：单浏览器内多账户隔离上下文
│   ├── profile.go       ProfileManager：多账户命名档案（独立进程）隔离
│   ├── tab.go           Tab：导航、等待、点击、输入、读取、截图、Eval
│   ├── wait.go          WaitBuilder：链式等待 API
│   ├── launch.go        端口探测、Chrome 启动
│   ├── targets.go       HTTP /json 查询与标签页同步
│   ├── options.go       函数式配置项 WithXxx（含 WithLogger）
│   ├── cookies.go       Cookie 注入 / 读取 / 导入导出
│   ├── listen.go        Network 域被动监听（Listener / Record）
│   ├── port.go          空闲端口分配
│   └── util.go          writeFile、contains
└── example/               可运行示例（go run ./example/xxx）
    ├── basic/             连接、导航、读取、截图
    ├── wait/              链式 Wait Builder
    ├── screenshot/        页面截图
    ├── listen/            网络监听
    ├── context/           单浏览器多上下文 Cookie 隔离（含同窗口多标签共享会话证明）
    ├── profile/           多进程浏览器隔离（ProfileManager）
    ├── concurrent_tabs/   同浏览器多标签并发任务 + 标签切换
    └── cookie/            Cookie 导入导出 / 注入
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
- `Browser.Close()` 是 teardown 操作，遵循 `io.Closer` 惯例，不接受 ctx。

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
| screenshot | `go run ./example/screenshot` | 视口截图，输出到 `screenshots/` |
| listen | `go run ./example/listen` | 被动监听网络请求/响应 |
| context | `go run ./example/context` | 单浏览器多上下文 Cookie 隔离：上下文间 Cookie 互不可见、同上下文多标签共享会话、窗口语义证明 |
| profile | `go run ./example/profile` | 多进程浏览器隔离：多档案并发任务、PID 证明、同名复用与关闭 |
| concurrent_tabs | `go run ./example/concurrent_tabs` | 同浏览器多标签页 goroutine 并发执行任务（互不干扰）+ BringToFront 切换 |
| cookie | `go run ./example/cookie` | Cookie 注入 / 读取 / 导出 / 导入 |

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
| `Connect(ctx) error` | 探测端口：活着就连，空闲就启动 Chrome |
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

### Tab —— 导航 / 读取

| 方法 | 说明 |
|---|---|
| `Navigate(ctx, url) error` | 导航并等待 load，超时/失败如实返回 error |
| `Reload(ctx) error` | 重新加载 |
| `Title(ctx) (string, error)` | 页面标题 |
| `CurrentURL(ctx) (string, error)` | 当前地址 |
| `HTML(ctx) (string, error)` | 完整 HTML |
| `Text(ctx, selector) (string, error)` | 元素文本 |
| `Attribute(ctx, selector, name) (string, error)` | 元素属性值 |
| `Count(ctx, selector) (int, error)` | 匹配元素数量（直接求值到 int） |
| `Eval(ctx, js) (interface{}, error)` | 执行 JS，按 JSON 语义返回原始值 |
| `Screenshot(ctx, path) error` | 截图保存 |

### Tab —— 等待（超时统一由 ctx 控制）

| 方法 | 说明 |
|---|---|
| `WaitReady(ctx) error` | 等待 body 就绪 |
| `WaitVisible(ctx, selector) error` | 等待元素可见 |
| `WaitURL(ctx, substr) error` | 轮询等待 URL 包含子串 |
| `WaitText(ctx, selector, substr) error` | 轮询等待元素文本包含子串 |
| `WaitCount(ctx, selector, n) error` | 轮询等待元素数量 ≥ n |

#### 链式 Wait Builder

上面的 `WaitXxx` 方法之外，`tab.Wait()` 提供流式构建器，把「等什么 + 等多久」串成一句，
底层仍复用同样的等待逻辑，语义一致：

```go
// 等元素可见，最多 10s（Timeout 在传入 ctx 之上再派生超时）
if err := tab.Wait().Element("#submit").Visible().Timeout(10 * time.Second).Do(ctx); err != nil {
    log.Fatal(err)
}

// 等列表项 >= 5 个
tab.Wait().Element("li.item").Count(5).Do(ctx)

// 等状态文本包含「完成」
tab.Wait().Element("#status").Text("完成").Do(ctx)

// 等 URL 跳转到含 dashboard 的地址
tab.Wait().URL("dashboard").Timeout(15 * time.Second).Do(ctx)

// 等页面就绪
tab.Wait().Ready().Do(ctx)
```

| 方法 | 说明 |
|---|---|
| `Wait() *WaitBuilder` | 开始构建一个等待 |
| `Element(selector)` | 设定目标选择器（供 Visible/Present/Text/Count 使用） |
| `Visible()` | 元素可见 |
| `Present()` | 元素存在于 DOM（数量 ≥ 1） |
| `Text(substr)` | 元素文本包含 substr |
| `Count(n)` | 匹配元素数量 ≥ n |
| `URL(substr)` | 当前地址包含 substr（独立条件） |
| `Ready()` | 页面 body 就绪（独立条件） |
| `Timeout(d)` | 设定整体超时；不调用则沿用 ctx 的 deadline |
| `Do(ctx) error` | 执行等待，超时/不满足如实返回 error |

### Tab —— 操作

| 方法 | 说明 |
|---|---|
| `Click(ctx, selector) error` | 点击（含可见性检查） |
| `ClickJS(ctx, selector) error` | 用 JS 触发点击，绕过可见性检查 |
| `BringToFront(ctx) error` | 标签页激活置前（后台窗口节流会丢输入，交互前先调用） |
| `WindowID(ctx) (int64, error)` | 标签页所属 OS 窗口编号（同窗口多标签验证） |
| `SendKeys(ctx, selector, text) error` | 输入文本（先清空） |
| `SetValue(ctx, selector, value) error` | 直接设值并触发 input/change，适配 React/Vue |

### Tab —— Cookie

| 方法 | 说明 |
|---|---|
| `SetCookie(ctx, cookie) error` | 注入单个 Cookie |
| `SetCookies(ctx, cookies) error` | 批量注入 |
| `GetCookies(ctx, urls...) ([]*network.Cookie, error)` | 读取 Cookie |
| `ExportCookies(ctx, path, urls...) error` | 导出到 JSON 文件 |
| `ImportCookies(ctx, path) error` | 从 JSON 文件导入 |

### 配置项 Option

| 函数 | 说明 |
|---|---|
| `WithChromePath(path)` | Chrome 可执行文件路径 |
| `WithUserDataDir(dir)` | 用户数据目录（持久化登录态） |
| `WithUserAgent(ua)` | User-Agent |
| `WithProxy(proxy)` | 代理，如 `http://127.0.0.1:7890` |
| `WithHeadless(bool)` | 无头模式 |
| `WithWindowSize(size)` | 窗口大小，如 `1920,1080` |
| `WithConnectTimeout(d)` | 连接超时（ctx 无 deadline 时的默认值） |
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
| `Records() []*Record` | 返回按到达顺序的所有记录 |
| `Stop()` | 停止监听并等待后台任务收尾 |

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

## 已知边界

- 响应头取自 CDP `responseReceived` 事件，为初步头；完整头（含部分 `Set-Cookie`）在
  `responseReceivedExtraInfo` 事件，当前未合并。
- `Eval` 返回值按 JSON 解码：`string` / `float64` / `map[string]interface{}` /
  `[]interface{}` / `bool` / `nil`。

---

## 许可证

本项目基于 [MIT License](LICENSE) 开源，可自由用于个人与商业项目。
