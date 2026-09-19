# TESTING

go-drission 的测试分层、跑法与当前稳定基线。

分层原则：**单元测试不依赖浏览器，必须秒级跑完；只有真实浏览器才能回答的问题才进 smoke。**
这条决定了下面的目录与命令划分，也决定了 CI 该跑哪些。

---

## 一、测试分层

| 层 | 位置 | 依赖浏览器 | 命令 | 用例数 |
|---|---|---|---|---|
| 单元测试 | `chromium/`、`session/` 下的 `*_test.go` | 否 | `go test ./...` | 102 |
| 冒烟测试 | `smoke/`（`//go:build smoke`） | **是** | `go test -tags smoke ./smoke/...` | 61 |

冒烟用 build tag 隔离的原因：它要真起 Chrome（冷启动 1~2 秒），不该拖慢日常 `go test`。
**本机找不到浏览器时，smoke 用例自动 skip，不误报失败。**

### smoke 内部的分组

| 文件 | 覆盖 |
|---|---|
| `smoke/smoke_test.go` | 选择器、元素操作、iframe（同域/跨域）、Session、反检测、截图、网络监听 |
| `smoke/lifecycle_test.go` | 多标签页、隔离上下文生命周期、BUG-06 / BUG-07 / BUG-11 定点回归 |
| `smoke/lifecycle_abnormal_test.go` | **异常路径**：资源在对面消失后是否 panic / 死锁 / 永久阻塞 |
| `smoke/stability_test.go` | **资源泄漏与稳定性**：Chrome 进程回收、goroutine 增长、档案锁释放、重复启停 |
| `smoke/profile_test.go` | ProfileManager：同名复用、注册表、端口分配、Cookie 隔离、CloseAll |
| `smoke/cookie_relay_test.go` | Cookie 接力与注入校验 |

---

## 二、日常验证命令

按顺序跑，全部通过才算干净：

```bash
gofmt -l .                                   # 必须为空
go build ./...
go vet ./...
go test -count=1 ./...                       # 单元测试，约 3 秒
go test -count=1 -race ./...                 # 并发改动必开 -race
go test -tags smoke -count=1 -timeout 15m ./smoke/...
golangci-lint run ./...                      # 见「六、注意事项」
```

推荐一次跑全（含 race 与 smoke）：

```bash
go test -race -tags smoke -count=1 -timeout 15m ./...
```

### 纯注释类改动的自证

本仓库有过一次事故：一批自称「注释瘦身」的改动里夹带了
`config.Defaults()` 的 `AntiDetect: true → false`，gofmt 与编译器都查不出来。
所以**只改注释也要能自证**：

```bash
git worktree add --detach .workbuddy/tmp/headbase HEAD
.workbuddy/tools/tokencmp/tokencmp.exe tokens .workbuddy/tmp/headbase > /tmp/head.txt
.workbuddy/tools/tokencmp/tokencmp.exe tokens .                      > /tmp/now.txt
diff /tmp/head.txt /tmp/now.txt     # 空 == 除注释外零差异

# 再把 tokens 换成 docs 跑一遍：空 == 导出标识符的文档注释没有变少
```

`tokencmp` 比对的是**剥离注释后的 token 流指纹**（不是 AST 打印，后者会受行号变化干扰）。

---

## 三、长时间稳定性 / 泄漏测试

`smoke/stability_test.go` 的轮次由环境变量控制，默认 5 轮（约 10 秒）以便日常跑。
要按架构文档做长时间观察，只调轮次即可，逻辑完全一致：

```bash
GO_DRISSION_STABILITY_ROUNDS=100 go test -tags smoke -run TestStability -timeout 30m ./smoke/...
```

它观察的是：

- **Chrome 进程回收**：Close 之后调试端口必须连不上（不用 `os.FindProcess` ——
  它在 Windows 上无论进程死活都返回成功，会得出假阴性）；
- **goroutine 不随轮次增长**：先预热取基线，再跑 N 轮比较，余量与轮次无关
  （写成 `rounds*N` 会把「每轮泄漏 N 个」放过去）；
