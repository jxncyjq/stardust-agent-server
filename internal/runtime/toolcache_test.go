package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestRenderToolResultCachesAndFooter(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	out, locator := renderToolResultContent("fetch_url", big, 4000, root, defaultToolResultCacheDir, slog.Default())

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
	// spec §4.1 spill_locator: the returned locator IS the cached file's
	// tool-root-relative path -- the same one the footer hands the model, so an
	// event and the model can never be pointed at different files.
	if locator != rel {
		t.Fatalf("spill locator = %q, want the footer's cache path %q", locator, rel)
	}
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
	out, locator := renderToolResultContent("read_file", big, 4000, root, defaultToolResultCacheDir, slog.Default())
	if strings.Contains(out, ".stardust/tool_results") {
		t.Fatalf("read_file result must not be cached, got %q", out)
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("read_file oversize should still get plain truncation footer")
	}
	// Nothing was written, so there is no full text to point at.
	if locator != "" {
		t.Fatalf("spill locator = %q, want empty: an uncached read_file result has no full-text file", locator)
	}
}

func TestRenderToolResultEmptyRootPlainTruncation(t *testing.T) {
	big := strings.Repeat("Z", 20000)
	out, locator := renderToolResultContent("fetch_url", big, 4000, "", defaultToolResultCacheDir, slog.Default())
	if strings.Contains(out, "tool_results") {
		t.Fatalf("empty toolRoot must not cache, got %q", out[:min2(200, len(out))])
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("empty toolRoot oversize should get plain truncation")
	}
	if locator != "" {
		t.Fatalf("spill locator = %q, want empty: without a sandbox nothing was written", locator)
	}
}

func TestRenderToolResultUnderBudgetUnchanged(t *testing.T) {
	small := "short"
	out, locator := renderToolResultContent("fetch_url", small, 4000, t.TempDir(), defaultToolResultCacheDir, slog.Default())
	if out != small {
		t.Fatalf("under-budget content must be returned verbatim, got %q", out)
	}
	// The contract's declared optional: nothing was truncated, so there is no
	// full-text file -- an empty locator here is correct, not a fallback.
	if locator != "" {
		t.Fatalf("spill locator = %q, want empty for an un-truncated result", locator)
	}
}

