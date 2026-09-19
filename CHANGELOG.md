# 变更日志

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

**0.x 阶段说明**：小版本内仍可能出现破坏性变更，但每次都会显式标注 `BREAKING`
并写清迁移方式。当前稳定基线（Go / OS / Chromium 版本、测试命令与结果）见 [`TESTING.md`](./TESTING.md)。

---

## [0.1.0] - 2026-09-19

首个对外基线：架构分层冻结、测试分层建立完毕。

### 新增

**浏览器自动化（`chromium`）**

- `Browser` / `Tab` / `Element` / `Frame` / `FrameElement` / `Selector` / `WaitBuilder` / `Listener` / `ProfileManager`
- 两套多账户隔离方案：单浏览器隔离上下文（`Browser.Context`）与多进程档案（`ProfileManager`）
- `WithInsecureTLS()`：跳过 TLS 证书校验，用于自签证书的内网站点
  （内部追加 `--ignore-certificate-errors` 与配套的 `--test-type`）
- `Record.SetCookies`：响应下发的全部 `Set-Cookie`，按出现顺序排列

**纯 HTTP 会话（`session`）**

- `Session` / `Jar` / `Response`
- Cookie 可导出成 JSON，可与浏览器侧互相接力实现免登录

### 变更

- **BREAKING**：`AntiDetect` 默认值由「开启」改为「关闭」。
  需要反检测请显式传 `chromium.WithAntiDetect(true)`；
  此前依赖默认值、未显式传参的调用方，浏览器指纹特征会回到原生值。
- 内部分包重构（9 步）：`chromium` 收敛为门面，实现下沉到
  `browser` / `page` / `chrome` / `cdpkit` / `network` / `profile` / `cookie` / `config` / `errs`。
  **对外 `import ".../chromium"` 的写法与全部公开符号均未变。**
- 全仓注释精简：对外文档只在门面 `chromium/api.go` 留一份，实现包压成一句并指向门面。

### 修复

编号沿用代码注释里的 BUG-xx：

- BUG-02 响应体上限默认值与文档不符（文档声称的 OOM 保护形同虚设）
- BUG-03 `SetCookies` 未校验 `SameSite=None` 必须带 `Secure` —— 返回 `nil` 但 Cookie 没落盘
- BUG-04 `Element.Count` 未命中时，CSS 与 XPath/ID 的行为不一致
- BUG-05 `canonicalHost` 处理 IPv6 错误，导致 Cookie 被静默丢弃
- BUG-06 隔离上下文内的标签页被 `Browser` 重复托管（同一 target 两份记录）
- BUG-07 attach 的超时加错位置，标签页建出来后每个操作都卡到超时
- BUG-08 响应体上限取 `math.MaxInt64` 时整数回绕，正文被吞且不报错
- BUG-09 纯空白选择器未被拦截，非法表达式直达浏览器
- BUG-11 并发 `Tabs()` 会销毁 `NewTab` 刚建好的标签页

### 测试

- 单元测试 95 个（不依赖浏览器，秒级跑完）；冒烟测试 60 个（真实 Chrome）
- 新增三组覆盖：资源泄漏与长时间稳定性、异常路径（不 panic / 不永久阻塞 / 该报错就报错）、Profile 真实流程
- 建立 `TESTING.md`

---

## 链接

- 仓库：<https://github.com/yymm456/go-drission>
