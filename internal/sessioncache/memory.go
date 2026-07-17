package sessioncache

import (
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
)

type Key struct {
	CompanyID    string
	AgentID      string
	SessionID    string
	ModelProfile string
	RecentTurns  int
	MaxTurnChars int
}

type Stats struct {
	Entries   int
	Hits      int64
	Misses    int64
	Evictions int64
}

type MemoryCache struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[Key][]domain.ConversationTurn
	order      []Key
	hits       int64
	misses     int64
	evictions  int64
}

func NewMemoryCache(maxEntries int) *MemoryCache {
	if maxEntries <= 0 {
		maxEntries = 128
	}
	return &MemoryCache{
		maxEntries: maxEntries,
		entries:    make(map[Key][]domain.ConversationTurn),
	}
}

func (c *MemoryCache) Get(key Key) ([]domain.ConversationTurn, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	turns, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	return cloneTurns(turns), true
}

func (c *MemoryCache) Put(key Key, turns []domain.ConversationTurn) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.entries[key]; !ok {
		c.order = append(c.order, key)
	}
	c.entries[key] = cloneTurns(turns)
	c.evictLocked()
}

func (c *MemoryCache) InvalidateSession(sessionID string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	next := c.order[:0]
	for _, key := range c.order {
		if key.SessionID == sessionID {
			delete(c.entries, key)
			continue
		}
		next = append(next, key)
	}
	c.order = next
}

func (c *MemoryCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Entries:   len(c.entries),
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
	}
}

func (c *MemoryCache) evictLocked() {
	for len(c.entries) > c.maxEntries && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if _, ok := c.entries[oldest]; ok {
			delete(c.entries, oldest)
			c.evictions++
		}
	}
}

func cloneTurns(turns []domain.ConversationTurn) []domain.ConversationTurn {
	if len(turns) == 0 {
		return nil
	}
	return append([]domain.ConversationTurn(nil), turns...)
}
