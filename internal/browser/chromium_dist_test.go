package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile 建一个可执行占位文件，供分发优先级测试断言"存在"。
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// resolveChromiumBin 的优先级：config override > 内置捆绑(存在) > PAL 系统探测 > ""(交 go-rod 下载)。
func TestResolveChromiumBinPriority(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundled-chrome")
	writeFile(t, bundled)
	system := filepath.Join(tmp, "system-chrome")
	writeFile(t, system)

	// 1. config override 最高
	got := resolveChromiumBin(ChromiumDist{ConfigBinPath: "/explicit/chrome", BundledPath: bundled, SystemPath: system})
	if got != "/explicit/chrome" {
		t.Fatalf("config override should win, got %q", got)
	}
	// 2. 无 config，内置存在则用内置
	got = resolveChromiumBin(ChromiumDist{BundledPath: bundled, SystemPath: system})
	if got != bundled {
		t.Fatalf("bundled should win over system, got %q", got)
	}
	// 3. 无 config、内置不存在，用系统
	got = resolveChromiumBin(ChromiumDist{BundledPath: filepath.Join(tmp, "missing"), SystemPath: system})
	if got != system {
		t.Fatalf("system fallback expected, got %q", got)
	}
	// 4. 都无 → 空串（go-rod 自动下载）
	got = resolveChromiumBin(ChromiumDist{})
	if got != "" {
		t.Fatalf("empty expected for auto-download, got %q", got)
	}
}

// TestTheBundledTierIsReachableFromTheRuntime 是 V3 抓到的那件事：
// **没有人填过 BundledChromiumPath**。
//
// `resolveChromiumBin` 里那一级优先级（内置捆绑的固定版 Chromium）写得好好的，单元
// 测试也覆盖着它，但从 RuntimeConfig 到 ManagerConfig 的那一跳把这个字段整个漏掉了
// ——于是打包时把 Chromium 放进 App 里也没用，运行时永远看不见它，退到系统探测或
// go-rod 下载。少接一个字段不会让任何东西报错：浏览器照常起来，用的只是另一个
// Chromium。这与 browser 配置键那次是同一个形状（见 cli 那条 14 键断言）。
//
// 这条守的是那一跳。它不需要真的 Chromium：给一个存在的文件当「内置的那个」，问
// 运行时最终解析到哪个可执行文件。
func TestTheBundledTierIsReachableFromTheRuntime(t *testing.T) {
	t.Parallel()

	bundled := filepath.Join(t.TempDir(), "chrome-bundled")
	if err := os.WriteFile(bundled, []byte("not a real browser, but a real file"), 0o755); err != nil {
		t.Fatalf("write the stand-in bundled browser: %v", err)
	}

	got := resolveChromiumBin(chromiumDistFor(RuntimeConfig{BundledChromiumPath: bundled}, ""))
	if got != bundled {
		t.Errorf("resolveChromiumBin = %q, want the bundled browser %q;\n"+
			"the config field never reaches the resolver, so a Chromium shipped inside the app "+
			"is invisible at runtime and the agent silently uses another one", got, bundled)
	}
}

// TestAnExplicitBinPathStillOutranksTheBundledOne：内置那一级不能盖过显式配置——
// 运维指名一个浏览器，就该用那个。
func TestAnExplicitBinPathStillOutranksTheBundledOne(t *testing.T) {
	t.Parallel()

	bundled := filepath.Join(t.TempDir(), "chrome-bundled")
	if err := os.WriteFile(bundled, []byte("x"), 0o755); err != nil {
		t.Fatalf("write the stand-in bundled browser: %v", err)
	}

	got := resolveChromiumBin(chromiumDistFor(
		RuntimeConfig{BinPath: "/opt/chosen-chrome", BundledChromiumPath: bundled}, ""))
	if got != "/opt/chosen-chrome" {
		t.Errorf("resolveChromiumBin = %q, want the explicitly configured /opt/chosen-chrome", got)
	}
}
