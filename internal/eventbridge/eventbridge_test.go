package eventbridge

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
)

func TestBridgeTeesRuntimeEventToPlatform(t *testing.T) {
	platform := observability.NewEventBus(8)
	b := New(platform, nil)
	events, cancel := platform.Subscribe(context.Background())
	defer cancel()

	err := b.Publish(context.Background(), domain.RuntimeEvent{
		Type:        "task_completed",
		TaskID:      "task-1",
		Message:     "done",
		TotalTokens: 5,
		CreatedAt:   time.Unix(1000, 0),
	})
	if err != nil {
		t.Fatalf("Bridge.Publish() error = %v, want nil", err)
	}

	select {
	case env := <-events:
		if env.Type != "task_completed" {
			t.Fatalf("envelope Type = %q, want task_completed", env.Type)
		}
		if env.SubjectID != "task-1" {
			t.Fatalf("envelope SubjectID = %q, want task-1", env.SubjectID)
		}
		if env.Data["task_id"] != "task-1" {
			t.Fatalf("envelope Data[task_id] = %v, want task-1", env.Data["task_id"])
		}
		if env.Data["message"] != "done" {
			t.Fatalf("envelope Data[message] = %v, want done", env.Data["message"])
		}
		if env.Data["total_tokens"] != 5 {
			t.Fatalf("envelope Data[total_tokens] = %v, want 5", env.Data["total_tokens"])
		}
		if !env.CreatedAt.Equal(time.Unix(1000, 0)) {
			t.Fatalf("envelope CreatedAt = %v, want 1000", env.CreatedAt)
		}
	case <-time.After(time.Second):
		t.Fatal("platform subscriber received no event, want teed envelope")
	}
}

func TestBridgePreservesEventsSnapshotForPollConsumers(t *testing.T) {
	platform := observability.NewEventBus(8)
	b := New(platform, nil)
	first := domain.RuntimeEvent{Type: "task_started", TaskID: "t1"}
	second := domain.RuntimeEvent{Type: "task_completed", TaskID: "t1"}
	if err := b.Publish(context.Background(), first); err != nil {
		t.Fatalf("Publish(first) error = %v, want nil", err)
	}
	if err := b.Publish(context.Background(), second); err != nil {
		t.Fatalf("Publish(second) error = %v, want nil", err)
	}
	got := b.Events()
	if len(got) != 2 || got[0].Type != "task_started" || got[1].Type != "task_completed" {
		t.Fatalf("Events() = %#v, want [task_started, task_completed] snapshot", got)
	}
}

func TestBridgePublishSurvivesClosedPlatform(t *testing.T) {
	platform := observability.NewEventBus(8)
	b := New(platform, nil)
	if err := platform.Close(); err != nil {
		t.Fatalf("platform.Close() error = %v, want nil", err)
	}
	// Platform bus is closed: tee fails internally (logged Warn), but the
	// authoritative append half must still succeed and Publish must return nil —
	// SSE is a best-effort notification layer, never a task-flow blocker.
	if err := b.Publish(context.Background(), domain.RuntimeEvent{Type: "task_completed", TaskID: "t1"}); err != nil {
		t.Fatalf("Publish() after platform close error = %v, want nil", err)
	}
	if got := b.Events(); len(got) != 1 {
		t.Fatalf("Events() len = %d, want 1 (append half unaffected by tee failure)", len(got))
	}
}
