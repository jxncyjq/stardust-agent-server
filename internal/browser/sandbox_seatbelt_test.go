package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试钉的是 profile 的**形状**——它拼得对不对。它证明不了「沙箱确实在路径上」，
// 那件事只有真机能证明（见 takeover_chromium_test.go 里那条按进程树判定的）。
//
// 每一条的依据都是 macOS runner 上的探针实测，不是文档推测：八轮探针的结论写在
// seatbeltProfile 的注释里。

func TestTheProfileConfinesWritesToTheProfileAndTempOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	userData := filepath.Join(dir, "userdata")
	temp := filepath.Join(dir, "T")
	if err := os.MkdirAll(temp, 0o700); err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}

	profile, err := seatbeltProfile(seatbeltSpec{UserDataDir: userData, TempDir: temp})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}

	if !strings.Contains(profile, "(deny file-write*)") {
		t.Errorf("the profile never denies writes, so it confines nothing:\n%s", profile)
	}
	if !strings.Contains(profile, "(allow file-write*") {
		t.Errorf("the profile denies every write, so the browser cannot even keep its own profile:\n%s", profile)
	}
	// 缓存目录（T 旁边的 C）也要在：探针实测缺了它浏览器起不来。
	//
	// 按 profile 自己的写法（%q）来比：Windows 上路径里的反斜杠在 %q 里是转义的，
	// 拿裸路径去 Contains 会永远对不上——那是测试的毛病，不是 profile 的。
	cacheDir := fmt.Sprintf("%q", filepath.Join(filepath.Dir(resolveForSeatbelt(temp)), "C"))
	if !strings.Contains(profile, cacheDir) {
		t.Errorf("the cache directory next to $TMPDIR is missing; the browser will not start:\n%s", profile)
	}
}

// TestTheProfileDoesNotHandOverTheWholeTempTree：整片放行 /private/var/folders 是
// 第一版的写法，探针实测那等于什么都没挡——那底下是**每个 app** 的临时目录。
func TestTheProfileDoesNotHandOverTheWholeTempTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	profile, err := seatbeltProfile(seatbeltSpec{
		UserDataDir: filepath.Join(dir, "userdata"),
		TempDir:     filepath.Join(dir, "T"),
	})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}

	for _, tooWide := range []string{`(subpath "/private/var/folders")`, `(subpath "/private/tmp")`, `(subpath "/")`} {
		if strings.Contains(profile, tooWide) {
			t.Errorf("the profile hands over %s — that is every app's temp directory, not ours:\n%s",
				tooWide, profile)
		}
	}
}

// TestTheProfileUsesResolvedPaths：SBPL 的 subpath 按解析后的真实路径匹配。写一个
// 软链进去，内核就对不上——症状是**连自己的 profile 目录都写不了**，浏览器一个字
// 都说不出来就退出。探针的第二轮正是死在这里。
func TestTheProfileUsesResolvedPaths(t *testing.T) {
	t.Parallel()

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this machine cannot make symlinks: %v", err)
	}
	// Windows 上 os.Symlink 造出来的未必是能当目录用的链接（需要特权与目录标志），
	// 而这条断言针对的是 macOS 的内核行为。**用真的写一次**来判断能不能穿过它——
	// Stat 在 Windows 上会说没问题，MkdirAll 才会失败。
	userData := filepath.Join(link, "userdata")
	if err := os.MkdirAll(userData, 0o700); err != nil {
		t.Skipf("the symlink on this machine cannot be written through: %v", err)
	}

	profile, err := seatbeltProfile(seatbeltSpec{UserDataDir: userData, TempDir: filepath.Join(real, "T")})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}

	resolved := fmt.Sprintf("%q", resolveForSeatbelt(filepath.Join(real, "userdata")))
	if !strings.Contains(profile, resolved) {
		t.Errorf("the profile carries the symlinked path instead of the resolved one;\n"+
			"the kernel matches on the resolved path, so the browser cannot write its own profile:\n%s", profile)
	}
}

// TestLoopbackOnlyEgressIsOptOut：默认不掐外网（部署可能没开代理），开了就只留回环。
func TestLoopbackOnlyEgressIsOptOut(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := seatbeltSpec{UserDataDir: filepath.Join(dir, "u"), TempDir: filepath.Join(dir, "T")}

	off, err := seatbeltProfile(spec)
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if strings.Contains(off, "deny network-outbound") {
		t.Errorf("the default profile cut off the network:\n%s", off)
	}

	spec.OnlyLoopbackEgress = true
	on, err := seatbeltProfile(spec)
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if !strings.Contains(on, "(deny network-outbound)") ||
		!strings.Contains(on, `(allow network-outbound (remote ip "localhost:*"))`) {
		t.Errorf("loopback-only egress was asked for but the profile does not say so:\n%s", on)
	}
}

// TestAMissingTempDirIsRefused：Chromium **不认** TMPDIR 的重定向（探针实测：指进
// profile 目录之后它照样起不来）。所以没有真实的临时目录就没法拼出能用的 profile，
// 这时要报错，而不是拼一份「浏览器起不来」的 profile 交出去。
func TestAMissingTempDirIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := seatbeltProfile(seatbeltSpec{UserDataDir: t.TempDir()}); err == nil {
		t.Fatal("a profile without a temp directory was accepted; the browser would fail to start with no explanation")
	}
	if _, err := seatbeltProfile(seatbeltSpec{TempDir: t.TempDir()}); err == nil {
		t.Fatal("a profile without a user data directory was accepted")
	}
}

// TestEveryPathInTheProfileIsAlreadyResolved 补上一条**不依赖软链**的守卫。
//
// 上一条（TestTheProfileUsesResolvedPaths）在 Windows 上跳过，于是「不解析路径」这个
// 变异在我实际跑测试的机器上是绿的——一条只在别处生效的测试，等于把验证推迟到了 CI。
//
// 这条换个问法：profile 里写的每个路径，自己再解析一次必须还是它自己。macOS 上
// /var→/private/var 会让未解析的路径露馅；Windows 上 8.3 短名（ADMINI~1）同样露馅。
func TestEveryPathInTheProfileIsAlreadyResolved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	profile, err := seatbeltProfile(seatbeltSpec{
		UserDataDir: filepath.Join(dir, "userdata"),
		TempDir:     os.TempDir(),
	})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}

	var checked int
	for _, line := range strings.Split(profile, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "(subpath ") {
			continue
		}
		var path string
		if _, err := fmt.Sscanf(line, "(subpath %q)", &path); err != nil {
			t.Fatalf("cannot read the path out of %q: %v", line, err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue // 还不存在的路径没得解析，交给 sandbox-exec 自己说
		}
		checked++
		if resolved != path {
			t.Errorf("the profile carries %q but the kernel will see %q;\n"+
				"SBPL matches on the resolved path, so this rule silently matches nothing",
				path, resolved)
		}
	}
	if checked == 0 {
		t.Fatal("no path in the profile could be checked; this test proves nothing as written")
	}
}
