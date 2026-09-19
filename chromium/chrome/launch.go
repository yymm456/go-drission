// Package chrome 负责 Chrome 进程与数据目录的全部底层操作：可执行文件查找、
// 启动与端口探测、进程树回收、端口分配、数据目录排他锁与档案标记。
//
// 本包只向下依赖 config 与 errs，不感知 Browser / Tab 等上层概念。
package chrome

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/yymm456/go-drission/chromium/config"
	"github.com/yymm456/go-drission/chromium/errs"
)

// ResolveChromePath 确定最终用于启动的浏览器可执行文件。
//
// 调用方用 WithChromePath 显式指定时，路径不存在直接报错（不回退，否则拼写错误会被静默掩盖）；
// 未指定时走自动发现，找不到则连同搜索过的全部位置一起返回。
func ResolveChromePath(o *config.Options) (string, error) {
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

// LaunchChrome 启动 Chrome，应用全部配置项，返回 exec.Cmd 以便调用方跟踪进程。
// ctx 控制「等待调试端口就绪」这一阶段：ctx 取消/到期即放弃等待并杀掉已启动的进程。
func LaunchChrome(ctx context.Context, port int, o *config.Options) (*exec.Cmd, error) {
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
	// --test-type 是 --ignore-certificate-errors 的配套项：只给前者时，
	// 部分 Chrome 版本仍会在导航阶段拦下证书错误（表现为 net::ERR_CERT_* 照旧抛出）。
	if o.InsecureTLS {
		args = append(args, "--ignore-certificate-errors", "--test-type")
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

	chromePath, err := ResolveChromePath(o)
	if err != nil {
		return nil, err
	}
	// 不用 exec.CommandContext：Chrome 的生命周期归 Browser 管（Close 里杀进程树），不该跟着连接握手的 ctx 走。
	// 若绑上 ctx，调用方在某个操作超时后取消派生 ctx，就会把正在使用的浏览器一起杀掉。
	//
	//nolint:noctx // 进程生命周期由 Browser 管理，不随调用方 ctx 取消
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
	if IsPortAlive(waitCtx, port) {
		return cmd, nil
	}
	for {
		select {
		case <-waitCtx.Done():
			// 端口未就绪：杀掉已启动的整棵进程树，避免留下僵尸 Chrome 及其子进程占用 profile 目录
			KillProcessTree(cmd)
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("等待端口 %d 就绪被取消: %w", port, err)
			}
			return nil, fmt.Errorf("chrome 已启动，但端口 %d 未在 %s 内就绪", port, timeout)
		case <-ticker.C:
			if IsPortAlive(waitCtx, port) {
				return cmd, nil
			}
		}
	}
}
