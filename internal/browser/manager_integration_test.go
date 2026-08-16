//go:build chromium

package browser

import (
	"testing"

	"github.com/go-rod/rod/lib/proto"
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

// TestReleaseContextIsolatesOtherContextsPages guards against the cross-session
// page-close bug: go-rod's Browser.Pages() calls an unfiltered Target.getTargets,
// so it returns pages from ALL incognito contexts. ReleaseContext must therefore
// NOT enumerate + close pages that way — doing so closed other live sessions'
// pages, canceling their page context so takeover injection / reads failed with
// "context canceled". Releasing one context must leave another context's page
// fully usable.
func TestReleaseContextIsolatesOtherContextsPages(t *testing.T) {
	m, err := NewManager(ManagerConfig{Headless: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	c1, err := m.AcquireContext(ContextOpts{})
	if err != nil {
		t.Fatalf("AcquireContext c1: %v", err)
	}
	c2, err := m.AcquireContext(ContextOpts{})
	if err != nil {
		t.Fatalf("AcquireContext c2: %v", err)
	}

	if _, err := c1.browser.Page(proto.TargetCreateTarget{URL: "about:blank"}); err != nil {
		t.Fatalf("c1 page: %v", err)
	}
	p2, err := c2.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("c2 page: %v", err)
	}

	// Releasing c1 must not touch c2's page.
	if err := m.ReleaseContext(c1); err != nil {
		t.Fatalf("ReleaseContext c1: %v", err)
	}

	// c2's page must still be alive — before the fix its context was canceled.
	res, err := p2.Eval("() => 1 + 1")
	if err != nil {
		t.Fatalf("c2 page unusable after releasing c1 (cross-context close bug): %v", err)
	}
	if res.Value.Int() != 2 {
		t.Fatalf("c2 page eval = %d, want 2", res.Value.Int())
	}
}
