package gateway

import (
	"sync"
	"testing"
	"time"
)

func TestDeliveryTrackerTrackTakeOnce(t *testing.T) {
	tr := NewDeliveryTracker()
	tr.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})
	got, ok := tr.Take("task-1")
	if !ok || got.ChatID != "42" {
		t.Fatalf("Take(task-1) = %v, %v, want telegram/42", got, ok)
	}
	// Consumed: a second Take misses.
	if _, ok := tr.Take("task-1"); ok {
		t.Fatalf("Take(task-1) second = ok, want miss after consume")
	}
	// Unknown task misses (legitimate: not a gateway-submitted task).
	if _, ok := tr.Take("other"); ok {
		t.Fatalf("Take(other) = ok, want miss")
	}
}

func TestDeliveryTrackerPendingSnapshot(t *testing.T) {
	tr := NewDeliveryTracker()
	tr.Track("a", DeliveryTarget{Platform: "telegram", ChatID: "1"})
	tr.Track("b", DeliveryTarget{Platform: "telegram", ChatID: "2"})
	pending := tr.Pending()
	if len(pending) != 2 {
		t.Fatalf("Pending() = %v, want 2 ids", pending)
	}
	tr.Take("a")
	if got := tr.Pending(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Pending() after take = %v, want [b]", got)
	}
}

func TestDeliveryTrackerGetAndMarkAttempt(t *testing.T) {
	tr := NewDeliveryTracker()
	tr.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})

	target, attempts, nextAt, ok := tr.Get("task-1")
	if !ok || target.ChatID != "42" || attempts != 0 || !nextAt.IsZero() {
		t.Fatalf("Get(task-1) = %v, %d, %v, %v, want telegram/42, 0, zero, true", target, attempts, nextAt, ok)
	}

	retryAt := time.Now().Add(500 * time.Millisecond)
	tr.MarkAttempt("task-1", retryAt)

	target, attempts, nextAt, ok = tr.Get("task-1")
	if !ok || target.ChatID != "42" || attempts != 1 || !nextAt.Equal(retryAt) {
		t.Fatalf("Get(task-1) after MarkAttempt = %v, %d, %v, %v, want telegram/42, 1, %v, true", target, attempts, nextAt, ok, retryAt)
	}

	// Unknown task id: Get misses, MarkAttempt is a no-op (does not panic or create an entry).
	if _, _, _, ok := tr.Get("other"); ok {
		t.Fatalf("Get(other) = ok, want miss")
	}
	tr.MarkAttempt("other", retryAt)
	if _, _, _, ok := tr.Get("other"); ok {
		t.Fatalf("MarkAttempt(other) created an entry, want no-op")
	}
}

func TestDeliveryTrackerConcurrent(t *testing.T) {
	tr := NewDeliveryTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "t" + string(rune('a'+i%26))
			tr.Track(id, DeliveryTarget{Platform: "telegram", ChatID: id})
			tr.Take(id)
		}(i)
	}
	wg.Wait()
}
