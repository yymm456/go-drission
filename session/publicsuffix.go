package session

import (
	"strings"

	"golang.org/x/net/publicsuffix"
)

// isPublicSuffix 判断域名本身是否是一个公共后缀（com、co.uk、github.io ...）。
//
// 为什么要单独判一次：RFC 6265 允许站点把 Cookie 的 Domain 设成自己的上级域，
// 但禁止设成公共后缀——否则 example.com 就能给所有 .com 域写 Cookie。
// 标准库的 net/http/cookiejar 依赖同一张 publicsuffix 表，本包自实现 Jar
// 时容易漏掉这一步，漏掉的后果是「跨站 Cookie 投毒」。
//
// 判定规则：publicsuffix.PublicSuffix 返回该主机的公共后缀，若它等于主机本身，
// 说明这个「域名」整个就是个后缀（com / co.uk / github.io），必须拒收。
// 例如：
//
//	com        -> PublicSuffix("com") == "com"        -> 拒绝
//	co.uk      -> PublicSuffix("co.uk") == "co.uk"    -> 拒绝
//	example.com-> PublicSuffix("example.com") == "com" -> 接受
//
// 注意 IP 字面量（127.0.0.1）：publicsuffix 会把它整体当作后缀返回，
// 但按 RFC 6265，IP 地址只能作为 host-only Cookie，本来就带前导点。
// 这里对 IP 直接放行，交由调用方通过 hostOnly 语义约束。
func isPublicSuffix(domain string) bool {
	if domain == "" {
		return true
	}
	if isIPLiteral(domain) {
		return false
	}
	suffix, _ := publicsuffix.PublicSuffix(domain)
	return strings.EqualFold(suffix, domain)
}

// isIPLiteral 判断 host 是否为 IPv4 / IPv6 字面量。
// IPv6 在 canonicalHost 里已被剥掉方括号，因此这里按「含冒号」即视为 IPv6。
func isIPLiteral(host string) bool {
	if strings.Contains(host, ":") {
		return true
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if (c >= '0' && c <= '9') || c == '.' {
			continue
		}
		return false
	}
	return host != "" && strings.Contains(host, ".")
}
