package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
)

func TestRegisterOwnedRevokesThroughLedger(t *testing.T) {
	ledger := lifecycle.NewLedger()
	r := NewRegistry(nil, nil, nil)
	RegisterOwned(ledger, "plugin:demo", r, Descriptor{Name: "demo_tool"}, okHandler())

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "demo_tool"}); err != nil {
		t.Fatalf("Execute before dispose: %v", err)
	}
	if labels := ledger.Snapshot()["plugin:demo"]; len(labels) != 1 || labels[0] != "tool:demo_tool" {
		t.Fatalf("want one ledger entry labelled tool:demo_tool, got %v", labels)
	}

	if err := ledger.DisposeOwner("plugin:demo"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	if _, err := r.Execute(context.Background(), domain.Agent{}, domain.ToolCall{Name: "demo_tool"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound after owner disposal, got %v", err)
	}
	if len(ledger.Snapshot()) != 0 {
		t.Fatalf("ledger must be empty after disposal, got %v", ledger.Snapshot())
	}
}

func TestRegisterOwnedHandleIsIdempotent(t *testing.T) {
	ledger := lifecycle.NewLedger()
	r := NewRegistry(nil, nil, nil)
	revoke := RegisterOwned(ledger, "plugin:demo", r, Descriptor{Name: "demo_tool"}, okHandler())

	if err := revoke(); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := revoke(); err != nil {
		t.Fatalf("second revoke must be a no-op, got %v", err)
	}
	if err := ledger.DisposeOwner("plugin:demo"); err != nil {
		t.Fatalf("DisposeOwner after manual revoke: %v", err)
	}
}
