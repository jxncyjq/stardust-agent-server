package evolution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrEvolutionEventSealed = errors.New("evolution event sealed")

type SealedEvolutionEvent struct {
	EvolutionEvent
	PreviousSeal string
	Seal         string
}

type SealedEvolutionEventLog struct {
	mu     sync.Mutex
	events []SealedEvolutionEvent
	byID   map[string]SealedEvolutionEvent
}

func NewSealedEvolutionEventLog() *SealedEvolutionEventLog {
	return &SealedEvolutionEventLog{
		byID: make(map[string]SealedEvolutionEvent),
	}
}

func (l *SealedEvolutionEventLog) Append(ctx context.Context, event EvolutionEvent) (SealedEvolutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return SealedEvolutionEvent{}, err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.byID[event.EventID]; ok {
		if sameEvolutionEvent(existing.EvolutionEvent, event) {
			return existing, nil
		}
		return SealedEvolutionEvent{}, ErrEvolutionEventSealed
	}
	previousSeal := ""
	if len(l.events) > 0 {
		previousSeal = l.events[len(l.events)-1].Seal
	}
	sealed := SealedEvolutionEvent{
		EvolutionEvent: event,
		PreviousSeal:   previousSeal,
		Seal:           sealEvolutionEvent(previousSeal, event),
	}
	l.events = append(l.events, sealed)
	l.byID[event.EventID] = sealed
	return sealed, nil
}

func (l *SealedEvolutionEventLog) Events(cycleID string) []SealedEvolutionEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var events []SealedEvolutionEvent
	for _, event := range l.events {
		if cycleID == "" || event.CycleID == cycleID {
			events = append(events, event)
		}
	}
	return append([]SealedEvolutionEvent(nil), events...)
}

func (l *SealedEvolutionEventLog) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	previousSeal := ""
	for _, event := range l.events {
		if event.PreviousSeal != previousSeal {
			return fmt.Errorf("verify event %q previous seal: %w", event.EventID, ErrEvolutionEventSealed)
		}
		if got := sealEvolutionEvent(event.PreviousSeal, event.EvolutionEvent); got != event.Seal {
			return fmt.Errorf("verify event %q seal: %w", event.EventID, ErrEvolutionEventSealed)
		}
		previousSeal = event.Seal
	}
	return nil
}

func sameEvolutionEvent(a, b EvolutionEvent) bool {
	return a.EventID == b.EventID &&
		a.CycleID == b.CycleID &&
		a.Stage == b.Stage &&
		a.AgentID == b.AgentID &&
		a.AssetID == b.AssetID &&
		a.EvidenceHash == b.EvidenceHash &&
		a.Decision == b.Decision
}

func sealEvolutionEvent(previousSeal string, event EvolutionEvent) string {
	return contentHash(fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		previousSeal,
		event.EventID,
		event.CycleID,
		event.Stage,
		event.AgentID,
		event.AssetID,
		event.EvidenceHash,
		event.Decision,
	))
}
