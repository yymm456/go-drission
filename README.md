# go-drission

用 **Go 标准写法**实现的浏览器自动化库，功能参考 Python 的 DrissionPage，但遵循 Go 的惯用法：
`context.Context` 贯穿所有 I/O、错误如实返回不吞掉、超时与取消由调用方掌控。

底层基于 [chromedp](https://github.com/chromedp/chromedp)（Chrome DevTools Protocol）。

---

## 目录结构

```
go-drission/
├── go.mod
├── main_debug.go        示例 / 调试入口
└── chromium/
    ├── browser.go       Browser：连接、标签页管理、OpenPage
    ├── tab.go           Tab：导航、等待、点击、输入、读取、截图、Eval
    ├── launch.go        端口探测、Chrome 启动
    ├── targets.go       HTTP /json 查询与标签页同步
    ├── options.go       函数式配置项 WithXxx
    ├── cookies.g.go     Cookie 注入 / 读取 / 导入导出
    ├── listen.go        Network 域被动监听（Listener / Record）
    ├── port.go          空闲端口分配
    └── util.go          writeFile、contains
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
    "go-drission/chromium"
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

### Tab —— 操作

| 方法 | 说明 |
|---|---|
| `Click(ctx, selector) error` | 点击（含可见性检查） |
| `ClickJS(ctx, selector) error` | 用 JS 触发点击，绕过可见性检查 |
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

## 已知边界

- 响应头取自 CDP `responseReceived` 事件，为初步头；完整头（含部分 `Set-Cookie`）在
  `responseReceivedExtraInfo` 事件，当前未合并。
- `Eval` 返回值按 JSON 解码：`string` / `float64` / `map[string]interface{}` /
  `[]interface{}` / `bool` / `nil`。
