package browser

import (
	"errors"
	"testing"
)

func asBrowserError(err error, target **BrowserError) bool { return errors.As(err, target) }

// newTestSession 直接建一个带会话的 store-only Runtime，不起 Chromium。
func newTakeoverRuntime(t *testing.T) (*Runtime, *Session) {
	t.Helper()
	r := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry()}
	sess := r.sessions.Create("task-1")
	return r, sess
}

func TestSetTakeoverTogglesFlag(t *testing.T) {
	r, sess := newTakeoverRuntime(t)
	if r.takeoverOf(sess) {
		t.Fatal("new session should not be under takeover")
	}
	if err := r.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover(true): %v", err)
	}
	if !r.takeoverOf(sess) {
		t.Fatal("takeover flag should be set")
	}
	if err := r.SetTakeover(sess.ID, false); err != nil {
		t.Fatalf("SetTakeover(false): %v", err)
	}
	if r.takeoverOf(sess) {
		t.Fatal("takeover flag should be cleared")
	}
}

func TestSetTakeoverUnknownSession(t *testing.T) {
	r, _ := newTakeoverRuntime(t)
	err := r.SetTakeover("sess-does-not-exist", true)
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeContextEvicted {
		t.Fatalf("want CONTEXT_EVICTED, got %v", err)
	}
}
