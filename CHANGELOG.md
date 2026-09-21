# 变更日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

**0.x 阶段说明**：小版本内仍可能出现破坏性变更，但每次都会显式标注 `BREAKING`
并写清迁移方式。当前稳定基线（Go / OS / Chromium 版本、测试命令与结果）见 [`TESTING.md`](./TESTING.md)。

---

## [未发布]

### 新增

- **`Tab.Activate(ctx)`**：激活并聚焦该 target（封装 `Target.activateTarget`）。
  与既有的 `BringToFront`（`Page.bringToFront`，"activates tab"）相比多一层聚焦 ——
  实测（非 headless）能把最小化的窗口恢复出来。两者都不保证把窗口顶到其它应用之上
  （那是 OS 级能力，CDP 没有）。
- **`Tab.SetWindowState(ctx, state)`**：设置所属窗口状态（封装 `Browser.setWindowBounds`
  的 windowState 维度），取值 `normal` / `minimized` / `maximized` / `fullscreen`。
  只暴露状态这一个维度，不把上游 `Bounds` 类型透出去。
- `ErrInvalidWindowState` 哨兵错误：非法状态值在发起 CDP 前就被拦下
  （否则 Chrome 只回一句 "Invalid window bounds"，看不出哪个值错了）。

### 已知边界

- Chrome 不允许从 `minimized` 直接切 `maximized` / `fullscreen`，必须先恢复 `normal`
  （实测报错 "To maximize a minimized or fullscreen window, restore it to normal state first."）。
  调用方需要自己按 `normal` 打底的顺序走。
- headless 下这两个 API 都能调用（Chrome 会给实例一个虚拟窗口，`WindowID` 返回非 0），
  但 `visibilityState` / `hasFocus` 的变化只有在带窗口的实例上才看得到。

## [0.4.0] - 2026-09-20

### 新增

- **`Tab.Close()`**：关闭标签页不必先拿到 `Browser`。与 `Browser.CloseTab(ctx, tab)`
  是同一件事（同一条 CDP 命令 `Target.closeTarget` + `ReleaseTab`），区别只是不收 ctx、
  不返回错误 —— 与 `Browser.Close` / `BrowserContext.Close` 保持一致。
  关闭后它在 Browser 台账里的登记会在下一次 `Tabs()` / `LatestTab()` / `GetTab()` 时
  被自动摘掉（`syncTabs` 发现 target 已消失就会释放并移除）。重复调用安全。
  注意：关掉窗口里的第一个标签页可能连同窗口一起关掉。

- **`Tab.Back(ctx)` / `Tab.Forward(ctx)`**：后退 / 前进一条历史记录。
  到达历史边界时返回 `ErrNoHistoryEntry`（可用 `errors.Is` 判断），**页面保持不动** ——
  不做静默 no-op，否则调用方分不清「真的退了」和「没得可退」。

- `ErrNoHistoryEntry` 哨兵错误。

### 修复

- **补上分区 Cookie（CHIPS）支持**：`chromium.Cookie` 此前缺少 `Partitioned` / `PartitionKey`
  两个字段，而 `session.CookieItem` 有 —— 跨包接力时分区属性会被 JSON 静默丢弃
  （Go 忽略未知字段），Cookie 退化成普通 Cookie 而调用方毫不知情，
  与「两包 JSON 字段一致」的承诺不符。
  现在两条注入路径（单个 / 批量）与读取路径都做了映射，语义为：
  - `Partitioned=true` 且有 `PartitionKey` → 写成 CDP 的 `CookiePartitionKey`
    （`TopLevelSite` 取 `PartitionKey` 的值，形如 `https://example.com`）；
  - 否则**原样传 nil** —— CDP 的语义是「不设 partitionKey 即普通 Cookie」，
    塞空结构体会把普通 Cookie 错误地变成分区 Cookie；
  - 标记了分区却没给 key，会在注入前被 `ValidateCookie` 拦成 `ErrInvalidCookie`
    （与 `SameSite=None` 缺 `Secure` 同类，不静默降级）。

### 文档

- 新增 `skills/go-drission/SKILL.md`：给 AI Coding Agent 用的行为规范 + API 指南 +
  架构约束（不是 README 的翻版），内容全部按源码核实过。
- `TESTING.md` 记录测试分层、稳定基线与长时间跑法。

### 已知边界

