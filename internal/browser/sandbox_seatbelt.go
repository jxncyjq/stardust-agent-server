package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// seatbeltBinary 是 macOS 上的外层沙箱：`sandbox-exec`（Seatbelt）。
//
// Apple 把它标了 deprecated 很多年，却仍随系统发，且是**不需要签名、不需要
// entitlements、不需要特权**就能包住一个任意子进程的唯一办法。App Sandbox 是另一回
// 事：它要求这个进程自己被签名并带上 entitlements，那约束的是 agent，不是浏览器。
const seatbeltBinary = "/usr/bin/sandbox-exec"

// seatbeltSpec 是包一个浏览器进程需要知道的东西。
type seatbeltSpec struct {
	// UserDataDir 是浏览器唯一需要**写**的自有目录（profile、cookies、缓存）。
	UserDataDir string
	// TempDir 是本用户的临时目录（$TMPDIR）。Chromium 一定要写它，见 seatbeltProfile。
	TempDir string
	// OnlyLoopbackEgress 掐掉直连外网，只留回环——浏览器的出网口是本机回环上的
	// 代理（见 egressproxy.go）。真机探针实测这么关之后浏览器照常起来。
	OnlyLoopbackEgress bool
}

// seatbeltProfile 拼出 SBPL（Seatbelt 的 profile 语言）。
//
// 形状与 Linux 那份 bwrap profile 一致：**整盘可读、只有自己那几个目录可写**。真正
// 要防的是写；读系统字体、证书、共享库一一列举既列不全，也会随系统版本漂移。
//
// 逐条的取舍，全部有真机探针的依据（八轮，见 sandbox_darwin_probe_test.go）：
//
//   - `(deny file-write*)` 打底，再逐条放行。第一版把 /private/tmp 与
//     /private/var/folders **整片**放行，探针实测「profile 之外的写」照样成功——
//     那层沙箱什么都没挡。一个看着像沙箱、实际不挡事的东西比没有更糟。
//   - 路径必须是**解析过符号链接的真实路径**：macOS 上 /tmp 与 /var 都指向
//     /private/…，而 SBPL 的 subpath 按解析后的路径匹配。写 /var/folders/xx 进
//     profile，内核看到的却是 /private/var/folders/xx——两边对不上，于是连自己的
//     profile 目录都写不了，浏览器一个字都说不出来就退出。
//   - 必须放行**本用户自己的** T 与 C（$TMPDIR 与它旁边的缓存目录）。Chromium 不认
//     TMPDIR 这个环境变量的重定向：探针里把 TMPDIR 指进 profile 目录之后它照样起不来，
//     而放行 T/C 之后立刻就起来了。这两个目录按用户分而不按 app 分，所以这仍然比
//     「只有 profile 目录可写」宽；但比放行整片 /private/var/folders 窄得多——后者
//     还含着别的用户与系统自己的那些。
//   - `(deny network-outbound)` 只留回环：浏览器的全部流量本就该经过本机的出口代理，
//     这层把「绕过代理直连」这条路也堵死。mDNSResponder 那条是解析用的 UNIX socket。
//   - **不**动 `(allow default)` 之外的读与进程操作：Chromium 是多进程的，自己还要
//     再拉起 renderer/GPU 并建立它**自己**的沙箱——探针确认两层可以叠（不需要
//     --no-sandbox），把内层砸掉反而是净损失。
func seatbeltProfile(spec seatbeltSpec) (string, error) {
	if strings.TrimSpace(spec.UserDataDir) == "" {
		return "", fmt.Errorf("the browser needs a writable user data directory to run under sandbox-exec")
	}
	userDataDir, err := filepath.Abs(spec.UserDataDir)
	if err != nil {
		return "", fmt.Errorf("resolve the browser user data dir %q: %w", spec.UserDataDir, err)
	}
	// 先把目录建出来：EvalSymlinks 只对**已存在**的路径有意义，而真正创建 user-data-dir
	// 的是 Chromium 自己——在沙箱里那一步永远轮不到（同 bwrap 那边的理由）。
	if err := os.MkdirAll(userDataDir, 0o700); err != nil {
		return "", fmt.Errorf("create the browser user data dir %q: %w", userDataDir, err)
	}
	temp := strings.TrimSpace(spec.TempDir)
	if temp == "" {
		return "", fmt.Errorf("the browser needs the user temp directory to run under sandbox-exec: " +
			"Chromium writes there regardless of TMPDIR")
	}

	writable := []string{resolveForSeatbelt(userDataDir)}
	userTemp := resolveForSeatbelt(strings.TrimSuffix(temp, string(os.PathSeparator)))
	writable = append(writable, userTemp)
	// 缓存目录是 T 旁边的 C：/private/var/folders/<xx>/<yyy>/{T,C}。
	writable = append(writable, filepath.Join(filepath.Dir(userTemp), "C"))

	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*\n")
	for _, path := range writable {
		fmt.Fprintf(&b, "  (subpath %q)\n", path)
	}
	b.WriteString("  (literal \"/dev/null\")\n")
	b.WriteString("  (literal \"/dev/dtracehelper\")\n")
	b.WriteString("  (regex #\"^/dev/tty\"))\n")
	if spec.OnlyLoopbackEgress {
		b.WriteString("(deny network-outbound)\n")
		b.WriteString("(allow network-outbound (remote ip \"localhost:*\"))\n")
		b.WriteString("(allow network-outbound (literal \"/private/var/run/mDNSResponder\"))\n")
	}
	return b.String(), nil
}

// resolveForSeatbelt 把路径解析到真实位置；解析不了就原样返回（那种路径此刻还不存在，
// 让 sandbox-exec 自己去说，而不是在这里猜一个）。
func resolveForSeatbelt(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// seatbeltUnavailableError 说清楚缺了什么、以及为什么不退回「不带沙箱照常跑」。
//
// 与 Linux 侧同一条策略：一个**以为自己被沙箱包着、实际没有**的部署，比一个起不来的
// 部署危险得多。
func seatbeltUnavailableError(err error) error {
	return fmt.Errorf("the browser must run inside the macOS sandbox, but %s is unusable: %w\n"+
		"sandbox-exec ships with macOS; if it is missing or blocked, this machine cannot run the "+
		"agent browser. Set browser.enabled=false to turn the browser off instead",
		seatbeltBinary, err)
}
