package browser

import (
	"sync"
	"testing"
)

// TestConcurrentSubscribeNoRace 并发压 Subscribe+cancel，验证 lifecycle 锁把 screencast
// start/stop 决策串行化后无数据竞争、无 panic，且所有取消回调跑完后订阅者数归零。
// 会话无活跃 page（不需要真 Chromium）：startScreencastLocked 在 pageOf==nil 处提前返回，
// 但计数检查 + 锁 + pageOf 的 sess 锁读取仍被完整并发执行，足以在 -race 下暴露竞态。
func TestConcurrentSubscribeNoRace(t *testing.T) {
	r := &Runtime{
		sessions:      NewSessionStore(),
		hubs:          newHubRegistry(),
		screencasters: &sync.Map{}, // 非 nil，走到 startScreencastLocked 的计数/pageOf 路径
	}
	sess := r.sessions.Create("race")

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, cancel, err := r.Subscribe(sess.ID)
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				return
			}
			cancel()
		}()
	}
	wg.Wait()

	if got := r.hubs.get(sess.ID).SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after all cancels = %d, want 0", got)
	}
}

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
