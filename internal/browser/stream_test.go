package browser

import (
	"testing"
)

func TestHubBroadcastToMultipleSubscribers(t *testing.T) {
	h := NewHub(8)
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Publish(StreamEvent{Type: EventProgress, Data: map[string]any{"action": "click"}})

	for i, ch := range []<-chan StreamEvent{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Type != EventProgress || ev.Seq != 1 {
				t.Fatalf("sub %d got %+v, want progress seq=1", i, ev)
			}
		default:
			t.Fatalf("sub %d received nothing", i)
		}
	}
}

func TestHubSeqMonotonicAcrossTypes(t *testing.T) {
	h := NewHub(8)
	ch, cancel := h.Subscribe()
	defer cancel()
	h.Publish(StreamEvent{Type: EventObservation})
	h.Publish(StreamEvent{Type: EventFrame})
	h.Publish(StreamEvent{Type: EventProgress})
	seqs := []uint64{(<-ch).Seq, (<-ch).Seq, (<-ch).Seq}
	if seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("seqs = %v, want 1,2,3", seqs)
	}
}

// 只缓 status（observation/progress），frame 不缓；ReplaySince 补发 seq>lastID 的 status。
func TestHubReplaySinceReturnsMissedStatusNotFrames(t *testing.T) {
	h := NewHub(8)
	h.Publish(StreamEvent{Type: EventObservation}) // seq1
	h.Publish(StreamEvent{Type: EventFrame})       // seq2 (不缓)
	h.Publish(StreamEvent{Type: EventProgress})    // seq3
	replay := h.ReplaySince(1)                     // 要 seq>1 的 status
	if len(replay) != 1 || replay[0].Type != EventProgress || replay[0].Seq != 3 {
		t.Fatalf("replay = %+v, want just progress seq=3", replay)
	}
}

func TestHubRingBufferEvictsOldStatus(t *testing.T) {
	h := NewHub(2) // 只留最近 2 条 status
	for i := 0; i < 5; i++ {
		h.Publish(StreamEvent{Type: EventProgress})
	}
	replay := h.ReplaySince(0) // 要全部（>0）
	if len(replay) != 2 {
		t.Fatalf("buffered %d, want 2 (ring cap)", len(replay))
	}
	if replay[0].Seq != 4 || replay[1].Seq != 5 {
		t.Fatalf("kept seqs %d,%d, want 4,5", replay[0].Seq, replay[1].Seq)
	}
}

func TestHubSubscriberCountAndStopSignal(t *testing.T) {
	h := NewHub(8)
	if h.SubscriberCount() != 0 {
		t.Fatal("want 0 subscribers initially")
	}
	_, cancel := h.Subscribe()
	if h.SubscriberCount() != 1 {
		t.Fatal("want 1 after subscribe")
	}
	cancel()
	if h.SubscriberCount() != 0 {
		t.Fatal("want 0 after cancel")
	}
}
