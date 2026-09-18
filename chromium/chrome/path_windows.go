//go:build windows

package chrome

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// chromeCandidates 返回 Windows 上按优先级排列的浏览器可执行文件路径候选。
//
// 顺序：用户级 Chrome（免管理员安装，最常见）→ 系统级 Chrome → Chrome SxS/Canary
// → Edge → Brave → Chromium。覆盖本机常见安装方式。
func chromeCandidates() []string {
	var list []string
	add := func(elem ...string) {
		list = append(list, filepath.Join(elem...))
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	programFiles := os.Getenv("PROGRAMFILES")
	programFilesX86 := os.Getenv("PROGRAMFILES(X86)")

	if localAppData != "" {
		add(localAppData, `Google\Chrome\Application\chrome.exe`)
		add(localAppData, `Google\Chrome SxS\Application\chrome.exe`)
		add(localAppData, `BraveSoftware\Brave-Browser\Application\brave.exe`)
		add(localAppData, `Chromium\Application\chrome.exe`)
	}
	if programFiles != "" {
		add(programFiles, `Google\Chrome\Application\chrome.exe`)
		add(programFiles, `Microsoft\Edge\Application\msedge.exe`)
		add(programFiles, `BraveSoftware\Brave-Browser\Application\brave.exe`)
	}
	if programFilesX86 != "" {
		add(programFilesX86, `Google\Chrome\Application\chrome.exe`)
		add(programFilesX86, `Microsoft\Edge\Application\msedge.exe`)
	}

	return list
}

// registryCandidates 通过 App Paths 注册表项兜底查询浏览器真实安装路径。
//
// 有些机器把浏览器装到了自定义目录（企业镜像、绿色版、便携版），硬编码路径枚举不到，
// 但安装程序通常会写 App Paths。查询失败（无权限 / 未安装 / reg 不可用）时静默跳过。
func registryCandidates() []string {
	keys := []string{
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`,
		`HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`,
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`,
		`HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`,
	}

	var out []string
	for _, key := range keys {
		if p := queryAppPath(key); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// regQueryTimeout 是读取注册表的兜底超时。
//
// reg.exe 正常几毫秒就返回，但注册表被安全软件挂钩或 hive 损坏时会卡住。
// 这个查询发生在浏览器自动发现的关键路径上，卡住就等于整个启动流程卡住，
// 所以给它一个明确上限。
const regQueryTimeout = 3 * time.Second

// queryAppPath 读取 App Paths 项的默认值（/ve），即可执行文件完整路径。
func queryAppPath(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), regQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "reg", "query", key, "/ve")
	// 隐藏控制台窗口：本库常被 GUI 程序嵌入，弹黑框会干扰调用方
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// 输出形如：
	//     (Default)    REG_SZ    C:\Program Files\Google\Chrome\Application\chrome.exe
	for line := range strings.SplitSeq(string(output), "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "REG_SZ")
		if idx < 0 {
			continue
		}
		p := strings.TrimSpace(line[idx+len("REG_SZ"):])
		p = strings.Trim(p, `"`)
		// 只接受指向 chrome/edge 的路径，避免取到 IE 等无关程序的默认值
		lower := strings.ToLower(p)
		if strings.HasSuffix(lower, ".exe") && fileExists(p) {
			return p
		}
	}
	return ""
}
