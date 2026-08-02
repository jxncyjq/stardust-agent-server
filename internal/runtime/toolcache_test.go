package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"
)

func TestRenderToolResultCachesAndFooter(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	out := renderToolResultContent("fetch_url", big, 4000, root, defaultToolResultCacheDir, slog.Default())

	if !strings.Contains(out, "硬截断") {
		t.Fatalf("missing hard-truncation footer: %q", out[:min2(300, len(out))])
	}
	if !strings.Contains(out, "read_file") || !strings.Contains(out, ".stardust/tool_results") {
		t.Fatalf("footer missing read_file/cache path: %q", out)
	}
	if !strings.HasPrefix(out, strings.Repeat("X", 4000)) {
		t.Fatalf("preview head missing")
	}
	rel := footerCachePath(t, out)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("cache file unreadable: %v", err)
	}
	if len([]rune(string(data))) != 20000 {
		t.Fatalf("cache file should hold full 20000 runes, got %d", len([]rune(string(data))))
	}
}

func TestRenderToolResultReadFileExemptFromCache(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("Y", 20000)
	out := renderToolResultContent("read_file", big, 4000, root, defaultToolResultCacheDir, slog.Default())
	if strings.Contains(out, ".stardust/tool_results") {
		t.Fatalf("read_file result must not be cached, got %q", out)
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("read_file oversize should still get plain truncation footer")
	}
}

func TestRenderToolResultEmptyRootPlainTruncation(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	out := renderToolResultContent("fetch_url", big, 4000, "", defaultToolResultCacheDir, slog.Default())
	if strings.Contains(out, "tool_results") {
		t.Fatalf("empty toolRoot must not cache, got %q", out[:min2(200, len(out))])
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("empty toolRoot oversize should get plain truncation")
	}
}

func TestRenderToolResultUnderBudgetUnchanged(t *testing.T) {
	small := "short"
	out := renderToolResultContent("fetch_url", small, 4000, t.TempDir(), defaultToolResultCacheDir, slog.Default())
	if out != small {
		t.Fatalf("under-budget content must be returned verbatim, got %q", out)
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func footerCachePath(t *testing.T, out string) string {
	t.Helper()
	const marker = `read_file path="`
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no read_file path marker in %q", out)
	}
	rest := out[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated read_file path in %q", rest)
	}
	return rest[:j]
}
