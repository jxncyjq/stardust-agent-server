package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxCapabilityGeneInjection = 3

var ErrCapabilityNotFound = errors.New("capability asset not found")

type GeneStatus string

const (
	GeneStatusDraft  GeneStatus = "draft"
	GeneStatusActive GeneStatus = "active"
	GeneStatusFrozen GeneStatus = "frozen"
)

type CapabilityOutcome string

const (
	CapabilityOutcomeSuccess CapabilityOutcome = "success"
	CapabilityOutcomeFailure CapabilityOutcome = "failure"
)

type Gene struct {
	ID           string
	Version      string
	Status       GeneStatus
	Tags         []string
	Match        string
	UseWhen      string
	Plan         string
	Avoid        string
	Constraints  string
	Validation   string
	SuccessRate  float64
	SuccessCount int
	FailureCount int
	UpdatedAt    time.Time
}

type Capsule struct {
	ID           string
	GeneIDs      []string
	Query        string
	Tags         []string
	Outcome      string
	SuccessCount int
	Confidence   float64
	CreatedAt    time.Time
}

type CapabilityQuery struct {
	Text string
	Tags []string
	TopK int
}

type GeneHit struct {
	Gene  Gene
	Score float64
}

type CapsuleHit struct {
	Capsule Capsule
	Score   float64
}

type CapabilityMemoryStore struct {
	mu       sync.Mutex
	genes    map[string]Gene
	capsules map[string]Capsule
}

func NewCapabilityMemoryStore() *CapabilityMemoryStore {
	return &CapabilityMemoryStore{
		genes:    make(map[string]Gene),
		capsules: make(map[string]Capsule),
	}
}

func (s *CapabilityMemoryStore) PutGene(ctx context.Context, gene Gene) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gene.Status == "" {
		gene.Status = GeneStatusDraft
	}
	if gene.UpdatedAt.IsZero() {
		gene.UpdatedAt = time.Now()
	}
	if gene.SuccessRate == 0 && gene.SuccessCount+gene.FailureCount > 0 {
		gene.SuccessRate = successRate(gene.SuccessCount, gene.FailureCount)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.genes[gene.ID] = copyGene(gene)
	return nil
}

