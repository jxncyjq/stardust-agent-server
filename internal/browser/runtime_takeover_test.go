package browser

import (
	"context"
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

func TestClickBlockedUnderTakeover(t *testing.T) {
	r, sess := newTakeoverRuntime(t)
	if err := r.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	_, err := r.Click(context.Background(), ClickReq{SessionID: sess.ID, Ref: "e1"})
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeTakeover {
		t.Fatalf("want SESSION_UNDER_TAKEOVER, got %v", err)
	}
}

func TestTypeBlockedUnderTakeover(t *testing.T) {
	r, sess := newTakeoverRuntime(t)
	_ = r.SetTakeover(sess.ID, true)
	_, err := r.Type(context.Background(), TypeReq{SessionID: sess.ID, Ref: "e1", Text: "x"})
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeTakeover {
		t.Fatalf("want SESSION_UNDER_TAKEOVER, got %v", err)
	}
}

func TestOpenBlockedUnderTakeover(t *testing.T) {
	r, sess := newTakeoverRuntime(t)
	_ = r.SetTakeover(sess.ID, true)
	// Open 会先 checkURL；用一个能过白名单解析的公网 http url，门控须在导航前触发。
	_, err := r.Open(context.Background(), OpenReq{SessionID: sess.ID, URL: "http://example.com"})
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeTakeover {
		t.Fatalf("want SESSION_UNDER_TAKEOVER, got %v", err)
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
