package memory

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

type episodicRecord struct {
	entry     domain.MemoryEntry
	embedding []float64
}

type EpisodicMemoryStore struct {
	mu       sync.Mutex
	nextID   int
	embedder port.EmbeddingProvider
	records  []episodicRecord
}

func NewEpisodicMemoryStore(embedder port.EmbeddingProvider) *EpisodicMemoryStore {
	return &EpisodicMemoryStore{embedder: embedder}
}

func (s *EpisodicMemoryStore) Add(ctx context.Context, agent domain.Agent, task domain.Task, content string) (domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryEntry{}, err
	}
	var embedding []float64
	if s.embedder != nil {
		vector, err := s.embedder.Embed(ctx, content)
		if err != nil {
			return domain.MemoryEntry{}, err
		}
		embedding = append([]float64(nil), vector...)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	entry := domain.MemoryEntry{
		ID:        "episodic-memory-" + strconv.Itoa(s.nextID),
		AgentID:   agent.ID,
		TaskID:    task.ID,
		Content:   content,
		CreatedAt: time.Now(),
	}
	s.records = append(s.records, episodicRecord{
		entry:     entry,
		embedding: embedding,
	})
	return entry, nil
}

func (s *EpisodicMemoryStore) Search(ctx context.Context, query string, topK int) ([]domain.MemoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return nil, nil
	}
	if s.embedder == nil {
		return s.searchByText(query, topK), nil
	}
	queryEmbedding, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.searchByEmbedding(queryEmbedding, topK), nil
}

func (s *EpisodicMemoryStore) searchByText(query string, topK int) []domain.MemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	lowerQuery := strings.ToLower(query)
	var matches []domain.MemoryEntry
	for _, record := range s.records {
		if lowerQuery == "" || strings.Contains(strings.ToLower(record.entry.Content), lowerQuery) {
			matches = append(matches, record.entry)
			if len(matches) == topK {
				break
			}
		}
	}
	return append([]domain.MemoryEntry(nil), matches...)
}

func (s *EpisodicMemoryStore) searchByEmbedding(queryEmbedding []float64, topK int) []domain.MemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	type scoredRecord struct {
		entry domain.MemoryEntry
		score float64
		order int
	}
	scored := make([]scoredRecord, 0, len(s.records))
	for idx, record := range s.records {
		scored = append(scored, scoredRecord{
			entry: record.entry,
			score: cosineSimilarity(queryEmbedding, record.embedding),
			order: idx,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].order < scored[j].order
		}
		return scored[i].score > scored[j].score
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}
	results := make([]domain.MemoryEntry, 0, len(scored))
	for _, record := range scored {
		results = append(results, record.entry)
	}
	return results
}

func cosineSimilarity(left, right []float64) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	n := len(left)
	n = min(n, len(right))
	var dot, leftNorm, rightNorm float64
	for idx := range n {
		dot += left[idx] * right[idx]
		leftNorm += left[idx] * left[idx]
		rightNorm += right[idx] * right[idx]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}
