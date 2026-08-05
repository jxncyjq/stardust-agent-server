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
