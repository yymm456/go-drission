package chromium

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

func isPortAlive(port int) bool {
	// 1. 快速 TCP 探测
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()

	// 2. 确认是 Chrome DevTools 端点，而非其他进程恰好占用端口
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
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

// launchChrome 启动 Chrome，应用全部配置项，返回 exec.Cmd 以便调用方跟踪进程
func launchChrome(port int, o *options) (*exec.Cmd, error) {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + o.userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--noerrdialogs", // 自动化场景不弹模态错误框（如数据目录占用），失败统一走返回错误
	}

	if o.headless {
		args = append(args, "--headless=new")
	}
	if o.windowSize != "" {
		args = append(args, "--window-size="+o.windowSize)
	}
	if o.userAgent != "" {
		args = append(args, "--user-agent="+o.userAgent)
	}
	if o.proxy != "" {
		args = append(args, "--proxy-server="+o.proxy)
	}
	// 追加自定义启动参数
	for _, f := range o.extraFlags {
		if f.value == "" {
			args = append(args, "--"+f.name)
		} else {
			args = append(args, "--"+f.name+"="+f.value)
		}
	}
	cmd := exec.Command(o.chromePath, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}

	// 等待调试端口就绪：超时时长遵循 connectTimeout（默认 10s）。
	// 首次启动全新用户数据目录时 Chrome 冷启动较慢，可通过 WithConnectTimeout 放大。
	timeout := o.connectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	const interval = 200 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isPortAlive(port) {
			return cmd, nil
		}
		time.Sleep(interval)
	}
	// 端口未就绪：杀掉已启动的整棵进程树，避免留下僵尸 Chrome 及其子进程占用 profile 目录
	killProcessTree(cmd)
	return nil, fmt.Errorf("chrome 已启动，但端口 %d 未在 %s 内就绪", port, timeout)
}

func defaultChromePath() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Program Files\Google\Chrome\Application\chrome.exe`
	case "darwin":
		return "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	default:
		return "google-chrome"
	}
}

// defaultUserDataDir 返回按端口隔离的默认用户数据目录。
// 参考 DrissionPage 默认行为：放在系统临时目录下（Windows 即 %TEMP%），以端口命名子目录，
// 形如 <temp>/go-drission/userData/<port>，同端口复用时登录态得以保留。
func defaultUserDataDir(port int) string {
	return filepath.Join(os.TempDir(), "go-drission", "userData", strconv.Itoa(port))
}
