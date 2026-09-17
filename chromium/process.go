package chromium

import (
	"os/exec"
	"runtime"
	"strconv"
)

// killProcessTree 结束 Chrome 进程及其所有子进程。
//
// 为什么不能只用 cmd.Process.Kill()：Chrome 是多进程架构（browser 主进程 +
// renderer / gpu / utility / crashpad handler 等子进程）。在 Windows 上 Kill 只
// 终止主进程，子进程会残留数秒并继续占用 user-data-dir 里的文件锁，导致紧接着的
// 下一次启动报「Chrome 无法对其数据目录执行读写操作」。因此必须杀整棵进程树。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	if runtime.GOOS == "windows" {
		// /F 强制结束，/T 连同子进程一起结束
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
		if err := kill.Run(); err == nil {
			_ = cmd.Wait()
			return
		}
		// taskkill 失败时退回普通 Kill，尽力而为
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
