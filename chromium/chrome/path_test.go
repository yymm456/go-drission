package chrome

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileExists 覆盖路径存在性判断的边界情况。
func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "chrome.exe")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !fileExists(file) {
		t.Error("已存在的文件应判定为存在")
	}
	if fileExists(dir) {
		t.Error("目录不应被判定为文件")
	}
	if fileExists(filepath.Join(dir, "nope.exe")) {
		t.Error("不存在的路径应判定为不存在")
	}
	if fileExists("") {
		t.Error("空路径应判定为不存在")
	}
}

// TestSearchChromeDoesNotPanic 保证自动查找在各种环境下都不会崩。
// 查得到就校验路径真实存在，查不到返回空串即可。
func TestSearchChromeDoesNotPanic(t *testing.T) {
	found, tried := searchChrome()

	if found != "" && !fileExists(found) {
		t.Errorf("自动发现返回了不存在的路径: %s", found)
	}
	if len(tried) == 0 {
		t.Error("查找后应至少记录一个尝试过的路径，否则错误信息无从排查")
	}
}

// TestSearchedChromePathsReturnsCopy 验证返回的是副本：
// 调用方改动返回值不应破坏内部记录。
func TestSearchedChromePathsReturnsCopy(t *testing.T) {
	first := SearchedChromePaths()
	if len(first) == 0 {
		t.Skip("本机未枚举到任何候选路径")
	}
	first[0] = "tampered"

	second := SearchedChromePaths()
	if second[0] == "tampered" {
		t.Error("SearchedChromePaths 返回了内部切片，外部改动污染了内部状态")
	}
}

// TestRefreshChromePath 验证缓存刷新后能重新查找（不校验结果，只保证不 panic 且状态合法）。
func TestRefreshChromePath(t *testing.T) {
	before := findChrome()
	RefreshChromePath()
	after := findChrome()

	if before != "" && after != before {
		t.Errorf("同一台机器上两次查找结果应一致: %q vs %q", before, after)
	}
}
