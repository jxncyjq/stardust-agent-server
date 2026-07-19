package observability

import (
	"context"
	"fmt"
	"testing"
)

// TestPublishBoundsHistoryToBuffer verifies that Publish never lets the
// retained history grow past the configured buffer size. We assert this
// indirectly: after publishing more events than the buffer holds, a fresh
// subscriber's replay must contain at most `buffer` events.
func TestPublishBoundsHistoryToBuffer(t *testing.T) {
	bus := NewEventBus(4)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := bus.Publish(ctx, EventEnvelope{Type: fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatalf("Publish(%d): unexpected error: %v", i, err)
		}
	}

	ch, cancel := bus.Subscribe(ctx)
	defer cancel()

	got := drainAvailable(ch, 4)
	if len(got) > 4 {
		t.Fatalf("history not bounded: replay delivered %d events, want <= 4: %+v", len(got), got)
	}
}

// TestSubscribeReplaysMostRecentNotOldest is the core regression test for
// I-2: a late subscriber must receive the most recent events (including the
// very latest), not the oldest ones that happened to fit in the channel
// buffer first.
func TestSubscribeReplaysMostRecentNotOldest(t *testing.T) {
	bus := NewEventBus(4)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := bus.Publish(ctx, EventEnvelope{Type: fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatalf("Publish(%d): unexpected error: %v", i, err)
		}
	}

	ch, cancel := bus.Subscribe(ctx)
	defer cancel()

	got := drainAvailable(ch, 4)
	if len(got) != 4 {
		t.Fatalf("expected replay of exactly 4 events, got %d: %+v", len(got), got)
	}

	want := []string{"e6", "e7", "e8", "e9"}
	for i, w := range want {
		if got[i].Type != w {
			t.Fatalf("replay[%d] = %q, want %q (full replay: %+v)", i, got[i].Type, w, got)
		}
	}

	for _, event := range got {
		if event.Type == "e0" {
			t.Fatalf("replay must not contain the oldest event e0, got: %+v", got)
		}
	}
	if got[len(got)-1].Type != "e9" {
		t.Fatalf("replay must contain the most recent event e9 as the last entry, got: %+v", got)
	}
}

// TestPublishDoesNotBlockOnSlowSubscriber preserves existing behavior: a
// subscriber whose channel is already full for *live* (non-replay) delivery
// must not block Publish; the event is silently dropped for that slow
// subscriber only.
func TestPublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	bus := NewEventBus(1)
	ctx := context.Background()

	ch, cancel := bus.Subscribe(ctx)
	defer cancel()

	// Fill the subscriber's channel buffer (capacity 1) without draining it.
	if err := bus.Publish(ctx, EventEnvelope{Type: "first"}); err != nil {
		t.Fatalf("Publish: unexpected error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := bus.Publish(ctx, EventEnvelope{Type: "second"}); err != nil {
			t.Errorf("Publish: unexpected error: %v", err)
		}
	}()
	select {
	case <-done:
	default:
	}
	<-done // Publish must return promptly even though ch is full and undrained.

	// Drain whatever made it through; must not have blocked the goroutine above.
	<-ch
}

// TestCloseClosesSubscriberChannels preserves existing Close semantics.
func TestCloseClosesSubscriberChannels(t *testing.T) {
	bus := NewEventBus(4)
	ctx := context.Background()

	ch, cancel := bus.Subscribe(ctx)
	defer cancel()

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	if _, ok := <-ch; ok {
		t.Fatalf("expected subscriber channel to be closed after bus Close")
	}

	if err := bus.Publish(ctx, EventEnvelope{Type: "after-close"}); err != ErrEventBusClosed {
		t.Fatalf("Publish after Close: got %v, want ErrEventBusClosed", err)
	}
}

// TestSubscribeWithCanceledContextReturnsClosedChannel preserves existing
// ctx-cancellation semantics for Subscribe.
func TestSubscribeWithCanceledContextReturnsClosedChannel(t *testing.T) {
	bus := NewEventBus(4)
	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()

	ch, cancel := bus.Subscribe(ctx)
	defer cancel()

	if _, ok := <-ch; ok {
		t.Fatalf("expected channel for canceled context to be closed immediately")
	}
}

// drainAvailable reads up to `max` immediately-available events from ch
// without blocking once no more are ready.
func drainAvailable(ch <-chan EventEnvelope, max int) []EventEnvelope {
	var got []EventEnvelope
	for len(got) < max {
		select {
		case event, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, event)
		default:
			return got
		}
	}
	return got
}
