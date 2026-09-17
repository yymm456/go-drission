package chromium

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

func isPortAlive(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// launchChrome 启动 Chrome，应用全部配置项，返回 exec.Cmd 以便调用方跟踪进程
func launchChrome(port int, o *options) (*exec.Cmd, error) {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + o.userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
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

	for i := 0; i < 50; i++ {
		if isPortAlive(port) {
			return cmd, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 端口未就绪：杀掉已启动的进程，避免留下僵尸 Chrome
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return nil, fmt.Errorf("chrome 已启动，但端口 %d 未在超时时间内就绪", port)
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
