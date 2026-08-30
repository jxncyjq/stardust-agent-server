package browser

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// bubblewrapBinary 是外层沙箱依赖的那个程序。
//
// 选 bubblewrap 而不是自己拼 namespaces：它是 Flatpak 与几乎所有桌面沙箱在用的
// 那一套，处理了一堆自己写会踩的细节（uid/gid map、挂载传播、seccomp、把
// setuid 位摘掉）。代价是多一个部署依赖，而这正是「缺了就拒绝启动」这条策略
// 要交换的东西：一个**以为自己被沙箱包着、实际没有**的部署，比一个起不来的部署
// 危险得多。
const bubblewrapBinary = "bwrap"

// bubblewrapSpec 是包一个浏览器进程需要知道的路径。
type bubblewrapSpec struct {
	// UserDataDir 是浏览器唯一需要**写**的地方（profile、cookies、缓存）。
	UserDataDir string
	// ExtraWritable 是别的必须可写的路径（下载目录之类）。可空。
	ExtraWritable []string
}

// bubblewrapArgs 拼出 bwrap 的参数，`--` 之后接原来的命令行。
//
// 这份 profile 的取舍逐条写在这里，因为它们决定了这层沙箱到底挡住了什么：
//
//   - `--ro-bind / /`：整个文件系统只读可见。浏览器要读系统字体、证书、共享库，
//     一一列举既列不全也会随发行版漂移；真正要防的是**写**。
//   - `--bind <user-data-dir>`：唯一可写的地方。一个被攻破的渲染进程能改的东西
//     被限制在这个会话自己的 profile 里。
//   - `--tmpfs /tmp`：临时目录是每次一新的空目录，不与宿主共享——Chromium 在
//     /tmp 里留的共享内存与锁文件不该被别的进程看见，反过来也一样。
//   - `--dev /dev` + `--proc /proc`：给一套干净的 /dev 与只看得见自己的 /proc。
//   - `--unshare-user --unshare-ipc --unshare-pid --unshare-uts`：换掉除网络以外
//     的命名空间。
//   - **不** `--unshare-net`：浏览器的全部流量要经过本机回环上的出口代理（见
//     egressproxy.go），而新的网络命名空间有它自己的回环，代理会变得不可达——
//     那样等于用一层沙箱换掉了 SSRF 防护。网络的边界由代理来守。
//   - `--die-with-parent`：agent 一死，沙箱里的进程跟着走。与 Linux 的
//     Pdeathsig 是两条独立的保险，都便宜。
//   - `--new-session`：不继承控制终端，避免 TIOCSTI 那类把输入塞回终端的把戏。
func bubblewrapArgs(spec bubblewrapSpec, command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("no command to wrap")
	}
	if strings.TrimSpace(spec.UserDataDir) == "" {
		return nil, fmt.Errorf("the browser needs a writable user data directory to run under bwrap")
	}
	userDataDir, err := filepath.Abs(spec.UserDataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve the browser user data dir %q: %w", spec.UserDataDir, err)
	}

	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--bind", userDataDir, userDataDir,
	}
	for _, path := range spec.ExtraWritable {
		if strings.TrimSpace(path) == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve writable path %q: %w", path, err)
		}
		args = append(args, "--bind", abs, abs)
	}
	args = append(args,
		"--unshare-user",
		"--unshare-ipc",
		"--unshare-pid",
		"--unshare-uts",
		"--die-with-parent",
		"--new-session",
		"--",
	)
	return append(args, command...), nil
}

// bubblewrapProbeArgs 是一次最便宜的「这台机器上 bwrap 到底能不能用」的探测。
//
// 光看文件在不在不够：Ubuntu 24.04 起，未特权的 user namespace 被 AppArmor 默认
// 挡掉（`kernel.apparmor_restrict_unprivileged_userns=1`），bwrap 装着却会在
// 「setting up uid map: Permission denied」上失败。那个失败必须在**启动浏览器之前**
// 就发现并说清楚，否则运维看到的是一次莫名其妙的浏览器起不来。
func bubblewrapProbeArgs() []string {
	return []string{"--ro-bind", "/", "/", "--unshare-user", "--", "/bin/true"}
}

// bubblewrapUnavailableError 说清楚缺的是什么、怎么补。
//
// 报错要能直接照做：这条策略会让一台没装 bwrap 的机器彻底用不了浏览器功能，那么
// 至少要让读到它的人一眼知道装什么。
func bubblewrapUnavailableError(reason error, output string) error {
	msg := fmt.Sprintf("the browser needs %s for its outer sandbox and it is not usable on this machine: %v",
		bubblewrapBinary, reason)
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		msg += "\n" + trimmed
	}
	return fmt.Errorf("%s\n"+
		"install it (apt install bubblewrap / dnf install bubblewrap / apk add bubblewrap), and note that\n"+
		"Ubuntu 24.04 and later also restrict unprivileged user namespaces by default — either allow them\n"+
		"(sysctl -w kernel.apparmor_restrict_unprivileged_userns=0) or ship an AppArmor profile for this agent",
		msg)
}

// lookBubblewrap 找到 bwrap 的绝对路径。抽成变量是为了在测试里替换。
var lookBubblewrap = func() (string, error) { return exec.LookPath(bubblewrapBinary) }

// userDataDirFromArgs 从 Chromium 的命令行里挑出 --user-data-dir 的值。
//
// 沙箱要知道**哪一个目录必须可写**，而那个目录是启动参数决定的（launcher 每次挑一
// 个临时目录）。读参数而不是另外传一遍，是为了不出现「参数里是 A、沙箱开的是 B」
// 这种两处各说一套的错——那种错的症状是浏览器起来了但存不下任何东西。
func userDataDirFromArgs(args []string) string {
	const flag = "--user-data-dir="
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, flag); ok {
			return value
		}
	}
	return ""
}
