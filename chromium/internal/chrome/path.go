package chrome

import (
	"os"
	"sync"
)

// 浏览器路径查找结果在进程内缓存：枚举候选 + 查注册表（Windows）有固定开销，
// 而 NewBrowser / ProfileManager.Open 可能被频繁调用，没必要每次重新搜一遍。
// 用 chromePathMu 统一保护三个缓存变量：RefreshChromePath 会重置它们，若不加锁，
// 与并发的 findChrome 读写会构成数据竞争（go test -race 可复现）。
var (
	chromePathMu     sync.Mutex
	chromePathLoaded bool
	chromePathCached string
	chromePathTried  []string
)

// ensureChromeLoadedLocked 首次调用时执行查找并缓存，之后直接复用。
// 必须在持有 chromePathMu 时调用。查找含文件 I/O 与注册表查询，但只在首次发生，
// 与旧的 sync.Once 语义一致（首个调用者做 I/O，其余等待）。
func ensureChromeLoadedLocked() {
	if !chromePathLoaded {
		chromePathCached, chromePathTried = searchChrome()
		chromePathLoaded = true
	}
}

// fileExists 判断路径存在且是普通文件。
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// searchChrome 按候选顺序查找第一个真实存在的浏览器可执行文件。
// 先查平台常见安装路径，未命中再用注册表（Windows）/ PATH（Linux）兜底。
func searchChrome() (string, []string) {
	tried := make([]string, 0, 16)

	candidates := append(chromeCandidates(), registryCandidates()...)
	for _, p := range candidates {
		if p == "" {
			continue
		}
		tried = append(tried, p)
		if fileExists(p) {
			return p, tried
		}
	}
	return "", tried
}

// findChrome 返回本机可用的浏览器路径；找不到时返回空字符串。
func findChrome() string {
	chromePathMu.Lock()
	defer chromePathMu.Unlock()
	ensureChromeLoadedLocked()
	return chromePathCached
}

// SearchedChromePaths 返回最近一次自动查找时枚举过的全部路径。
// 主要用于排查「找不到浏览器」：错误里会列出这些位置，便于确认真实安装路径。
func SearchedChromePaths() []string {
	chromePathMu.Lock()
	defer chromePathMu.Unlock()
	ensureChromeLoadedLocked()
	out := make([]string, len(chromePathTried))
	copy(out, chromePathTried)
	return out
}

// RefreshChromePath 丢弃缓存的浏览器路径，下次查找时重新枚举。
// 场景：进程启动后用户才安装浏览器，或安装位置发生了变化。
func RefreshChromePath() {
	chromePathMu.Lock()
	defer chromePathMu.Unlock()
	chromePathLoaded = false
	chromePathCached = ""
	chromePathTried = nil
}
