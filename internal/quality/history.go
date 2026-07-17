package quality

import (
	"context"
	"sync"
	"time"
)

type EvalRunRecord struct {
	ID        string
	AgentID   string
	TaskID    string
	Component string
	Status    EvalStatus
	Reason    string
	Score     float64
	CreatedAt time.Time
}

type TrustScoreSnapshot struct {
	AgentID   string
	Score     float64
	Decision  TrustDecision
	Reason    string
	CreatedAt time.Time
}

type DegradationDecision struct {
	AgentID     string
	Component   string
	Decision    string
	Reason      string
	QualityDrop float64
	CreatedAt   time.Time
}

type TrendQuery struct {
	AgentID   string
	TaskID    string
	Component string
	Since     time.Time
	Until     time.Time
}

type EvalTrend struct {
	Total        int
	ByStatus     map[EvalStatus]int
	Latest       EvalRunRecord
	AverageScore float64
}

type QualitySummary struct {
	EvalRuns             int
	LatestEvalStatus     EvalStatus
	TrustSnapshots       int
	LatestTrustDecision  TrustDecision
	DegradationDecisions int
}

type QualityHistoryStore struct {
	mu                   sync.Mutex
	evalRuns             []EvalRunRecord
	trustSnapshots       []TrustScoreSnapshot
	degradationDecisions []DegradationDecision
}

func NewQualityHistoryStore() *QualityHistoryStore {
	return &QualityHistoryStore{}
}

func (s *QualityHistoryStore) AppendEvalRun(ctx context.Context, record EvalRunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evalRuns = append(s.evalRuns, record)
	return nil
}

func (s *QualityHistoryStore) EvalTrend(ctx context.Context, query TrendQuery) (EvalTrend, error) {
	if err := ctx.Err(); err != nil {
		return EvalTrend{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trend := EvalTrend{ByStatus: make(map[EvalStatus]int)}
	var scoreSum float64
	for _, record := range s.evalRuns {
		if !matchesEvalRecord(record, query) {
			continue
		}
		trend.Total++
		trend.ByStatus[record.Status]++
		scoreSum += record.Score
		if record.CreatedAt.After(trend.Latest.CreatedAt) || trend.Latest.ID == "" {
			trend.Latest = record
		}
	}
	if trend.Total > 0 {
		trend.AverageScore = scoreSum / float64(trend.Total)
	}
	return trend, nil
}

func (s *QualityHistoryStore) AppendTrustSnapshot(ctx context.Context, snapshot TrustScoreSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trustSnapshots = append(s.trustSnapshots, snapshot)
	return nil
}

func (s *QualityHistoryStore) TrustHistory(ctx context.Context, agentID string) ([]TrustScoreSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var history []TrustScoreSnapshot
	for _, snapshot := range s.trustSnapshots {
		if agentID != "" && snapshot.AgentID != agentID {
			continue
		}
		history = append(history, snapshot)
	}
	return history, nil
}

func (s *QualityHistoryStore) AppendDegradationDecision(ctx context.Context, decision DegradationDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.degradationDecisions = append(s.degradationDecisions, decision)
	return nil
}

func (s *QualityHistoryStore) DegradationDecisions(ctx context.Context, query TrendQuery) ([]DegradationDecision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var decisions []DegradationDecision
	for _, decision := range s.degradationDecisions {
		if !matchesDegradationDecision(decision, query) {
			continue
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func (s *QualityHistoryStore) Summary(ctx context.Context, query TrendQuery) (QualitySummary, error) {
	if err := ctx.Err(); err != nil {
		return QualitySummary{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var summary QualitySummary
	var latestEval EvalRunRecord
	var latestTrust TrustScoreSnapshot
	for _, record := range s.evalRuns {
		if !matchesEvalRecord(record, query) {
			continue
		}
		summary.EvalRuns++
		if record.CreatedAt.After(latestEval.CreatedAt) || latestEval.ID == "" {
			latestEval = record
			summary.LatestEvalStatus = record.Status
		}
	}
	for _, snapshot := range s.trustSnapshots {
		if query.AgentID != "" && snapshot.AgentID != query.AgentID {
			continue
		}
		if !withinRange(snapshot.CreatedAt, query) {
			continue
		}
		summary.TrustSnapshots++
		if snapshot.CreatedAt.After(latestTrust.CreatedAt) || latestTrust.AgentID == "" {
			latestTrust = snapshot
			summary.LatestTrustDecision = snapshot.Decision
		}
	}
	for _, decision := range s.degradationDecisions {
		if matchesDegradationDecision(decision, query) {
			summary.DegradationDecisions++
		}
	}
	return summary, nil
}

func matchesEvalRecord(record EvalRunRecord, query TrendQuery) bool {
	if query.AgentID != "" && record.AgentID != query.AgentID {
		return false
	}
	if query.TaskID != "" && record.TaskID != query.TaskID {
		return false
	}
	if query.Component != "" && record.Component != query.Component {
		return false
	}
	return withinRange(record.CreatedAt, query)
}

func matchesDegradationDecision(decision DegradationDecision, query TrendQuery) bool {
	if query.AgentID != "" && decision.AgentID != query.AgentID {
		return false
	}
	if query.Component != "" && decision.Component != query.Component {
		return false
	}
	return withinRange(decision.CreatedAt, query)
}

func withinRange(at time.Time, query TrendQuery) bool {
	if !query.Since.IsZero() && at.Before(query.Since) {
		return false
	}
	if !query.Until.IsZero() && at.After(query.Until) {
		return false
	}
	return true
}
