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
	out := renderToolResultContent("fetch_url", big, 4000, root, dir, slog.Default())

	if !strings.Contains(out, "tool_results/sess-X/") {
		t.Fatalf("footer path not session-scoped: %q", out)
	}
	rel := footerCachePath(t, out)
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
