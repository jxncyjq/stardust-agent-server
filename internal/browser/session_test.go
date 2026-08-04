package browser

import (
	"sync"
	"testing"
)

func TestSessionStoreCreateGet(t *testing.T) {
	s := NewSessionStore()
	sess := s.Create("task-1")
	if sess.ID == "" {
		t.Fatal("Create returned empty ID")
	}
	got, ok := s.Get(sess.ID)
	if !ok || got.ID != sess.ID {
		t.Fatalf("Get(%q) miss", sess.ID)
	}
	s.Delete(sess.ID)
	if _, ok := s.Get(sess.ID); ok {
		t.Fatalf("expected deleted")
	}
}

// TestSessionSerialLock 验证 WithLock 对同一会话串行——并发累加无丢失即证明互斥。
func TestSessionSerialLock(t *testing.T) {
	s := NewSessionStore()
	sess := s.Create("task-1")
	var counter int
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.WithLock(func() { counter++ })
		}()
	}
	wg.Wait()
	if counter != 100 {
		t.Fatalf("counter = %d, want 100 (lock not serializing)", counter)
	}
}
