//go:build windows

package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsAppDataDir(t *testing.T) {
	pal := newPlatformAdapter()
	d := pal.AppDataDir()
	if d == "" {
		t.Fatal("AppDataDir empty")
	}
	// 应落在 %LOCALAPPDATA% 下（若环境有该变量）
	if la := os.Getenv("LOCALAPPDATA"); la != "" && !strings.HasPrefix(d, la) {
		t.Fatalf("AppDataDir %q not under LOCALAPPDATA %q", d, la)
	}
}

func TestWindowsToNativePath(t *testing.T) {
	pal := newPlatformAdapter()
	got := pal.ToNativePath("a/b/c")
	if got != `a\b\c` {
		t.Fatalf("ToNativePath = %q, want a\\b\\c", got)
	}
}

func TestWindowsSafeDeleteMissingFileOK(t *testing.T) {
	pal := newPlatformAdapter()
	// 删不存在的文件应不报错（幂等）
	if err := pal.SafeDelete(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("SafeDelete missing: %v", err)
	}
	// 删存在的文件应成功
	f := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(f, []byte("hi"), 0o644)
	if err := pal.SafeDelete(f); err != nil {
		t.Fatalf("SafeDelete existing: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
}