func (s *CapabilityMemoryStore) SearchGenes(ctx context.Context, query CapabilityQuery) ([]GeneHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := query.TopK
	if limit <= 0 || limit > maxCapabilityGeneInjection {
		limit = maxCapabilityGeneInjection
	}
	queryText := strings.ToLower(query.Text)
	queryTags := tagSet(query.Tags)

	s.mu.Lock()
	defer s.mu.Unlock()
	hits := make([]GeneHit, 0, len(s.genes))
	for _, gene := range s.genes {
		if gene.Status != GeneStatusActive {
			continue
		}
		score := scoreGene(queryText, queryTags, gene)
		if score <= 0 {
			continue
		}
		hits = append(hits, GeneHit{
			Gene:  copyGene(gene),
			Score: score,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Gene.ID < hits[j].Gene.ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return copyGeneHits(hits), nil
}

func (s *CapabilityMemoryStore) MarkOutcome(ctx context.Context, geneID string, outcome CapabilityOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gene, ok := s.genes[geneID]
	if !ok {
		return ErrCapabilityNotFound
	}
	switch outcome {
	case CapabilityOutcomeSuccess:
		gene.SuccessCount++
	case CapabilityOutcomeFailure:
		gene.FailureCount++
	}
	gene.SuccessRate = successRate(gene.SuccessCount, gene.FailureCount)
	gene.UpdatedAt = time.Now()
	s.genes[geneID] = gene
	return nil
}

func (s *CapabilityMemoryStore) FreezeGene(ctx context.Context, geneID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gene, ok := s.genes[geneID]
	if !ok {
		return ErrCapabilityNotFound
	}
	gene.Status = GeneStatusFrozen
	gene.UpdatedAt = time.Now()
	s.genes[geneID] = gene
	return nil
}

func (s *CapabilityMemoryStore) PromoteCapsule(ctx context.Context, capsule Capsule) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if capsule.CreatedAt.IsZero() {
		capsule.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capsules[capsule.ID] = copyCapsule(capsule)
	return nil
}

func (s *CapabilityMemoryStore) SearchCapsules(ctx context.Context, query CapabilityQuery) ([]CapsuleHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query.TopK <= 0 {
		return nil, nil
	}
	queryText := strings.ToLower(query.Text)
	queryTags := tagSet(query.Tags)

	s.mu.Lock()
	defer s.mu.Unlock()
	hits := make([]CapsuleHit, 0, len(s.capsules))
	for _, capsule := range s.capsules {
		score := scoreCapsule(queryText, queryTags, capsule)
		if score <= 0 {
			continue
		}
		hits = append(hits, CapsuleHit{
			Capsule: copyCapsule(capsule),
			Score:   score,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Capsule.ID < hits[j].Capsule.ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > query.TopK {
		hits = hits[:query.TopK]
	}
	return copyCapsuleHits(hits), nil
}

func scoreGene(queryText string, queryTags map[string]bool, gene Gene) float64 {
	score := gene.SuccessRate*10 + float64(gene.SuccessCount)*0.2 - float64(gene.FailureCount)*0.5
	score += float64(tagMatches(queryTags, gene.Tags)) * 4
	text := strings.ToLower(strings.Join([]string{
		gene.ID,
		gene.Match,
		gene.UseWhen,
		gene.Plan,
		gene.Avoid,
		gene.Constraints,
		gene.Validation,
		strings.Join(gene.Tags, " "),
	}, " "))
	if queryText != "" && strings.Contains(text, queryText) {
		score += 3
	}
	for _, word := range strings.Fields(queryText) {
		if strings.Contains(text, word) {
			score++
		}
	}
	return score
}

func scoreCapsule(queryText string, queryTags map[string]bool, capsule Capsule) float64 {
	score := capsule.Confidence*10 + float64(capsule.SuccessCount)*0.3
	score += float64(tagMatches(queryTags, capsule.Tags)) * 4
	text := strings.ToLower(capsule.Query + " " + strings.Join(capsule.Tags, " ") + " " + strings.Join(capsule.GeneIDs, " "))
	if queryText != "" && strings.Contains(text, queryText) {
		score += 3
	}
	for _, word := range strings.Fields(queryText) {
		if strings.Contains(text, word) {
			score++
		}
	}
	return score
}

func successRate(successCount int, failureCount int) float64 {
	total := successCount + failureCount
	if total == 0 {
		return 0
	}
	return float64(successCount) / float64(total)
}

func tagSet(tags []string) map[string]bool {
	set := make(map[string]bool, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		set[tag] = true
	}
	return set
}

func tagMatches(queryTags map[string]bool, assetTags []string) int {
	var matches int
	for _, tag := range assetTags {
		if queryTags[strings.ToLower(tag)] {
			matches++
		}
	}
	return matches
}

func copyGene(gene Gene) Gene {
	gene.Tags = append([]string(nil), gene.Tags...)
	return gene
}

func copyCapsule(capsule Capsule) Capsule {
	capsule.GeneIDs = append([]string(nil), capsule.GeneIDs...)
	capsule.Tags = append([]string(nil), capsule.Tags...)
	return capsule
}

func copyGeneHits(hits []GeneHit) []GeneHit {
	out := make([]GeneHit, len(hits))
	for idx, hit := range hits {
		out[idx] = GeneHit{
			Gene:  copyGene(hit.Gene),
			Score: hit.Score,
		}
	}
	return out
}

func copyCapsuleHits(hits []CapsuleHit) []CapsuleHit {
	out := make([]CapsuleHit, len(hits))
	for idx, hit := range hits {
		out[idx] = CapsuleHit{
			Capsule: copyCapsule(hit.Capsule),
			Score:   hit.Score,
		}
	}
	return out
}
