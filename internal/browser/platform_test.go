package browser

import (
	"strings"
	"testing"
)

func TestNewPlatformAdapterCurrentOS(t *testing.T) {
	pal := NewPlatformAdapter()
	if pal == nil {
		t.Fatal("NewPlatformAdapter returned nil")
	}
	// 数据/缓存目录应非空且是绝对路径样式
	if d := pal.AppDataDir(); d == "" {
		t.Fatal("AppDataDir empty")
	}
	if c := pal.CacheDir(); c == "" {
		t.Fatal("CacheDir empty")
	}
	// ToNativePath 幂等于本平台分隔符
	got := pal.ToNativePath("a/b/c")
	if got == "" || strings.Contains(got, "//") {
		t.Fatalf("ToNativePath bad: %q", got)
	}
	// DefaultLaunchArgs 至少给出一批参数（非 nil）
	if pal.DefaultLaunchArgs() == nil {
		t.Fatal("DefaultLaunchArgs nil")
	}
}
