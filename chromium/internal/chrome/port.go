package chrome

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// FindFreePort 让操作系统分配一个空闲端口。
//
// 这里刻意不用 net.ListenConfig.Listen：本函数被 NewBrowser 调用，而 NewBrowser 是
// 纯构造函数、拿不到 ctx（改签名会波及所有调用方）。绑定 127.0.0.1:0 是本地瞬时操作，
// 没有可取消的等待阶段，硬塞一个 context.Background() 只是为了让检查项闭嘴。
func FindFreePort() (int, error) {
	//nolint:noctx // 见上：瞬时本地 bind，无可取消阶段，且构造函数无 ctx 可用
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("无法分配空闲端口: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// DefaultUserDataDir 返回按端口隔离的默认用户数据目录。
// 参考 DrissionPage 默认行为：放在系统临时目录下（Windows 即 %TEMP%），以端口命名子目录，
// 形如 <temp>/go-drission/userData/<port>，同端口复用时登录态得以保留。
func DefaultUserDataDir(port int) string {
	return filepath.Join(os.TempDir(), "go-drission", "userData", strconv.Itoa(port))
}
