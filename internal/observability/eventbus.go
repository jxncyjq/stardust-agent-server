package observability

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrEventBusClosed = errors.New("event bus closed")

type EventEnvelope struct {
	ID        string         `json:"id,omitempty"`
	Type      string         `json:"type"`
	SubjectID string         `json:"subject_id,omitempty"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}

// EventBus is a small in-memory publish/subscribe bus for platform-facing events.
type EventBus struct {
	mu          sync.Mutex
	buffer      int
	closed      bool
	events      []EventEnvelope
	subscribers map[chan EventEnvelope]struct{}
}

// NewEventBus creates an EventBus whose per-subscriber channel capacity is
// buffer (minimum 1). buffer also bounds retained history: Publish keeps at
// most the most recent buffer events, and Subscribe replays exactly that
// retained history into every new subscriber without dropping any of it.
func NewEventBus(buffer int) *EventBus {
	if buffer < 1 {
		buffer = 1
	}
	return &EventBus{
		buffer:      buffer,
		subscribers: make(map[chan EventEnvelope]struct{}),
	}
}

// Publish appends event to the bus history and fans it out to all current
// subscribers. History is retained as a ring bounded to the bus's buffer
// size (see NewEventBus): once more than buffer events have been published,
// the oldest entries are dropped so only the most recent buffer events are
// kept. This keeps memory usage bounded for long-running processes.
//
// Live delivery to subscribers remains non-blocking: a subscriber whose
// channel is already full drops that event (existing contract, unchanged
// here) — this only affects live fan-out, not what gets retained in history
// for future subscribers.
func (b *EventBus) Publish(ctx context.Context, event EventEnvelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrEventBusClosed
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if event.Data == nil {
		event.Data = map[string]any{}
	}
	b.events = append(b.events, event)
	if len(b.events) > b.buffer {
		overflow := len(b.events) - b.buffer
		copy(b.events, b.events[overflow:])
		b.events = b.events[:b.buffer]
	}
	for subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
	return nil
}

// Subscribe registers a new subscriber and replays the retained history
// (the most recent up to buffer events, see Publish) into its channel
// before returning it. Because the channel's capacity equals the bus's
// buffer size and history is bounded to that same size, the full replay is
// guaranteed to fit without dropping anything — late subscribers always
// receive the most recent events, including the latest one published.
func (b *EventBus) Subscribe(ctx context.Context) (<-chan EventEnvelope, func()) {
	if err := ctx.Err(); err != nil {
		ch := make(chan EventEnvelope)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan EventEnvelope, b.buffer)
	b.mu.Lock()
	if b.closed {
		close(ch)
		b.mu.Unlock()
		return ch, func() {}
	}
	for _, event := range b.events {
		ch <- event
	}
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subscribers[ch]; ok {
				delete(b.subscribers, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

func (b *EventBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for subscriber := range b.subscribers {
		close(subscriber)
		delete(b.subscribers, subscriber)
	}
	return nil
}
