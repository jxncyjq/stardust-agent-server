package sessioncache

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestMemoryCacheReturnsCopiesAndTracksStats(t *testing.T) {
	t.Parallel()

	cache := NewMemoryCache(2)
	key := Key{
		CompanyID:    "company-1",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		ModelProfile: "dev",
		RecentTurns:  4,
		MaxTurnChars: 6000,
	}
	turns := []domain.ConversationTurn{{ID: "turn-1", Content: "first"}}

	if _, ok := cache.Get(key); ok {
		t.Fatalf("Get(%#v) ok = true, want false before Put", key)
	}
	cache.Put(key, turns)

	got, ok := cache.Get(key)
	if !ok {
		t.Fatalf("Get(%#v) ok = false, want true after Put", key)
	}
	got[0].Content = "mutated"
	gotAgain, ok := cache.Get(key)
	if !ok {
		t.Fatalf("Get(%#v) second ok = false, want true", key)
	}
	if gotAgain[0].Content != "first" {
		t.Fatalf("cached turn content = %q, want immutable copy", gotAgain[0].Content)
	}

	stats := cache.Stats()
	if stats.Entries != 1 || stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("Stats() = %#v, want entries=1 hits=2 misses=1", stats)
	}
}

func TestMemoryCacheEvictsOldestEntryWhenFull(t *testing.T) {
	t.Parallel()

	cache := NewMemoryCache(1)
	first := Key{SessionID: "session-1", RecentTurns: 4, MaxTurnChars: 6000}
	second := Key{SessionID: "session-2", RecentTurns: 4, MaxTurnChars: 6000}

	cache.Put(first, []domain.ConversationTurn{{ID: "turn-1"}})
	cache.Put(second, []domain.ConversationTurn{{ID: "turn-2"}})

	if _, ok := cache.Get(first); ok {
		t.Fatalf("Get(first) ok = true, want evicted")
	}
	if _, ok := cache.Get(second); !ok {
		t.Fatalf("Get(second) ok = false, want retained")
	}
	if got := cache.Stats().Evictions; got != 1 {
		t.Fatalf("Stats().Evictions = %d, want 1", got)
	}
}

func TestMemoryCacheInvalidatesBySession(t *testing.T) {
	t.Parallel()

	cache := NewMemoryCache(4)
	sessionOne := Key{SessionID: "session-1", RecentTurns: 4, MaxTurnChars: 6000}
	sessionTwo := Key{SessionID: "session-2", RecentTurns: 4, MaxTurnChars: 6000}
	cache.Put(sessionOne, []domain.ConversationTurn{{ID: "turn-1"}})
	cache.Put(sessionTwo, []domain.ConversationTurn{{ID: "turn-2"}})

	cache.InvalidateSession("session-1")

	if _, ok := cache.Get(sessionOne); ok {
		t.Fatalf("Get(sessionOne) ok = true, want invalidated")
	}
	if _, ok := cache.Get(sessionTwo); !ok {
		t.Fatalf("Get(sessionTwo) ok = false, want retained")
	}
	if got := cache.Stats().Entries; got != 1 {
		t.Fatalf("Stats().Entries = %d, want 1", got)
	}
}
