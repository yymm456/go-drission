# 变更日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

**0.x 阶段说明**：小版本内仍可能出现破坏性变更，但每次都会显式标注 `BREAKING`
并写清迁移方式。当前稳定基线（Go / OS / Chromium 版本、测试命令与结果）见 [`TESTING.md`](./TESTING.md)。

---

## [未发布]

### 新增

- **`Tab.Back(ctx)` / `Tab.Forward(ctx)`**：后退 / 前进一条历史记录。
  到达历史边界时返回 `ErrNoHistoryEntry`（可用 `errors.Is` 判断），**页面保持不动** ——
  不做静默 no-op，否则调用方分不清「真的退了」和「没得可退」。
- `ErrNoHistoryEntry` 哨兵错误。

### 已知边界

- Back / Forward 判定「导航完成」用的是**地址变化**，不是 load 事件：
  历史导航（从缓存恢复）不会再触发一次 load，等它会一路卡到 ctx 超时。
  因此 `Navigate` 等 load、`Back`/`Forward` 等地址、`Reload` 只发命令不等 ——
  这三者当前并不一致，`Reload` 的语义以后单独讨论。

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
