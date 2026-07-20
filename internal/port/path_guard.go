package port

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrPathOutsideWorkspace = errors.New("path outside workspace")

type WorkspacePathGuard struct {
	root string
}

func NewWorkspacePathGuard(root string) WorkspacePathGuard {
	return WorkspacePathGuard{root: filepath.Clean(root)}
}

func (g WorkspacePathGuard) Check(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	clean := filepath.Clean(path)
	if err := g.checkLexical(clean); err != nil {
		return "", err
	}
	// A lexical check alone is not a boundary: it only proves the *spelling* of
	// the path stays inside the root. A symlink inside the workspace pointing
	// out of it spells fine, and the caller's os.Open then follows it straight
	// out. Compare real paths as well.
	if err := g.checkResolved(clean); err != nil {
		return "", err
	}
	// Return the lexical path, not the resolved one: callers use it for the
	// actual I/O, and substituting a resolved path would change the filenames
	// they see (e.g. /var vs /private/var on macOS).
	return clean, nil
}

func (g WorkspacePathGuard) checkLexical(clean string) error {
	rel, err := filepath.Rel(g.root, clean)
	if err != nil {
		return fmt.Errorf("check path relation: %w", err)
	}
	if rel == "." {
		return nil
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s", ErrPathOutsideWorkspace, clean)
	}
	return nil
}

// checkResolved re-runs the containment test on symlink-resolved paths.
//
// The root is resolved too: a workspace may itself live under a link (a linked
// project directory, or /tmp on macOS), and resolving only the target would
// then make every legitimate path look external.
func (g WorkspacePathGuard) checkResolved(clean string) error {
	realRoot, err := resolveExisting(g.root)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	realTarget, err := resolveExisting(clean)
	if err != nil {
		return fmt.Errorf("resolve path %q: %w", clean, err)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return fmt.Errorf("check resolved path relation: %w", err)
	}
	if rel == "." {
		return nil
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s resolves outside the workspace", ErrPathOutsideWorkspace, clean)
	}
	return nil
}

// resolveExisting resolves symlinks in path. When path does not exist yet — the
// normal case for a file about to be written — it resolves the nearest existing
// ancestor and re-appends the remainder.
//
// Skipping the check for non-existent targets would leave the hole open: a
// write under an escaping link would sail through precisely because the file is
// not there yet.
func resolveExisting(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		// The entry exists but could not be resolved (a broken link, or a
		// permission problem). Refusing here is deliberate: an unresolvable
		// path cannot be proven to stay inside the workspace.
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		// Reached the filesystem root without finding anything that exists.
		return path, nil
	}
	resolvedParent, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
