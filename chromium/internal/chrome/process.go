package chrome

import (
	"os/exec"
	"runtime"
	"strconv"
)

// KillProcessTree 结束 Chrome 进程及其所有子进程。
//
// 为什么不能只用 cmd.Process.Kill()：Chrome 是多进程架构（browser 主进程 +
// renderer / gpu / utility / crashpad handler 等子进程）。在 Windows 上 Kill 只
// 终止主进程，子进程会残留数秒并继续占用 user-data-dir 里的文件锁，导致紧接着的
// 下一次启动报「Chrome 无法对其数据目录执行读写操作」。因此必须杀整棵进程树。
func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid

	if runtime.GOOS == "windows" {
		// /F 强制结束，/T 连同子进程一起结束。
		//
		// 刻意不用 exec.CommandContext：这是 Close 路径上的清理动作，必须执行到底。
		// 若把调用方的 ctx 接进来，恰好 ctx 已过期时 taskkill 会被直接放弃，
		// 残留的子进程会继续占着 user-data-dir 的文件锁。
		//
		//nolint:noctx // 见上：清理动作不可取消，且 KillProcessTree 只拿到 *exec.Cmd
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
