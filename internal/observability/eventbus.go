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

func NewEventBus(buffer int) *EventBus {
	if buffer < 1 {
		buffer = 1
	}
	return &EventBus{
		buffer:      buffer,
		subscribers: make(map[chan EventEnvelope]struct{}),
	}
}

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
	for subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
	return nil
}

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
		select {
		case ch <- event:
		default:
		}
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
