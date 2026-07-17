package port

import (
	"context"
	"errors"
	"fmt"
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
	rel, err := filepath.Rel(g.root, clean)
	if err != nil {
		return "", fmt.Errorf("check path relation: %w", err)
	}
	if rel == "." {
		return clean, nil
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %s", ErrPathOutsideWorkspace, clean)
	}
	return clean, nil
}
