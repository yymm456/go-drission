//go:build !windows

package chromium

import (
	"os"
	"os/exec"
	"path/filepath"
)

// chromeCandidates 返回 macOS / Linux 上按优先级排列的浏览器可执行文件路径候选。
func chromeCandidates() []string {
	var list []string

	// macOS：应用程序目录（系统级 + 用户级）
	if home, err := os.UserHomeDir(); err == nil {
		list = append(list,
			filepath.Join(home, "Applications", "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
			filepath.Join(home, "Applications", "Chromium.app", "Contents", "MacOS", "Chromium"),
		)
	}
	list = append(list,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	)

	// Linux：交给 PATH 解析，覆盖各发行版的不同命名
	for _, name := range []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"microsoft-edge",
		"microsoft-edge-stable",
		"brave-browser",
	} {
		if p, err := exec.LookPath(name); err == nil {
			list = append(list, p)
		}
	}

	return list
}

// registryCandidates 在非 Windows 平台上无对应机制，返回空。
func registryCandidates() []string { return nil }
