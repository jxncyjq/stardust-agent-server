package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

const (
	// defaultToolResultCacheDir is where oversized tool results are cached,
	// relative to the run's tool sandbox root, so read_file can page them back.
	defaultToolResultCacheDir = ".stardust/tool_results"
	// toolResultCacheFileMax bounds a single cached file so a giant result cannot
	// write unbounded bytes into the workspace.
	toolResultCacheFileMax = 2_000_000
)

// renderToolResultContent decides how one tool result's text reaches the model.
//
//   - Within maxResultChars: returned verbatim.
//   - Oversized AND cacheable (non-empty toolRoot, tool is not read_file): the
//     full text is cached under toolRoot/cacheDir and the model gets a preview +
//     a self-describing footer naming the cache path and the exact read_file call
//     to page the rest.
//   - Oversized but NOT cacheable (empty toolRoot = no sandbox, or a read_file
//     result whose cache would create a persist→read→persist loop): plain
//     self-describing truncation via truncateText (P0), no disk write.
//
// A cache write failure is never fatal: it is logged (fail-loud 铁律 requires
// recording, not swallowing) and the result falls back to plain truncation so
// the model still gets a usable, self-describing answer. A sandbox-escape
// failure (port.ErrPathOutsideWorkspace, "本不该发生") is logged at Error; other
// write failures (disk full, etc.) at Warn.
//
// The SECOND return value is the spill locator (spec §4.1's spill_locator): the
// TOOL-ROOT-RELATIVE path of the file holding the full text, which is the only
// clue a trajectory viewer has for fetching what the preview cut off. It is the
// empty string on every path that wrote no such file — that is the contract's
// declared optional ("结果没超长就没有全文文件"), NOT a fallback for an error:
//
//   - maxResultChars <= 0 (folding disabled): nothing was truncated, so there is
//     no full-text file to point at.
//   - Within maxResultChars: same — the model already has the whole thing.
//   - Oversized but NOT cacheable (empty toolRoot / read_file): truncateText
//     writes nothing to disk, so no file exists to name.
//   - Cache write FAILED: the failure is already logged loudly above; the full
//     text genuinely does not exist on disk, so naming a path would point the
//     trajectory at a file that is not there. Empty is the honest answer.
//   - Cache write succeeded: relPath, the path the footer also hands the model.
//
// Before this, relPath only ever reached the model's footer text; no caller
// could see it, so the event log had no way to carry it and a truncated tool
// result in the trajectory had no continuation.
func renderToolResultContent(toolName, content string, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) (string, string) {
	if maxResultChars <= 0 {
		return content, ""
	}
	runes := []rune(content)
	total := len(runes)
	if total <= maxResultChars {
		return content, ""
	}
	if strings.TrimSpace(toolRoot) == "" || toolName == "read_file" {
		return truncateText(content, maxResultChars), ""
	}
	relPath, err := writeToolResultCache(toolRoot, cacheDir, toolName, content)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		escaped := errors.Is(err, port.ErrPathOutsideWorkspace)
		msg := "tool result cache write failed; falling back to plain truncation"
		attrs := []any{"tool", toolName, "cache_dir", cacheDir, "error", err, "sandbox_escape", escaped}
		if escaped {
			logger.Error(msg, attrs...)
		} else {
			logger.Warn(msg, attrs...)
		}
		return truncateText(content, maxResultChars), ""
	}
	return string(runes[:maxResultChars]) + fmt.Sprintf(
		"\n\n──────── [输出被硬截断 / OUTPUT HARD-TRUNCATED] ────────\n"+
			"这是硬截断（上下文预算限制），非数据或参数问题——重试不会有帮助。已保存全文，可用 read_file 翻页续读。\n"+
			"This is a hard truncation; retrying won't help. The result is saved to a file — page through it with read_file.\n"+
			"显示 %d / 共 %d 字符（rune）。\n"+
			"取回剩余：read_file path=%q offset=%d\n",
		maxResultChars, total, relPath, maxResultChars), relPath
}

// writeToolResultCache writes content to toolRoot/cacheDir/<tool>-<hash>.md,
// guarded to stay inside toolRoot, and returns the path RELATIVE to toolRoot
// (forward-slashed, what read_file expects). guard.Check runs BEFORE any mkdir
// so a traversal cacheDir cannot create directories outside the sandbox. Errors
// are returned, never swallowed.
func writeToolResultCache(toolRoot, cacheDir, toolName, content string) (string, error) {
	absRoot, err := filepath.Abs(toolRoot)
	if err != nil {
		return "", fmt.Errorf("resolve tool root %q: %w", toolRoot, err)
	}
	// Guard against a doubled .stardust: cacheDir carries a ".stardust/" prefix
	// (defaultToolResultCacheDir), but if toolRoot is ITSELF a .stardust directory
	// — the user pointed a session's working_dir at the .stardust folder — joining
	// them yields <root>/.stardust/.stardust/tool_results. Drop the redundant
	// prefix so it lands at <root=.stardust>/tool_results/... in that case.
	if filepath.Base(absRoot) == ".stardust" {
		cacheDir = strings.TrimPrefix(filepath.ToSlash(cacheDir), ".stardust/")
	}
	sum := sha256.Sum256([]byte(content))
	name := fmt.Sprintf("%s-%s.md", sanitizeToolName(toolName), hex.EncodeToString(sum[:])[:10])
	dir := filepath.Join(absRoot, cacheDir)
	target := filepath.Join(dir, name)

	guard := port.NewWorkspacePathGuard(absRoot)
	if _, err := guard.Check(context.Background(), target); err != nil {
		return "", fmt.Errorf("cache path escapes workspace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if runes := []rune(content); len(runes) > toolResultCacheFileMax {
		content = string(runes[:toolResultCacheFileMax]) + "\n\n[... stored copy capped ...]"
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write cache file: %w", err)
	}
	rel, err := filepath.Rel(absRoot, target)
	if err != nil {
		return "", fmt.Errorf("relativize cache path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

// sessionCacheDir returns the tool-result cache dir for a task, isolated by
// session: <defaultToolResultCacheDir>/<sanitized session key>. The key is
// task.SessionID, falling back to task.ID, then to "no-session" (a task always
// has an ID, so the final fallback guards the impossible rather than silently
// defaulting). Isolating by session keeps one session's cached tool output out
// of another session's file tools (list_files/read_file/search_content share
// the sandbox root) and makes per-session cleanup possible.
func sessionCacheDir(task domain.Task) string {
	key := strings.TrimSpace(task.SessionID)
	if key == "" {
		key = strings.TrimSpace(task.ID)
	}
	if key == "" {
		key = "no-session"
	}
	return filepath.Join(defaultToolResultCacheDir, sanitizeCacheSegment(key))
}

// sanitizeCacheSegment keeps only filename-safe chars from a path segment (a
// session or task id). IDs are already safe (e.g. "session-1785…",
// "gui-task-…"); this is a defensive guard mirroring sanitizeToolName. An
// all-illegal segment degrades to "session" rather than an empty path element.
func sanitizeCacheSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}

// sanitizeToolName keeps only filename-safe chars from a tool name for the cache
// file. Tool names are already [a-z_], so this is a defensive guard.
func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tool"
	}
	return out
}