func TestSessionCacheDir(t *testing.T) {
	cases := []struct {
		name string
		task domain.Task
		want string // relative, forward-slashed
	}{
		{"session id used", domain.Task{ID: "gui-task-1", SessionID: "session-abc"}, defaultToolResultCacheDir + "/session-abc"},
		{"fallback to task id", domain.Task{ID: "gui-task-1", SessionID: ""}, defaultToolResultCacheDir + "/gui-task-1"},
		{"both empty -> no-session", domain.Task{ID: "", SessionID: ""}, defaultToolResultCacheDir + "/no-session"},
		{"illegal chars sanitized", domain.Task{ID: "x", SessionID: "a/b c:d"}, defaultToolResultCacheDir + "/a-b-c-d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filepath.ToSlash(sessionCacheDir(tc.task))
			if got != tc.want {
				t.Fatalf("sessionCacheDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeCacheSegmentAllIllegal(t *testing.T) {
	if got := sanitizeCacheSegment("///"); got != "session" {
		t.Fatalf("all-illegal segment = %q, want session", got)
	}
}

func TestSessionIsolationDifferentDirsSameContent(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	dirA := sessionCacheDir(domain.Task{ID: "t1", SessionID: "sess-A"})
	dirB := sessionCacheDir(domain.Task{ID: "t2", SessionID: "sess-B"})

	relA, err := writeToolResultCache(root, dirA, "fetch_url", big)
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	relB, err := writeToolResultCache(root, dirB, "fetch_url", big)
	if err != nil {
		t.Fatalf("write B: %v", err)
	}
	if relA == relB {
		t.Fatalf("same content in different sessions must not share a path: %q", relA)
	}
	if !strings.Contains(relA, "sess-A") || !strings.Contains(relB, "sess-B") {
		t.Fatalf("paths not session-isolated: A=%q B=%q", relA, relB)
	}
	for _, rel := range []string{relA, relB} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len([]rune(string(data))) != 20000 {
			t.Fatalf("%s len = %d, want 20000", rel, len([]rune(string(data))))
		}
	}
}

func TestSessionScopedRenderReadBack(t *testing.T) {
	root := t.TempDir()
	dir := sessionCacheDir(domain.Task{ID: "t1", SessionID: "sess-X"})
	big := strings.Repeat("Y", 20000)
	out, locator := renderToolResultContent("fetch_url", big, 4000, root, dir, slog.Default())

	if !strings.Contains(out, "tool_results/sess-X/") {
		t.Fatalf("footer path not session-scoped: %q", out)
	}
	rel := footerCachePath(t, out)
	if locator != rel {
		t.Fatalf("spill locator = %q, want the footer's session-scoped cache path %q", locator, rel)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read back session-scoped cache: %v", err)
	}
	if len([]rune(string(data))) != 20000 {
		t.Fatalf("read-back len = %d, want 20000", len([]rune(string(data))))
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

// TestWriteToolResultCacheAvoidsDoubleStardust guards the fix for a user who
// pointed a session's working_dir at the .stardust folder itself: without the
// guard, joining a .stardust toolRoot with the ".stardust/tool_results" cacheDir
// produced <root>/.stardust/.stardust/tool_results. The cache must land directly
// under the .stardust root (no doubled segment).
func TestWriteToolResultCacheAvoidsDoubleStardust(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".stardust")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := writeToolResultCache(root, sessionCacheDir(domain.Task{SessionID: "s1"}), "fetch_url", strings.Repeat("X", 100))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	slash := filepath.ToSlash(rel)
	if strings.Contains(slash, ".stardust/") {
		t.Fatalf("rel path must not re-add .stardust under a .stardust root, got %q", rel)
	}
	if !strings.HasPrefix(slash, "tool_results/") {
		t.Fatalf("expected tool_results/... under the .stardust root, got %q", rel)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("cache file missing at resolved path: %v", err)
	}
}

// TestWriteToolResultCacheNormalRootKeepsStardust confirms the guard does NOT
// change the normal case: a non-.stardust toolRoot still nests under .stardust.
func TestWriteToolResultCacheNormalRootKeepsStardust(t *testing.T) {
	root := t.TempDir()
	rel, err := writeToolResultCache(root, sessionCacheDir(domain.Task{SessionID: "s1"}), "fetch_url", strings.Repeat("X", 100))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.HasPrefix(filepath.ToSlash(rel), ".stardust/tool_results/") {
		t.Fatalf("normal root should nest under .stardust/tool_results, got %q", rel)
	}
}

// 第五条返回路径：折叠被关掉（maxResultChars <= 0）时内容原样返回，也就没有全文文件。
func TestRenderToolResultFoldingDisabledHasNoLocator(t *testing.T) {
	big := strings.Repeat("X", 20000)
	out, locator := renderToolResultContent("fetch_url", big, 0, t.TempDir(), defaultToolResultCacheDir, slog.Default())
	if out != big {
		t.Fatalf("maxResultChars<=0 must return the content verbatim (len %d, want %d)", len(out), len(big))
	}
	if locator != "" {
		t.Fatalf("spill locator = %q, want empty: nothing was truncated so nothing was spilled", locator)
	}
}

// 写缓存失败那条路径：定位符必须是空串。它已经在 fail-loud 地记日志了，而全文
// **确实不存在**——这时报一个路径出去，轨迹会去取一个不在那儿的文件。
func TestRenderToolResultCacheWriteFailureHasNoLocator(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("X", 20000)
	// 一个逃出沙箱的 cacheDir：guard.Check 在任何 mkdir 之前就拒掉它。
	out, locator := renderToolResultContent("fetch_url", big, 4000, root,
		filepath.Join("..", "escaped"), slog.Default())
	if locator != "" {
		t.Fatalf("spill locator = %q, want empty: the cache write failed so there is no full-text file", locator)
	}
	if !strings.Contains(out, "硬截断") {
		t.Fatalf("cache write failure should still degrade to plain truncation, got %q", out[:min2(200, len(out))])
	}
	if strings.Contains(out, "escaped") {
		t.Fatalf("a failed cache write must not advertise a path, got %q", out)
	}
}
