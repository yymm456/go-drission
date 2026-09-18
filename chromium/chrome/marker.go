package chrome

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yymm456/go-drission/chromium/config"
	"github.com/yymm456/go-drission/chromium/errs"
)

// 档案标记文件：写在用户数据目录里，记录「本库在这个目录里、用哪个端口启动过 Chrome」。
//
// 为什么需要它：`Connect` 的策略是「端口活着就连，空闲就启动」。可端口上活着的可能是
// **另一个** Chrome（别的 user-data-dir、别人的登录态）。此时
// `OpenPage(ctx, 9223, WithUserDataDir("./profiles/a"))` 会安静地接管它——
// 你以为在用档案 A，实际在用别的东西，Cookie 隔离与多账号方案全部无声失效。
//
// 为什么不用已有的 go-drission.lock：那把锁的语义是「本进程即将启动 Chrome，
// 别的实例别来抢」，作用域是「本库的进程」。手动执行的 Chrome 完全不知道它的存在，
// 因此无法反过来证明端口上那个浏览器用的是哪个目录。
//
// 为什么不用 CDP 查：协议不暴露 user-data-dir；唯一含该信息的 chrome://version
// 需要额外开一个标签页去读 DOM，代价与副作用都远大于读一个本地文件。
const (
	profileMarkerName = "go-drission.profile.json"

	// profileMarkerSchema 用于将来变更格式时识别旧文件；不匹配即视为无标记。
	profileMarkerSchema = 1
)

// profileMarker 是档案标记文件的内容。
type profileMarker struct {
	Schema int `json:"schema"`
	Port   int `json:"port"`
	PID    int `json:"pid,omitempty"`
}

// WriteProfileMarker 在用户数据目录里写下「我们刚用这个端口启动了 Chrome」。
//
// 失败只记日志：目录只读、权限受限都不该阻断启动本身，最坏结果是下次接管时
// 核实不了（那时会走「无法证实」分支）。
func WriteProfileMarker(dir string, port, pid int) error {
	if dir == "" {
		return nil
	}
	data, err := json.Marshal(profileMarker{Schema: profileMarkerSchema, Port: port, PID: pid})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, profileMarkerName), data, 0o600)
}

// readProfileMarker 读取档案标记。不存在、无法读取或格式不符时返回 ok=false，
// 调用方据此进入「无法证实」分支。
func readProfileMarker(dir string) (profileMarker, bool) {
	var m profileMarker
	if dir == "" {
		return m, false
	}
	data, err := os.ReadFile(filepath.Join(dir, profileMarkerName))
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(data, &m); err != nil || m.Schema != profileMarkerSchema {
		return m, false
	}
	return m, true
}

// VerifyPortOwner 核实端口上已有的 Chrome 是否就是调用方期望的那一个。
//
// 判定结果分三种：
//
//   - 目录里有标记且端口一致 → 就是我们启动的，放行。
//   - 目录里有标记但端口不同 → 明确不一致，返回 ErrBrowserMismatch。
//   - 目录里没有标记（Chrome 是手动起的，或不是本库启的）→ 无法证实。
//
// 无法证实时的处理是刻意分开的：
//
//   - 调用方显式指定了 WithUserDataDir → 它对档案有明确预期，核实不了就报错，
//     避免「以为在用档案 A，实际在用别人的浏览器」这种最难排查的静默失败。
//   - 未显式指定（走默认临时目录）→ 仅告警放行，保留「端口活着就直接连」
//     这一被广泛依赖的行为（例如接管自己手动开启 9222 的 Chrome）。
//
// 想无条件跳过请用 WithTrustExistingBrowser。
func VerifyPortOwner(o *config.Options, port int) error {
	if o.TrustExistingBrowser {
		return nil
	}

	dir := o.UserDataDir
	marker, ok := readProfileMarker(dir)

	switch {
	case ok && marker.Port == port:
		o.Logger.Info("已核实端口上的 Chrome 属于本档案", "port", port, "dir", dir)
		return nil

	case ok && marker.Port != port:
		return fmt.Errorf(
			"%w：用户数据目录 %s 的档案标记记录的是端口 %d，但本次要连接的是端口 %d。"+
				"这个端口上的浏览器很可能属于别的档案（或别的程序）。"+
				"确认要接管请改端口，或加 WithTrustExistingBrowser() 显式跳过核实",
			errs.ErrBrowserMismatch, dir, marker.Port, port)

	case o.UserDataDirSet:
		return fmt.Errorf(
			"%w：你指定了 WithUserDataDir(%q)，但该目录里没有本库写入的档案标记，"+
				"因此无法确认端口 %d 上的浏览器真的在用它。"+
				"若那是你手动启动的 Chrome 且确定就是它，加 WithTrustExistingBrowser() 跳过核实",
			errs.ErrBrowserMismatch, dir, port)

	default:
		// 默认临时目录 + 无标记：接管对象本就无法确定，但这是「连上我自己那个 9222」
		// 的常见用法，不能因为核实不了就报错，只把风险讲清楚。
		o.Logger.Warn("端口上已有 Chrome 且无法核实其用户数据目录，将直接接管",
			"port", port, "expectedDir", dir)
		return nil
	}
}
