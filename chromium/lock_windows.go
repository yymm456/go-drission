//go:build windows

package chromium

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// profileLock 是数据目录的 OS 级排他文件锁。
// 两个实例并发用同一 user-data-dir 启动 Chrome 时，后启动的 Chrome 会弹
// 「无法对其数据目录执行读写操作」对话框；持有本锁可把这种竞态提前转化为明确的 Go 错误。
// 锁由 OS 维护：进程退出（含崩溃）时句柄自动关闭，不会留下陈旧锁。
type profileLock struct {
	path   string
	handle syscall.Handle
}

// acquireProfileLock 以独占方式（share mode = 0，禁止任何其他进程并发打开）打开锁文件。
// 已被其他进程持有时返回错误。
func acquireProfileLock(userDataDir string) (*profileLock, error) {
	if err := os.MkdirAll(userDataDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(userDataDir, "go-drission.lock")
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(ptr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // 不共享：其他进程的任何打开请求都会失败
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, fmt.Errorf("数据目录 %s 正被另一个实例占用（锁文件被独占打开）: %w", userDataDir, err)
	}
	return &profileLock{path: path, handle: h}, nil
}

// release 关闭锁句柄，允许其他实例获取该数据目录。
func (l *profileLock) release() {
	if l == nil || l.handle == syscall.InvalidHandle {
		return
	}
	_ = syscall.CloseHandle(l.handle)
	l.handle = syscall.InvalidHandle
}
