package quality

import (
	"context"
	"fmt"
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
	// Validate at the door. An event whose type carries no defined weight cannot
	// be scored later without either failing the read or silently counting zero,
	// so it must not enter the store in the first place.
	if _, err := trustDelta(event.Type); err != nil {
		return fmt.Errorf("log security event for agent %q: %w", event.AgentID, err)
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
		// Not merely defensive duplication of LogSecurityEvent's gate: an event
		// already in the store — recorded before a type was renamed, or written by
		// some future path — must not be scored as a silent zero either.
		delta, err := trustDelta(event.Type)
		if err != nil {
			return 0, fmt.Errorf("score agent %q: %w", agentID, err)
		}
		score = clampTrustScore(score + delta)
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

// trustDelta maps a security event to its contribution to an agent's trust
// score. An unrecognised type is an error, not a zero.
//
// Returning 0 for the default branch looked harmless and was not: the trust
// score is the coordinator's execution gate (CanExecute → TrustDecisionBlocked
// suspends the task), so a type this switch has not been taught about — a new
// one added without updating it, or a misspelled string from upstream —
// contributed nothing to the score and an agent that should have been blocked
// kept running, with nothing anywhere reporting it.
func trustDelta(eventType SecurityEventType) (float64, error) {
	switch eventType {
	case SecurityEventPermissionDenied:
		return -0.1, nil
	case SecurityEventInjectionDetected:
		return -0.25, nil
	case SecurityEventSecretExposed:
		return -0.4, nil
	case SecurityEventHardLoop:
		return -0.2, nil
	case SecurityEventHumanRecoverySucceeded:
		return 0.15, nil
	case SecurityEventSafeCompletion:
		return 0.05, nil
	default:
		return 0, fmt.Errorf("unknown security event type %q", eventType)
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
