package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/task"
)

// securityEventForSignal maps a runtime learning signal onto a trust security
// event type. It reports false for signals that carry no trust consequence so
// the caller can skip them rather than recording a neutral event.
func securityEventForSignal(signal evolution.SignalKind) (quality.SecurityEventType, bool) {
	switch signal {
	case evolution.SignalPermissionViolation:
		return quality.SecurityEventPermissionDenied, true
	case evolution.SignalSecretExposure:
		return quality.SecurityEventSecretExposed, true
	case evolution.SignalHardLoopFailure:
		return quality.SecurityEventHardLoop, true
	case evolution.SignalSuccess:
		return quality.SecurityEventSafeCompletion, true
	default:
		return "", false
	}
}

// newTrustScoreScanJob builds a background job that translates trust-relevant
// runtime learning events published on the event bus into security events for
// the trust score manager. This is the minimal, non-invasive integration: it
// keeps trust scores observable and queryable from the same event stream the
// runtime already emits, without changing the coordinator dispatch path.
func newTrustScoreScanJob(events port.EventBus, manager *quality.TrustScoreManager) task.BackgroundJob {
	processed := make(map[string]bool)
	var mu sync.Mutex
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if events == nil || manager == nil {
			return nil
		}
		for _, event := range events.Events() {
			learning, ok := evolution.ParseLearningRuntimeEvent(event)
			if !ok {
				continue
			}
			eventType, relevant := securityEventForSignal(learning.Signal)
			if !relevant {
				continue
			}
			key := event.TaskID + ":" + event.Message + ":" + event.CreatedAt.UTC().Format(time.RFC3339Nano)
			mu.Lock()
			if processed[key] {
				mu.Unlock()
				continue
			}
			processed[key] = true
			mu.Unlock()
			at := learning.PublishedAt
			if at.IsZero() {
				at = event.CreatedAt
			}
			if err := manager.LogSecurityEvent(ctx, quality.SecurityEvent{
				AgentID: learning.AgentID,
				Type:    eventType,
				At:      at,
			}); err != nil {
				return fmt.Errorf("log trust security event for %s: %w", learning.TaskID, err)
			}
		}
		return nil
	}
}