- **档案锁可释放**：关闭后同名档案能再次打开；
- **重复关闭安全**：`Browser.Close()`、`ProfileManager.Close(name)` 调两次都不 panic。

---

## 四、异常路径测试

`smoke/lifecycle_abnormal_test.go` 统一用三条判据：

1. **不 panic** —— 在 goroutine 里 recover，把 panic 转成一条错误报告，
   否则 panic 会掀翻整个测试进程，其它用例结果就读不到了；
2. **不永久阻塞** —— 每个调用卡 `abnormalTimeout`（30s）；
3. **该报错时必须报错** —— 静默成功也算失败，因为它会把调用方骗过去。

覆盖的场景：浏览器关闭后操作、标签页关闭后操作、监听等待中标签页被关、
并发「关浏览器 + 建标签页」、已取消的 ctx、**Chrome 被外部杀掉**、上下文关闭后取标签页。

---

## 五、当前稳定基线

| 项 | 值 |
|---|---|
| Go | go1.26.0 windows/amd64 |
| OS | Windows 10（`RDP-Tcp` 会话，非服务） |
| Chromium | Chrome 153.0.8010.48（`C:\Program Files\Google\Chrome\Application`） |
| golangci-lint | v2.13.2（必须用绝对路径，见下） |
| 单元测试 | 102 个，全通过 |
| 冒烟测试 | 61 个，全通过（约 112 秒，含新增的稳定性/异常/档案用例） |
| `go test -race` | 全通过，无 data race 报告 |

最后一次全量：

```
ok  chromium                1.115s
ok  chromium/browser        1.115s
ok  chromium/chrome         1.432s
ok  chromium/cookie         1.066s
ok  chromium/network        1.440s
ok  chromium/page           1.452s
ok  chromium/profile        1.099s
ok  session                 3.198s
ok  smoke                  110.599s   （-race -tags smoke）
```

---

## 六、注意事项（踩过的坑）

1. **smoke 必须显式指定用户数据目录**。
   不指定时库会按端口在 `%TEMP%\go-drission\userData\<port>` 建目录，
   且刻意不删（真实登录态在里面，替用户删是灾难）。测试侧必须自己管：
   `TestMain` 建、`TestMain` 删。本机曾跑到第 78 次累积 1.1 GB。

2. **先关浏览器，再删用户数据目录**。
   目录里有被 Chrome 独占的文件（如 `CrashpadMetrics-active.pma`），
   顺序反了会报「另一个程序正在使用此文件」。
   用 `t.TempDir()` + `t.Cleanup(b.Close)` 可借 Cleanup 的 LIFO 保证顺序。
   但**档案类用例不要用 `t.TempDir()`**：`CloseAll` 之后 Chrome 退出是异步的，
   `TempDir` 紧接着删必然失败并把用例判红 —— 那是清理时序问题，不是被测代码的问题。
   用 `smoke/stability_test.go` 里的 `tempProfileDir()`（等一下再删，容忍删不掉）。

3. **对某个 Tab 操作时，ctx 必须从那个 `Tab.Ctx` 派生**。
   本库靠 ctx 携带 target 路由。若误用同浏览器里另一个 Tab 的 ctx，
   CDP 命令会路由到那个还活着的页面 —— 于是「关闭后再操作」看起来成功了，
   实际上测的是别的页面。这个坑在写「关闭后」类用例时最容易踩。

4. **golangci-lint 必须用绝对路径**：

   ```bash
   C:/Users/Administrator/.workbuddy/binaries/golangci-lint/golangci-lint.exe run ./...
   ```

   PATH 里那个是旧版（v2.6.1 / go1.25.3），会报
   `Go language version (go1.25) ... lower than the targeted (1.26.0)` ——
   看着像配置坏了，其实是调错了二进制。

5. **冒烟测试会真的弹出/启动 Chrome**。用 `WithHeadless(true)` 时不弹窗，
   但进程确实在跑；调试时想看画面可以临时去掉该选项。

6. `ProfileManager.CloseAll()` 是**终态**操作：之后 `Open` 返回 `ErrProfileClosed`，
   语义同 `sql.DB.Close`。想继续用请新建 manager。重复调用是安全的。
