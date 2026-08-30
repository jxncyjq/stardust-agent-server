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
	// SESSION_NOT_FOUND，不是 CONTEXT_EVICTED：后者的意思是「你曾经有的那个会话，
	// 它的上下文被回收了」，对一个从未存在过的 id 说这句话是在编造一段历史。
	var be *BrowserError
	if !asBrowserError(err, &be) || be.Code != CodeSessionNotFound {
		t.Fatalf("want SESSION_NOT_FOUND, got %v", err)
	}
}

// TestCloseClearsTakeover：Close 的 per-session 分支须在删表前把 takeover 标志清零，
// 防止标志悬挂。会话无 Context（无 Chromium）时 ReleaseContext 不应被调用（否则 nil
// 解引用），Close 仍须成功把会话从表里删掉。
// 直接在活跃指针上验证标志被清，而非仅通过会话删除来间接验证。
func TestCloseClearsTakeover(t *testing.T) {
	r, sess := newTakeoverRuntime(t)
	// 前置条件：设置 takeover=true 并验证
	if err := r.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover(true): %v", err)
	}
	if !r.takeoverOf(sess) {
		t.Fatal("precondition: takeover flag should be true before Close")
	}
	// 执行 Close：per-session 分支，nil Context 下可能返回 error，这里只关心标志被清。
	_ = r.Close(context.Background(), CloseReq{SessionID: sess.ID})
	// 主要检查：直接验证活跃指针上的标志已清（即使会话已从表删除，指针仍有效）。
	if r.takeoverOf(sess) {
		t.Fatal("takeover flag should be cleared on the live session pointer after Close")
	}
	// 次要检查：会话已从表删除；再置接管应因未知会话报错。
	if err := r.SetTakeover(sess.ID, true); err == nil {
		t.Fatal("closed session should be removed from store, SetTakeover must error")
	}
}
