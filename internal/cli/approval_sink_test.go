package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/observability"
)

func TestPlatformApprovalSinkPublishesPending(t *testing.T) {
	bus := observability.NewEventBus(8)
	sink := newPlatformApprovalSink(bus, nil)
	events, cancel := bus.Subscribe(context.Background())
	defer cancel()

	sink.ApprovalPending(context.Background(), "task-1", "ticket-1", "write_file", map[string]string{"path": "/tmp/x"})

	select {
	case env := <-events:
		if env.Type != "approval_pending" {
			t.Fatalf("Type = %q, want approval_pending", env.Type)
		}
		if env.SubjectID != "task-1" || env.Data["ticket_id"] != "ticket-1" || env.Data["tool"] != "write_file" {
			t.Fatalf("envelope = %#v, want task-1/ticket-1/write_file", env)
		}
		args, ok := env.Data["arguments"].(map[string]string)
		if !ok || args["path"] != "/tmp/x" {
			t.Fatalf("Data[arguments] = %#v, want map with path", env.Data["arguments"])
		}
	case <-time.After(time.Second):
		t.Fatal("no approval_pending envelope received")
	}
}

func TestPlatformApprovalSinkPublishesResolved(t *testing.T) {
	bus := observability.NewEventBus(8)
	sink := newPlatformApprovalSink(bus, nil)
	events, cancel := bus.Subscribe(context.Background())
	defer cancel()

	sink.ApprovalResolved(context.Background(), "task-1", "ticket-1", "denied")

	select {
	case env := <-events:
		if env.Type != "approval_resolved" || env.Data["ticket_id"] != "ticket-1" || env.Data["decision"] != "denied" {
			t.Fatalf("envelope = %#v, want approval_resolved/ticket-1/denied", env)
		}
	case <-time.After(time.Second):
		t.Fatal("no approval_resolved envelope received")
	}
}

func TestPlatformApprovalSinkSwallowsPublishError(t *testing.T) {
	bus := observability.NewEventBus(8)
	if err := bus.Close(); err != nil {
		t.Fatalf("bus.Close() error = %v, want nil", err)
	}
	sink := newPlatformApprovalSink(bus, nil)
	// Closed bus: Publish errors internally; sink must not panic and (being
	// error-less) simply logs Warn — approval flow is never blocked.
	sink.ApprovalPending(context.Background(), "task-1", "ticket-1", "write_file", nil)
	sink.ApprovalResolved(context.Background(), "task-1", "ticket-1", "approved")
}
