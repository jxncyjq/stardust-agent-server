package browser

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// 外层沙箱的 profile 是一份**安全决定的清单**，而它最容易出的错不是崩，是悄悄少了
// 一条：少一个 --unshare 就少一层隔离，多一个可写路径就多一个逃逸面，而浏览器照常
// 打开，没有任何症状。所以逐条钉住。

func wrapForTest(t *testing.T, spec bubblewrapSpec) []string {
	t.Helper()

	args, err := bubblewrapArgs(spec, []string{"/opt/chrome", "--headless=new"})
	if err != nil {
		t.Fatalf("bubblewrapArgs: %v", err)
	}
	return args
}

func TestTheSandboxMakesTheFilesystemReadOnlyExceptTheProfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	args := wrapForTest(t, bubblewrapSpec{UserDataDir: dir})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ro-bind / /") {
		t.Error("the filesystem is not bound read-only; a compromised renderer could write anywhere it can reach")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !strings.Contains(joined, "--bind "+abs+" "+abs) {
		t.Errorf("the profile directory is not writable; the browser cannot store cookies or cache: %s", joined)
	}
	if !strings.Contains(joined, "--tmpfs /tmp") {
		t.Error("/tmp is shared with the host; the browser's shared-memory and lock files leak both ways")
	}
	// /dev/shm 不是可选项：--dev 给的最小 devtmpfs 里没有它，而 Chromium 的
	// Crashpad 初始化就要用——缺了它浏览器启动即崩，且崩得没有一句有用的错误。
	if !strings.Contains(joined, "--tmpfs /dev/shm") {
		t.Error("/dev/shm is missing; Chromium crashes during Crashpad initialization with no useful error")
	}
}

func TestTheSandboxDropsEveryNamespaceExceptTheNetwork(t *testing.T) {
	t.Parallel()

	args := wrapForTest(t, bubblewrapSpec{UserDataDir: t.TempDir()})
	joined := strings.Join(args, " ")

	for _, flag := range []string{"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-uts"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("%s is missing; that is one layer of isolation gone with no visible symptom", flag)
		}
	}
	// 网络**故意**不隔离：浏览器的全部流量要走本机回环上的出口代理，而新的网络
	// 命名空间有它自己的回环，代理会变得不可达——那等于用一层沙箱换掉了 SSRF 防护。
	if strings.Contains(joined, "--unshare-net") {
		t.Error("the network namespace is unshared; the egress proxy on loopback becomes unreachable and " +
			"SSRF protection is silently traded away for this sandbox")
	}
}

func TestTheSandboxDiesWithTheAgent(t *testing.T) {
	t.Parallel()

	args := wrapForTest(t, bubblewrapSpec{UserDataDir: t.TempDir()})
	if !strings.Contains(strings.Join(args, " "), "--die-with-parent") {
		t.Error("--die-with-parent is missing; a crashed agent leaves a sandboxed browser running")
	}
}

func TestTheWrappedCommandComesLast(t *testing.T) {
	t.Parallel()

	args := wrapForTest(t, bubblewrapSpec{UserDataDir: t.TempDir()})
	dash := -1
	for i, arg := range args {
		if arg == "--" {
			dash = i
			break
		}
	}
	if dash < 0 {
		t.Fatal("no -- separator; bwrap would read the browser's own flags as its own")
	}
	got := args[dash+1:]
	if len(got) != 2 || got[0] != "/opt/chrome" || got[1] != "--headless=new" {
		t.Errorf("command after -- = %v, want the browser command verbatim", got)
	}
}

func TestExtraWritablePathsAreBoundWritable(t *testing.T) {
	t.Parallel()

	downloads := t.TempDir()
	args := wrapForTest(t, bubblewrapSpec{UserDataDir: t.TempDir(), ExtraWritable: []string{downloads, "  "}})
	abs, err := filepath.Abs(downloads)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--bind "+abs+" "+abs) {
		t.Errorf("the extra writable path was dropped: %v", args)
	}
}

// TestASandboxWithNowhereToWriteIsRefused：没有可写目录的浏览器起不来，而那种失败
// 发生在 Chromium 内部，看起来像「浏览器坏了」。在拼参数这一步就说清楚。
func TestASandboxWithNowhereToWriteIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := bubblewrapArgs(bubblewrapSpec{}, []string{"/opt/chrome"}); err == nil {
		t.Error("a sandbox with no writable user data dir was accepted")
	}
	if _, err := bubblewrapArgs(bubblewrapSpec{UserDataDir: t.TempDir()}, nil); err == nil {
		t.Error("a sandbox with nothing to run was accepted")
	}
}

// TestTheProbeActuallyExercisesAUserNamespace: 光看文件在不在不够——Ubuntu 24.04
// 起未特权 user namespace 被 AppArmor 默认挡掉，bwrap 装着却会在
// 「setting up uid map: Permission denied」上失败。探测必须真的去建一个 user
// namespace，否则它只能证明「文件存在」。
func TestTheProbeActuallyExercisesAUserNamespace(t *testing.T) {
	t.Parallel()

	joined := strings.Join(bubblewrapProbeArgs(), " ")
	if !strings.Contains(joined, "--unshare-user") {
		t.Error("the probe does not create a user namespace, so it cannot detect the case it exists for")
	}
}

func TestTheUnavailableErrorTellsYouWhatToInstall(t *testing.T) {
	t.Parallel()

	err := bubblewrapUnavailableError(errors.New("exec: \"bwrap\": executable file not found in $PATH"),
		"bwrap: setting up uid map: Permission denied")

	for _, want := range []string{"bubblewrap", "apparmor_restrict_unprivileged_userns", "uid map"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; this refusal blocks the whole browser feature, so it "+
				"has to say how to fix it", err, want)
		}
	}
}
