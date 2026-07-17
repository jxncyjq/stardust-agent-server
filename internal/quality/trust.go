package quality

import (
	"context"
	"sync"
	"time"
)

const (
	defaultTrustScore = 0.7
	minTrustScore     = 0.0
	maxTrustScore     = 1.0
	blockedThreshold  = 0.3
	cautiousThreshold = 0.5
)

type SecurityEventType string

const (
	SecurityEventPermissionDenied       SecurityEventType = "permission_denied"
	SecurityEventInjectionDetected      SecurityEventType = "injection_detected"
	SecurityEventSecretExposed          SecurityEventType = "secret_exposed"
	SecurityEventHardLoop               SecurityEventType = "hard_loop"
	SecurityEventHumanRecoverySucceeded SecurityEventType = "human_recovery_succeeded"
	SecurityEventSafeCompletion         SecurityEventType = "safe_completion"
)

type TrustDecision string

const (
	TrustDecisionAllow    TrustDecision = "allow"
	TrustDecisionCautious TrustDecision = "cautious"
	TrustDecisionBlocked  TrustDecision = "blocked"
)

type SecurityEvent struct {
	AgentID string
	Type    SecurityEventType
	At      time.Time
}

type TrustScoreManager struct {
	mu        sync.Mutex
	events    map[string][]SecurityEvent
	snapshots map[string][]TrustScoreSnapshot
}

func NewTrustScoreManager() *TrustScoreManager {
	return &TrustScoreManager{
		events:    make(map[string][]SecurityEvent),
		snapshots: make(map[string][]TrustScoreSnapshot),
	}
}

func (m *TrustScoreManager) LogSecurityEvent(ctx context.Context, event SecurityEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[event.AgentID] = append(m.events[event.AgentID], event)
	return nil
}

func (m *TrustScoreManager) EffectiveScore(ctx context.Context, agentID string, at time.Time) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	score := defaultTrustScore
	for _, event := range m.events[agentID] {
		if event.At.After(at) {
			continue
		}
		score = clampTrustScore(score + trustDelta(event.Type))
	}
	return score, nil
}

func (m *TrustScoreManager) CanExecute(ctx context.Context, agentID string, _ RiskLevel, at time.Time) (TrustDecision, error) {
	score, err := m.EffectiveScore(ctx, agentID, at)
	if err != nil {
		return "", err
	}
	return decisionForScore(score), nil
}

func (m *TrustScoreManager) Snapshot(ctx context.Context, agentID string, at time.Time) (TrustScoreSnapshot, error) {
	score, err := m.EffectiveScore(ctx, agentID, at)
	if err != nil {
		return TrustScoreSnapshot{}, err
	}
	snapshot := TrustScoreSnapshot{
		AgentID:   agentID,
		Score:     score,
		Decision:  decisionForScore(score),
		Reason:    trustReason(score),
		CreatedAt: at,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[agentID] = append(m.snapshots[agentID], snapshot)
	return snapshot, nil
}

func (m *TrustScoreManager) History(ctx context.Context, agentID string) ([]TrustScoreSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	history := append([]TrustScoreSnapshot(nil), m.snapshots[agentID]...)
	return history, nil
}

func decisionForScore(score float64) TrustDecision {
	if score < blockedThreshold {
		return TrustDecisionBlocked
	}
	if score < cautiousThreshold {
		return TrustDecisionCautious
	}
	return TrustDecisionAllow
}

func trustReason(score float64) string {
	switch decisionForScore(score) {
	case TrustDecisionBlocked:
		return "trust score below blocked threshold"
	case TrustDecisionCautious:
		return "trust score below cautious threshold"
	default:
		return "trust score allows execution"
	}
}

func trustDelta(eventType SecurityEventType) float64 {
	switch eventType {
	case SecurityEventPermissionDenied:
		return -0.1
	case SecurityEventInjectionDetected:
		return -0.25
	case SecurityEventSecretExposed:
		return -0.4
	case SecurityEventHardLoop:
		return -0.2
	case SecurityEventHumanRecoverySucceeded:
		return 0.15
	case SecurityEventSafeCompletion:
		return 0.05
	default:
		return 0
	}
}

func clampTrustScore(score float64) float64 {
	if score < minTrustScore {
		return minTrustScore
	}
	if score > maxTrustScore {
		return maxTrustScore
	}
	return score
}
