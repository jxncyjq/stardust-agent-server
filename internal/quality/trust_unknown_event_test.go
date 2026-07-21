package quality

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Audit item V10: trustDelta's default branch returned 0 for any event type it
// did not recognise.
//
// A silent 0 is not a neutral default here. The trust score is the coordinator's
// execution gate (CanExecute -> TrustDecisionBlocked suspends the task), so a
// security event type that this switch has not been taught about — a new one
// added without updating it, or a misspelled string from upstream — contributes
// nothing, and an agent that should have been blocked keeps running. Nothing
// anywhere reports it.

func TestLogSecurityEventRejectsUnknownType(t *testing.T) {
	t.Parallel()

	m := NewTrustScoreManager()
	err := m.LogSecurityEvent(context.Background(), SecurityEvent{
		AgentID: "agent-1",
		Type:    SecurityEventType("permision_denied"), // deliberately misspelled
		At:      time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("LogSecurityEvent(unknown type) error = nil, want an error naming the type")
	}
	if !strings.Contains(err.Error(), "permision_denied") {
		t.Errorf("error = %q, want it to name the rejected type", err.Error())
	}
}

func TestLogSecurityEventAcceptsEveryKnownType(t *testing.T) {
	t.Parallel()

	// Guards the other direction: a validation that rejected a legitimate type
	// would silently stop recording real security signals, which is worse than
	// the bug being fixed.
	known := []SecurityEventType{
		SecurityEventPermissionDenied,
		SecurityEventInjectionDetected,
		SecurityEventSecretExposed,
		SecurityEventHardLoop,
		SecurityEventHumanRecoverySucceeded,
		SecurityEventSafeCompletion,
	}
	m := NewTrustScoreManager()
	for _, eventType := range known {
		if err := m.LogSecurityEvent(context.Background(), SecurityEvent{
			AgentID: "agent-1",
			Type:    eventType,
			At:      time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Errorf("LogSecurityEvent(%q) error = %v, want nil", eventType, err)
		}
	}
}

// TestEffectiveScoreFailsLoudOnUnknownStoredType covers the read side. The write
// gate above cannot be the only check: an event already in the store (recorded
// before a type was renamed, or written by a future code path) must not be
// scored as a silent zero either.
func TestEffectiveScoreFailsLoudOnUnknownStoredType(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	m := NewTrustScoreManager()
	// Bypass the write gate on purpose — same package, so the store is reachable.
	m.events["agent-1"] = []SecurityEvent{{AgentID: "agent-1", Type: SecurityEventType("retired_type"), At: at}}

	if _, err := m.EffectiveScore(context.Background(), "agent-1", at); err == nil {
		t.Fatal("EffectiveScore(unknown stored type) error = nil, want an error")
	}
}
