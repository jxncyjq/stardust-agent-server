// Package sessionstate owns the on-disk home of per-session state: the single
// resolver that decides where a session's directory lives, and the checkpoint
// store that persists a suspended task's tool-loop state under it.
package sessionstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultRootName is the directory (under the user home) used when no valid
// workspace.root is configured. It reuses this repo's ".stardust" convention.
const defaultRootName = ".stardust"

// ResolveWorkspaceRoot turns the configured workspace.root into a concrete
// absolute directory. It expands a leading "~" to the user home. A configured,
// existing directory is used as-is. A configured-but-invalid path (non-empty but
// missing or not a directory) falls back to <home>/.stardust and returns a
// non-empty warning describing the fallback so the caller can log it fail-loud
// rather than silently swallowing a typo'd path. An empty configuration is the
// legitimate default (no warning).
func ResolveWorkspaceRoot(configured string) (root string, warning string) {
	home, err := os.UserHomeDir()
	if err != nil {
		// UserHomeDir failing is an unrecoverable environment fault; surface it
		// through the warning channel. Resolve defaultRootName to an absolute
		// path via the process cwd so the "concrete absolute directory" promise
		// in the doc comment above still holds even on this failure branch.
		abs, absErr := filepath.Abs(defaultRootName)
		if absErr != nil {
			// Getwd itself failed — an unrecoverable environment fault. Return the
			// bare name and shout about both failures; never pretend this is fine.
			return defaultRootName, fmt.Sprintf("cannot resolve user home dir: %v; and cannot absolutize %q: %v", err, defaultRootName, absErr)
		}
		return abs, fmt.Sprintf("cannot resolve user home dir: %v; falling back to %q", err, abs)
	}
	fallback := filepath.Join(home, defaultRootName)

	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return fallback, ""
	}
	expanded := expandTilde(trimmed, home)
	info, statErr := os.Stat(expanded)
	if statErr == nil && info.IsDir() {
		return expanded, ""
	}
	// Use %s inside literal quotes (not %q) for the paths: %q would re-escape
	// backslashes in Windows paths (each "\" becomes "\\" in the output),
	// corrupting the path text embedded in the warning.
	detail := "not a directory"
	if statErr != nil {
		detail = statErr.Error()
	}
	return fallback, fmt.Sprintf("configured workspace.root \"%s\" unusable (%s), falling back to \"%s\"", expanded, detail, fallback)
}

// expandTilde replaces a leading "~" (optionally "~/") with the user home dir.
// A "~" that is not the first path segment is left untouched.
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}

// SessionDir returns the directory that holds one session's persisted state:
// <base>/session/<sessionKey>. base is the workspace root (M1b) or, once
// working_dir lands (M3), <working_dir>/.stardust. sessionKey isolates state per
// session so concurrent tasks never write the same file.
func SessionDir(base, sessionKey string) string {
	return filepath.Join(base, "session", sessionKey)
}

// SessionBase returns the base directory under which a session's state
// (checkpoints, approval tickets, plans) lives. When workingDir is non-empty the
// base is <workingDir>/.stardust (design §4.0: a session bound to a working dir
// keeps its state alongside that dir); otherwise it is the workspace root. This
// is the single resolver for the base — callers must never hand-join it
// elsewhere. SessionDir(SessionBase(root, wd), key) yields the full session dir.
func SessionBase(workspaceRoot, workingDir string) string {
	if strings.TrimSpace(workingDir) == "" {
		return workspaceRoot
	}
	return filepath.Join(workingDir, defaultRootName)
}
