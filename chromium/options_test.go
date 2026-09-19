package chromium

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yymm456/go-drission/chromium/chrome"
	"github.com/yymm456/go-drission/chromium/config"
)

// TestWithDefaultTimeoutOption 覆盖 Option 的默认取值与边界处理。
func TestWithDefaultTimeoutOption(t *testing.T) {
	o := config.Defaults()
	if o.DefaultTimeout != config.DefaultTabTimeout {
		t.Errorf("默认超时应为 %v，实际 %v", config.DefaultTabTimeout, o.DefaultTimeout)
	}
	if o.AntiDetect {
		t.Error("反检测应默认关闭")
	}

	WithDefaultTimeout(0)(o)
	if o.DefaultTimeout != 0 {
		t.Errorf("传 0 应关闭内置超时，实际 %v", o.DefaultTimeout)
	}
	WithDefaultTimeout(-5 * time.Second)(o)
	if o.DefaultTimeout != 0 {
		t.Errorf("负数应归零，实际 %v", o.DefaultTimeout)
	}
	WithAntiDetect(false)(o)
	if o.AntiDetect {
		t.Error("WithAntiDetect(false) 未生效")
	}
}

// TestInsecureTLSOption 覆盖跳过证书校验的开关。
func TestInsecureTLSOption(t *testing.T) {
	o := config.Defaults()
	if o.InsecureTLS {
		t.Error("跳过证书校验应默认关闭")
	}
	WithInsecureTLS()(o)
	if !o.InsecureTLS {
		t.Error("WithInsecureTLS() 未生效")
	}
}

// TestWithChromePathExplicit 区分「未指定」与「显式指定」：
// 显式指定一个不存在的路径时，必须报错而不是悄悄回退到自动发现。
func TestWithChromePathExplicit(t *testing.T) {
	o := config.Defaults()
	if o.ChromePathSet {
		t.Error("未调用 WithChromePath 时 chromePathSet 应为 false")
	}

	WithChromePath("  ")(o)
	if o.ChromePathSet {
		t.Error("空白路径不应被视为显式指定")
	}

	WithChromePath(filepath.Join(t.TempDir(), "not-exists.exe"))(o)
	if !o.ChromePathSet {
		t.Fatal("显式指定后 chromePathSet 应为 true")
	}
	if _, err := chrome.ResolveChromePath(o); err == nil {
		t.Error("显式指定的路径不存在时应报错，不应静默回退到自动发现")
	}
}
