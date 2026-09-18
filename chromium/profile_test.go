package chromium

import (
	"path/filepath"
	"testing"
)

// TestSanitizeProfileName 覆盖档案名到目录名的转换，重点是路径穿越与非法字符。
func TestSanitizeProfileName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"account_001", "account_001"},
		{"user-A", "user-A"},
		// 穿越序列中的 . 与 / 都变成 _，再被 Trim 掉首尾下划线，最终只剩普通目录名
		{"../../etc/passwd", "etc_passwd"},
		{"a/b\\c", "a_b_c"},
		{"用户名", "default"}, // 非 ASCII 全部替换成 _，Trim 后为空 → 回退默认名
		{"__", "default"},  // 去下划线后为空，回退默认名
		{"", "default"},
		{"  ", "default"},
		{"a b", "a_b"},
	}

	for _, c := range cases {
		if got := sanitizeProfileName(c.in); got != c.want {
			t.Errorf("sanitizeProfileName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeProfileNameStaysInsideBaseDir 是安全回归测试：
// 恶意档案名不得拼接出 baseDir 之外的路径。
func TestSanitizeProfileNameStaysInsideBaseDir(t *testing.T) {
	base := filepath.Clean("/tmp/profiles")
	for _, name := range []string{
		"../../etc/passwd",
		"..\\..\\windows\\system32",
		"/absolute/path",
		"....//....//",
	} {
		joined := filepath.Join(base, sanitizeProfileName(name))
		if filepath.Dir(joined) != base {
			t.Errorf("档案名 %q 逃逸出 baseDir: %s", name, joined)
		}
	}
}

// TestTakeAndReleasePort 覆盖端口分配与回收池，确保失败后端口能复用且不会重复归还。
func TestTakeAndReleasePort(t *testing.T) {
	pm := NewProfileManager("profiles", 9300)

	pm.mu.Lock()
	p1 := pm.takePortLocked()
	p2 := pm.takePortLocked()
	pm.mu.Unlock()

	if p1 != 9300 || p2 != 9301 {
		t.Fatalf("端口应按 basePort 递增分配，实际 %d / %d", p1, p2)
	}

	// 归还后应优先复用，而不是继续往后拿
	pm.releasePort(p1)
	pm.releasePort(p1) // 重复归还必须被忽略
	pm.mu.Lock()
	freeLen := len(pm.free)
	p3 := pm.takePortLocked()
	pm.mu.Unlock()

	if freeLen != 1 {
		t.Errorf("重复归还同一端口导致回收池出现重复项，池大小 %d", freeLen)
	}
	if p3 != p1 {
		t.Errorf("归还的端口应被优先复用，实际拿到 %d（期望 %d）", p3, p1)
	}
}
