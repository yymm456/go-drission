package chromium

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yymm456/go-drission/chromium/internal/config"
	"github.com/yymm456/go-drission/chromium/internal/errs"
)

// noProxyTransport 是绕过系统代理的 HTTP transport。
//
// 调试端口与 /json 端点都跑在本机回环地址上，必须直连：一旦走了 HTTP_PROXY
// 环境变量指向的代理，探测请求会被转发到外部代理，导致「明明活着却探测失败」，
// 进而每次都去重新启动一个 Chrome。
var noProxyTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	ResponseHeaderTimeout: 3 * time.Second,
}

// isPortAlive 判断指定端口上是否有一个可用的 Chrome DevTools 端点。
// 探测过程受 ctx 约束：ctx 取消或超时立即返回 false，不会拖慢调用方。
func isPortAlive(ctx context.Context, port int) bool {
	if err := ctx.Err(); err != nil {
		return false
	}

	// 1. 快速 TCP 探测
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(probeCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	// 探测用的连接，关闭失败无补救手段，也没必要上报
	_ = conn.Close()

	// 2. 确认是 Chrome DevTools 端点，而非其他进程恰好占用端口
	client := &http.Client{Timeout: 3 * time.Second, Transport: noProxyTransport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false
	}
	return v.Browser != ""
}

// resolveChromePath 确定最终用于启动的浏览器可执行文件。
//
// 规则：调用方用 WithChromePath 显式指定时，路径不存在就直接报错（不悄悄回退，
// 否则拼写错误会被静默掩盖）；未指定时才走自动发现，找不到则连同搜索过的
// 全部位置一起返回，方便调用方一眼看出该装到哪。
func resolveChromePath(o *config.Options) (string, error) {
	if o.ChromePathSet {
		if !fileExists(o.ChromePath) {
			return "", fmt.Errorf("chromium: WithChromePath 指定的浏览器不存在: %s", o.ChromePath)
		}
		return o.ChromePath, nil
	}
	if p := findChrome(); p != "" {
		return p, nil
	}
	tried := SearchedChromePaths()
	return "", fmt.Errorf("%w：已搜索 %d 个位置:\n  %s\n"+
		"请用 WithChromePath(\"<实际路径>\") 显式指定",
		errs.ErrChromeNotFound, len(tried), strings.Join(tried, "\n  "))
}

// launchChrome 启动 Chrome，应用全部配置项，返回 exec.Cmd 以便调用方跟踪进程。
// ctx 控制「等待调试端口就绪」这一阶段：ctx 取消/到期即放弃等待并杀掉已启动的进程。
func launchChrome(ctx context.Context, port int, o *config.Options) (*exec.Cmd, error) {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + o.UserDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--noerrdialogs", // 自动化场景不弹模态错误框（如数据目录占用），失败统一走返回错误
	}

	// Chrome 111+ 会校验 WebSocket 握手时的 Host/Origin 头，
	// 不加这条在高版本 Chrome 上会出现连接被拒（403 / Rejected an incoming WebSocket connection）。
	args = append(args, "--remote-allow-origins=*")

	// 服务器/容器上 /dev/shm 通常只有 64MB，不加这条 Chrome 会随机崩溃
	args = append(args, "--disable-dev-shm-usage")

	if o.AntiDetect {
		// 抹掉最明显的自动化痕迹：
		//   --disable-blink-features=AutomationControlled  去掉 navigator.webdriver 的底层标记来源
		//   --excludeSwitches=enable-automation            去掉「正受到自动测试软件的控制」提示条
		// navigator.webdriver 本身由新建标签页时的初始化脚本抹除（见 anti_detect.go）
		args = append(args,
			"--disable-blink-features=AutomationControlled",
			"--excludeSwitches=enable-automation",
			"--disable-infobars",
			"--mute-audio",
		)
	}
	if o.Lang != "" {
		args = append(args, "--lang="+o.Lang)
	}
	if o.Headless {
		args = append(args, "--headless=new")
	}
	if o.WindowSize != "" {
		args = append(args, "--window-size="+o.WindowSize)
	}
	if o.UserAgent != "" {
		args = append(args, "--user-agent="+o.UserAgent)
	}
	if o.Proxy != "" {
		args = append(args, "--proxy-server="+o.Proxy)
	}
	// 追加自定义启动参数
	for _, f := range o.ExtraFlags {
		if f.Value == "" {
			args = append(args, "--"+f.Name)
		} else {
			args = append(args, "--"+f.Name+"="+f.Value)
		}
	}

	chromePath, err := resolveChromePath(o)
	if err != nil {
		return nil, err
	}
	// 刻意不用 exec.CommandContext：Chrome 的生命周期归 Browser 管（Close 里杀进程树），
	// 不该跟着连接握手的 ctx 走。若绑上 ctx，调用方在某个操作超时后取消派生 ctx，
	// 就会把正在使用的浏览器一起杀掉。
	//
	//nolint:noctx // 见上：进程生命周期由 Browser 管理，不随调用方 ctx 取消
	cmd := exec.Command(chromePath, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}

	// 等待调试端口就绪：受 ctx 与 connectTimeout 双重约束，取先到者。
	// 首次启动全新用户数据目录时 Chrome 冷启动较慢，可通过 WithConnectTimeout 放大。
	timeout := o.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()

	const interval = 200 * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 先立刻探一次，避免刚启动就被迫白等一个间隔
	if isPortAlive(waitCtx, port) {
		return cmd, nil
	}
	for {
		select {
		case <-waitCtx.Done():
			// 端口未就绪：杀掉已启动的整棵进程树，避免留下僵尸 Chrome 及其子进程占用 profile 目录
			killProcessTree(cmd)
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("等待端口 %d 就绪被取消: %w", port, err)
			}
			return nil, fmt.Errorf("chrome 已启动，但端口 %d 未在 %s 内就绪", port, timeout)
		case <-ticker.C:
			if isPortAlive(waitCtx, port) {
				return cmd, nil
			}
		}
	}
}

// defaultUserDataDir 返回按端口隔离的默认用户数据目录。
// 参考 DrissionPage 默认行为：放在系统临时目录下（Windows 即 %TEMP%），以端口命名子目录，
// 形如 <temp>/go-drission/userData/<port>，同端口复用时登录态得以保留。
func defaultUserDataDir(port int) string {
	return filepath.Join(os.TempDir(), "go-drission", "userData", strconv.Itoa(port))
}
