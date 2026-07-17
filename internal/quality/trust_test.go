package quality

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestTrustScoreManagerStartsAtDefaultScore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewTrustScoreManager()
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	got, err := manager.EffectiveScore(ctx, "agent-1", at)
	if err != nil {
		t.Fatalf("EffectiveScore(agent-1) error = %v, want nil", err)
	}
	if !sameScore(got, 0.7) {
		t.Fatalf("EffectiveScore(agent-1) = %.4f, want 0.7000", got)
	}
}

func TestTrustScoreManagerRecalculatesFromSecurityEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewTrustScoreManager()
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	events := []SecurityEvent{
		{AgentID: "agent-1", Type: SecurityEventPermissionDenied, At: at},
		{AgentID: "agent-1", Type: SecurityEventHardLoop, At: at.Add(time.Minute)},
		{AgentID: "agent-1", Type: SecurityEventSafeCompletion, At: at.Add(2 * time.Minute)},
	}
	for _, event := range events {
		if err := manager.LogSecurityEvent(ctx, event); err != nil {
			t.Fatalf("LogSecurityEvent(%s) error = %v, want nil", event.Type, err)
		}
	}

	got, err := manager.EffectiveScore(ctx, "agent-1", at.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("EffectiveScore(agent-1) error = %v, want nil", err)
	}
	if !sameScore(got, 0.45) {
		t.Fatalf("EffectiveScore(agent-1) = %.4f, want 0.4500", got)
	}
}

func TestTrustScoreManagerReturnsDispatchDecisionBands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewTrustScoreManager()
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	cautiousAgent := "agent-cautious"
	for _, eventType := range []SecurityEventType{
		SecurityEventPermissionDenied,
		SecurityEventHardLoop,
	} {
		if err := manager.LogSecurityEvent(ctx, SecurityEvent{AgentID: cautiousAgent, Type: eventType, At: at}); err != nil {
			t.Fatalf("LogSecurityEvent(%s) error = %v, want nil", eventType, err)
		}
	}
	blockedAgent := "agent-blocked"
	if err := manager.LogSecurityEvent(ctx, SecurityEvent{AgentID: blockedAgent, Type: SecurityEventSecretExposed, At: at}); err != nil {
		t.Fatalf("LogSecurityEvent(secret_exposed) error = %v, want nil", err)
	}
	if err := manager.LogSecurityEvent(ctx, SecurityEvent{AgentID: blockedAgent, Type: SecurityEventInjectionDetected, At: at}); err != nil {
		t.Fatalf("LogSecurityEvent(injection_detected) error = %v, want nil", err)
	}

	for _, tc := range []struct {
		name    string
		agentID string
		want    TrustDecision
	}{
		{name: "default score allows", agentID: "agent-allow", want: TrustDecisionAllow},
		{name: "mid score is cautious", agentID: cautiousAgent, want: TrustDecisionCautious},
		{name: "low score blocks", agentID: blockedAgent, want: TrustDecisionBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := manager.CanExecute(ctx, tc.agentID, RiskHigh, at)
			if err != nil {
				t.Fatalf("CanExecute(%q) error = %v, want nil", tc.agentID, err)
			}
			if got != tc.want {
				t.Fatalf("CanExecute(%q) = %s, want %s", tc.agentID, got, tc.want)
			}
		})
	}
}

func TestTrustScoreManagerIgnoresFutureEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	manager := NewTrustScoreManager()
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	if err := manager.LogSecurityEvent(ctx, SecurityEvent{
		AgentID: "agent-1",
		Type:    SecurityEventSecretExposed,
		At:      at.Add(time.Hour),
	}); err != nil {
		t.Fatalf("LogSecurityEvent(secret_exposed) error = %v, want nil", err)
	}

	got, err := manager.EffectiveScore(ctx, "agent-1", at)
	if err != nil {
		t.Fatalf("EffectiveScore(agent-1) error = %v, want nil", err)
	}
	if !sameScore(got, 0.7) {
		t.Fatalf("EffectiveScore(agent-1) = %.4f, want 0.7000", got)
	}
}

func sameScore(got, want float64) bool {
	return math.Abs(got-want) < 0.0001
}