- **`Browser.Close()` 会关掉本实例托管的全部标签页**，不只是断开连接 —— 这是把它写清楚
  的一条（**只改文档与注释，行为未变**）：实现里逐个取消标签页上下文，而 chromedp 在
  `ctx.Done` 里做 `DetachFromTarget` + `CloseTarget`，因此**只要是 attach 过的 target 都会
  被关**，`NewTab` 出来的和从外部附着来的一样中招（唯一例外是 chromedp 视作「原始标签」的
  建连锚点）。
  实测（真实 Chrome）：若被关掉的包含浏览器里最后一个标签，非 headless 的 Chrome 会直接
  退出，调试端口随之消失。
  → 「只断连接、保留页面」不能靠 `Close`；要清连接缓存请先确认浏览器已不可达。

- Back / Forward 判定「导航完成」用的是**地址变化**，不是 load 事件：历史导航
  （从缓存恢复）不会再触发一次 load，等它会一路卡到 ctx 超时。因此 `Navigate` 等 load、
  `Back`/`Forward` 等地址、`Reload` 只发命令不等 —— 这三者当前并不一致，
  `Reload` 的语义以后单独讨论。

### 测试

- 单元测试 105 个（不依赖浏览器）、冒烟测试 97 个（真实 Chrome）
- 100 轮启停稳定性实测：goroutine 零增长、无进程与端口残留
- smoke 统一改用 40000+ 高位调试端口，避开 9222（用户最可能自己开着的调试端口）

## [0.3.0] - 2026-09-19

### 新增

- **`chromium.WithInsecureTLS()`**：跳过 TLS 证书校验，用于自签证书的内网站点。
  内部追加 `--ignore-certificate-errors` 与配套的 `--test-type`
  （只给前者时部分 Chrome 版本仍会拦下证书错误）。与 `session.WithInsecureTLS` 同名同义。
- **`Record.SetCookies []string`**：响应下发的全部 `Set-Cookie`，按出现顺序排列。

### 修复

- **响应头缺 `Set-Cookie`**（此前记在 README「已知边界」里的真实缺口）：
  CDP 把 `Set-Cookie` 藏在 `responseReceivedExtraInfo` 事件里，只听 `responseReceived` 拿不到。
  现已合并两个事件。三个决定实现方式的事实：
  - ExtraInfo 可能在 `responseReceived` **之前或之后**到达 → 两侧都要尝试合并；
  - **并非每个响应都有** ExtraInfo → 它不来是正常的，不当作错误；
  - 重复头用 `\n` 连接成单键 → 多值按 `\n` 拆开即可还原。

  因为 `ResponseHeaders` 是 `map[string]string`，一次响应下发多个 `Set-Cookie`
  时同名键会互相覆盖，所以多值另开 `SetCookies` 字段存放。

### 变更

- **BREAKING**：`AntiDetect` 默认值由「开启」改为「关闭」。
  需要反检测请显式传 `chromium.WithAntiDetect(true)`；
  此前依赖默认值、未显式传参的调用方，浏览器指纹特征会回到原生值。
- 内部分包重构（9 步）：`chromium` 收敛为门面，实现下沉到
  `browser` / `page` / `chrome` / `cdpkit` / `network` / `profile` / `cookie` / `config` / `errs`。
  **对外 `import ".../chromium"` 的写法与全部公开符号均未变。**
- 全仓注释精简：对外文档只在门面 `chromium/api.go` 留一份，实现包压成一句并指向门面。

### 测试

- 单元测试 102 个（不依赖浏览器，秒级跑完）；冒烟测试 61 个（真实 Chrome）
- 新增三组覆盖：资源泄漏与长时间稳定性、异常路径
  （不 panic / 不永久阻塞 / 该报错就报错）、Profile 真实流程
- 建立 `TESTING.md`；CI 的 smoke job 改为 `-race` 并放宽超时到 10m

---

## [0.2.0] - 2026-09-18

### 新增

- `session` 包：纯 HTTP 会话（`Session` / `Jar` / `Response`），
  Cookie 可导出成 JSON，可与浏览器侧互相接力实现免登录
- `Element` / `FrameElement` API

### 修复

BUG-02 ~ BUG-11 等一批缺陷（响应体上限、Cookie 校验、IPv6 主机解析、
隔离上下文重复托管、attach 超时、并发 `Tabs()` 误销毁新建标签页等）

---

## [0.1.1] - 2026-09-17

- 整理 import 分组（标准库与模块分组）

## [0.1.0] - 2026-09-17

- 模块改名为 `github.com/yymm456/go-drission`

---

## 链接

- 仓库：<https://github.com/yymm456/go-drission>
