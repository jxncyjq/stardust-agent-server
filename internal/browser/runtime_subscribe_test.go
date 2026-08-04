package browser

import (
	"testing"
)

// 不启动 Chromium：直接建 Runtime 的会话 Hub，验证 Subscribe 拿到发布的事件。
// hubFor 是内部方法，按会话惰性建 Hub；emitProgress 往会话 Hub 发 progress。
func TestRuntimeSubscribeReceivesEmittedProgress(t *testing.T) {
	r := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry()}
	sess := r.sessions.Create("t1")

	ch, cancel, err := r.Subscribe(sess.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	r.emitProgress(sess.ID, "click", "done", "e1")

	select {
	case ev := <-ch:
		if ev.Type != EventProgress {
			t.Fatalf("got %v, want progress", ev.Type)
		}
	default:
		t.Fatal("no event received")
	}
}

func TestRuntimeSubscribeUnknownSession(t *testing.T) {
	r := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry()}
	if _, _, err := r.Subscribe("nope"); err == nil {
		t.Fatal("expected error for unknown session")
	}
}
