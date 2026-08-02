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
func renderToolResultContent(toolName, content string, maxResultChars int, toolRoot, cacheDir string, logger *slog.Logger) string {
	if maxResultChars <= 0 {
		return content
	}
	runes := []rune(content)
	total := len(runes)
	if total <= maxResultChars {
		return content
	}
	if strings.TrimSpace(toolRoot) == "" || toolName == "read_file" {
		return truncateText(content, maxResultChars)
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
		return truncateText(content, maxResultChars)
	}
	return string(runes[:maxResultChars]) + fmt.Sprintf(
		"\n\n──────── [输出被硬截断 / OUTPUT HARD-TRUNCATED] ────────\n"+
			"这是硬截断（上下文预算限制），非数据或参数问题——重试不会有帮助。已保存全文，可用 read_file 翻页续读。\n"+
			"This is a hard truncation; retrying won't help. The result is saved to a file — page through it with read_file.\n"+
			"显示 %d / 共 %d 字符（rune）。\n"+
			"取回剩余：read_file path=%q offset=%d\n",
		maxResultChars, total, relPath, maxResultChars)
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
