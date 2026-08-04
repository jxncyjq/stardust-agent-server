//go:build chromium

package browser

import (
	"testing"
)

func TestManagerAcquireReleaseContext(t *testing.T) {
	m, err := NewManager(ManagerConfig{Headless: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	c1, err := m.AcquireContext(ContextOpts{})
	if err != nil {
		t.Fatalf("AcquireContext: %v", err)
	}
	c2, err := m.AcquireContext(ContextOpts{})
	if err != nil {
		t.Fatalf("AcquireContext 2: %v", err)
	}
	if c1.id == c2.id {
		t.Fatalf("two contexts share id %q — not isolated", c1.id)
	}
	if err := m.ReleaseContext(c1); err != nil {
		t.Fatalf("ReleaseContext: %v", err)
	}
}
