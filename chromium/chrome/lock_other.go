//go:build !windows

package chrome

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ProfileLock 是数据目录的 OS 级排他文件锁（unix 用 flock 实现）。
// 语义与 Windows 版一致：已被其他进程持有时 AcquireProfileLock 返回错误；
// 进程退出时内核自动释放，不会留下陈旧锁。
type ProfileLock struct {
	file *os.File
}

// AcquireProfileLock 对锁文件加非阻塞排他 flock。
func AcquireProfileLock(userDataDir string) (*ProfileLock, error) {
	if err := os.MkdirAll(userDataDir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(userDataDir, "go-drission.lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("数据目录 %s 正被另一个实例占用（flock 排他锁已被持有）: %w", userDataDir, err)
	}
	return &ProfileLock{file: f}, nil
}

// release 解除 flock 并关闭锁文件。
func (l *ProfileLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
