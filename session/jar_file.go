package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExportJSON 把容器内的全部 Cookie 序列化成 JSON（带缩进，便于人工查看与版本管理）。
func (j *Jar) ExportJSON() ([]byte, error) {
	return json.MarshalIndent(j.All(), "", "  ")
}

// ExportJSONFor 只导出访问 rawURL 时会用到的 Cookie。
//
// 与「浏览器接力」配合时的推荐入口：产物可以直接交给
// chromium 包的 Tab.ImportCookiesJSON / Tab.LoginWithCookies 使用。
// 之所以按 URL 过滤，是因为把 A 站点的全量 Cookie（含其它域）灌进浏览器既无必要，
// 也会把无关域的登录态一并带过去。
func (j *Jar) ExportJSONFor(rawURL string) ([]byte, error) {
	items, err := j.CookiesFor(rawURL)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(items, "", "  ")
}

// ImportJSON 从 JSON 恢复 Cookie。字段与 chromium.Cookie 一致，
// 因此浏览器导出的 cookies.json 可以直接喂进来。
func (j *Jar) ImportJSON(data []byte) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil // 空文件视为无可导入内容，不报错
	}
	var items []CookieItem
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCookieJSON, err)
	}
	j.Load(items)
	return nil
}

// SaveFile 把 Cookie 持久化到文件（父目录不存在时自动创建）。
// 文件权限 0600：Cookie 里通常带会话令牌，不该被同机其他用户读到。
func (j *Jar) SaveFile(path string) error {
	data, err := j.ExportJSON()
	if err != nil {
		return err
	}
	return writeCookieFile(path, data)
}

// SaveFileFor 只把访问 rawURL 时会用到的 Cookie 持久化到文件。
func (j *Jar) SaveFileFor(rawURL, path string) error {
	data, err := j.ExportJSONFor(rawURL)
	if err != nil {
		return err
	}
	return writeCookieFile(path, data)
}

// writeCookieFile 统一 Cookie 落盘行为：自动建父目录 + 0600 权限。
func writeCookieFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("%w: 创建目录 %s: %w", ErrCookieFileWrite, dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCookieFileWrite, path, err)
	}
	return nil
}

// LoadFile 从文件恢复 Cookie；文件不存在时不视为错误（首次运行属常态）。
func (j *Jar) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: %s: %w", ErrCookieFileRead, path, err)
	}
	if len(data) == 0 {
		return nil
	}
	return j.ImportJSON(data)
}

// ---------- Session 级别的便捷封装 ----------

// SaveCookies 把当前会话的 Cookie 存盘。
func (s *Session) SaveCookies(path string) error {
	return s.jar.SaveFile(path)
}

// SaveCookiesFor 只把访问 rawURL 会用到的 Cookie 存盘（便于直接交给浏览器导入）。
func (s *Session) SaveCookiesFor(rawURL, path string) error {
	return s.jar.SaveFileFor(rawURL, path)
}

// LoadCookies 从文件恢复 Cookie 到当前会话。
func (s *Session) LoadCookies(path string) error {
	return s.jar.LoadFile(path)
}

// Cookies 返回会话当前有效的 Cookie 快照（已剔除过期项）。
//
// 「当前会话的 Cookie」直觉上就是还能用的那些，因此这里不返回过期项——
// 需要连过期项一起拿去存盘请用 AllCookies（或在 Jar 层用 ExportJSON / SaveFile）。
func (s *Session) Cookies() []CookieItem {
	return s.jar.Valid()
}

// AllCookies 返回会话的全部 Cookie，含已过期项。用于需要完整镜像底层容器的场景。
func (s *Session) AllCookies() []CookieItem {
	return s.jar.All()
}

// CookiesFor 返回访问 rawURL 时实际会携带的 Cookie 快照。
func (s *Session) CookiesFor(rawURL string) ([]CookieItem, error) {
	return s.jar.CookiesFor(rawURL)
}

// SetCookie 直接写入一条 Cookie（不经过 HTTP 响应），常用于把浏览器里的登录态搬过来。
func (s *Session) SetCookie(item CookieItem) {
	s.jar.Load([]CookieItem{item})
}

// SetCookies 批量写入 Cookie，返回成功写入的条数（空名或空域名会被跳过）。
func (s *Session) SetCookies(items []CookieItem) int {
	return s.jar.Load(items)
}

// ClearCookies 清空会话的 Cookie。
func (s *Session) ClearCookies() {
	s.jar.Clear()
}
