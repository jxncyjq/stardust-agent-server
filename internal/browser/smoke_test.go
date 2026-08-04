package browser

import (
	"testing"

	"github.com/go-rod/rod/lib/launcher"
)

// TestGoRodImportable 只验证 go-rod 可编译链接，不启动浏览器。
func TestGoRodImportable(t *testing.T) {
	l := launcher.New()
	if l == nil {
		t.Fatal("launcher.New() returned nil")
	}
}
