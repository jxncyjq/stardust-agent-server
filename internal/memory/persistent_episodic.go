package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// EpisodicStore is the write+search contract the cognitive memory provider
// depends on. Both the in-memory *EpisodicMemoryStore (tests / non-sqlite
// fallback) and the SQLite-backed *PersistentEpisodicStore satisfy it.
type EpisodicStore interface {
	Add(ctx context.Context, agent domain.Agent, task domain.Task, content string) (domain.MemoryEntry, error)
	Search(ctx context.Context, query string, topK int) ([]domain.MemoryEntry, error)
}

// EpisodicPersister is the durable backend PersistentEpisodicStore writes
// through. *storage.SQLiteRepository satisfies it structurally (methods added in
// slice B1); declaring it here keeps the memory package free of a storage import.
type EpisodicPersister interface {
	AddEpisodicMemory(ctx context.Context, entry domain.MemoryEntry) error
	SearchEpisodicMemory(ctx context.Context, query string, topK int) ([]domain.MemoryEntry, error)
}

// PersistentEpisodicStore is an EpisodicStore whose entries survive restarts by
// persisting through an EpisodicPersister.
type PersistentEpisodicStore struct {
	persister EpisodicPersister
}

func NewPersistentEpisodicStore(persister EpisodicPersister) *PersistentEpisodicStore {
	return &PersistentEpisodicStore{persister: persister}
}

func (s *PersistentEpisodicStore) Add(ctx context.Context, agent domain.Agent, task domain.Task, content string) (domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryEntry{}, err
	}
	entry := domain.MemoryEntry{
		ID:        newEpisodicID(),
		AgentID:   agent.ID,
		TaskID:    task.ID,
		Content:   content,
		CreatedAt: time.Now(),
	}
	if err := s.persister.AddEpisodicMemory(ctx, entry); err != nil {
		return domain.MemoryEntry{}, fmt.Errorf("persist episodic entry %q: %w", entry.ID, err)
	}
	return entry, nil
}

func (s *PersistentEpisodicStore) Search(ctx context.Context, query string, topK int) ([]domain.MemoryEntry, error) {
	return s.persister.SearchEpisodicMemory(ctx, query, topK)
}

// newEpisodicID mirrors internal/server/http.go's newRequestID: a random hex id
// with a stable prefix, falling back to a timestamp if the RNG fails.
func newEpisodicID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("episodic-%d", time.Now().UnixNano())
	}
	return "episodic-" + hex.EncodeToString(data[:])
}
