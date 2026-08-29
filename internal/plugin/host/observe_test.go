package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
)

// The observe extension is the first seam where the HOST calls the GUEST for
// something other than running the guest's own tool. These tests pin the three
// properties that make that safe: it is only wired when granted, its answer is
// discarded, and its failures land on the plugin's own health rather than on
// the caller's result.

func observeDeps(bus *adapter.MemoryEventBus, onFault func(context.Context, string, string, string)) Deps {
	return Deps{
		PluginName: "legion-observer",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events:     bus,
		OnFault:    onFault,
	}
}

func observedToolSpec(registry *tool.Registry, extensions perm.Extensions, deps Deps) Spec {
	return Spec{
		Name:       "legion-observer",
		Registry:   registry,
		Extensions: extensions,
		Tools: []tool.Descriptor{{
			Name: "observed_tool", Description: "d", Group: "g", RiskLevel: "low",
		}},
		Deps: deps,
	}
}

func observingRegistry() *tool.Registry {
	return tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow),
		tool.NewRolePermissionEnforcer(map[string]bool{"developer:observed_tool": true}),
		tool.NoopGuardrails{},
	)
}

func TestPluginObserverSendsTheCallAndItsResult(t *testing.T) {
	var got guestToolObservation
	guest := guestCallerFunc(func(_ context.Context, op int32, in []byte) ([]byte, error) {
		if op != abi.OpObserveToolResult {
			t.Errorf("guest received op %d, want %d", op, abi.OpObserveToolResult)
		}
		if err := json.Unmarshal(in, &got); err != nil {
			t.Fatalf("decode observation %q: %v", in, err)
		}
		return []byte(`{"error":"unsupported op"}`), nil
	})
	observer := newPluginObserver(observeDeps(adapter.NewMemoryEventBus(), nil), guest)

	observer.Observe(context.Background(),
		domain.ToolCall{ID: "c1", Name: "demo_tool", Arguments: map[string]string{"k": "v"}},
		domain.ToolResult{Success: true, Output: "done"})

	if got.CallID != "c1" || got.Tool != "demo_tool" || got.Arguments["k"] != "v" {
		t.Errorf("observation = %+v, want the call it was told about", got)
	}
	if !got.Success || got.Output != "done" {
		t.Errorf("observation = %+v, want the result it was told about", got)
	}
}

// TestPluginObserverDiscardsWhateverTheGuestAnswers: a guest that returns a
// "corrected" result must change nothing, and answering normally — whatever
// the body says — is not a fault.
func TestPluginObserverDiscardsWhateverTheGuestAnswers(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return []byte(`{"call_id":"c1","success":false,"output":"tampered","error":"denied"}`), nil
	})
	faults := 0
	observer := newPluginObserver(observeDeps(adapter.NewMemoryEventBus(),
		func(context.Context, string, string, string) { faults++ }), guest)

	// The signature itself is the proof: Observe returns nothing at all.
	observer.Observe(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_tool"},
		domain.ToolResult{Success: true, Output: "the real output"})

	if faults != 0 {
		t.Errorf("a guest that answered normally produced %d faults, want 0", faults)
	}
}

// TestPluginObserverCountsAFailedNotificationAsAFault: an observer that keeps
// trapping is a plugin that keeps failing, and G1's counter is what eventually
// unloads it. The caller's result is untouched either way.
func TestPluginObserverCountsAFailedNotificationAsAFault(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return nil, errors.New("guest trapped")
	})
	bus := adapter.NewMemoryEventBus()
	var categories []string
	observer := newPluginObserver(observeDeps(bus, func(_ context.Context, category, _, _ string) {
		categories = append(categories, category)
	}), guest)

	observer.Observe(context.Background(), domain.ToolCall{ID: "c1", Name: "demo_tool"},
		domain.ToolResult{Success: true})

	if len(categories) != 1 || categories[0] != CategoryTrap {
		t.Errorf("fault categories = %v, want [%s]", categories, CategoryTrap)
	}
	events, err := bus.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Message, "observe:demo_tool") {
		t.Errorf("events = %+v, want one naming the observed tool", events)
	}
}

// TestPluginObserverDoesNotCountACallerCancellation: the caller walked away,
// which says nothing about the plugin.
func TestPluginObserverDoesNotCountACallerCancellation(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return nil, context.Canceled
	})
	faults := 0
	observer := newPluginObserver(observeDeps(adapter.NewMemoryEventBus(),
		func(context.Context, string, string, string) { faults++ }), guest)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observer.Observe(ctx, domain.ToolCall{ID: "c1", Name: "demo_tool"}, domain.ToolResult{Success: true})

	if faults != 0 {
		t.Errorf("a cancelled notification produced %d faults, want 0", faults)
	}
}

// TestContributeToolsRegistersTheObserverOnlyWhenGranted is the enforcement:
// an ungranted extension is not a runtime check, it is an absent registration.
func TestContributeToolsRegistersTheObserverOnlyWhenGranted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extensions perm.Extensions
		wantSeam   bool
	}{
		{name: "granted", extensions: perm.Extensions{Observe: true}, wantSeam: true},
		{name: "not granted", extensions: perm.Extensions{}, wantSeam: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := lifecycle.NewLedger()
			owner := lifecycle.Owner("plugin:legion-observer")
			registry := observingRegistry()

			var mu sync.Mutex
			notified := 0
			guest := guestCallerFunc(func(_ context.Context, op int32, _ []byte) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				if op == abi.OpObserveToolResult {
					notified++
				}
				return []byte(`{"call_id":"","success":true,"output":"ok","error":""}`), nil
			})
			contributeTools(ledger, owner,
				observedToolSpec(registry, tc.extensions, observeDeps(adapter.NewMemoryEventBus(), nil)),
				guest, func(func() error) {})
			// The gateable catalog is process-wide: leaving this subtest's
			// contribution filed would make the next one panic on a duplicate.
			t.Cleanup(func() {
				if err := ledger.DisposeOwner(owner); err != nil {
					t.Errorf("dispose owner: %v", err)
				}
			})

			hasObserver := false
			for _, label := range ledger.Snapshot()[owner] {
				if strings.HasPrefix(label, "observer:") {
					hasObserver = true
				}
			}
			if hasObserver != tc.wantSeam {
				t.Fatalf("ledger holds %v; observer registered = %t, want %t",
					ledger.Snapshot()[owner], hasObserver, tc.wantSeam)
			}

			if _, err := registry.Execute(context.Background(),
				domain.Agent{ID: "a", Role: "developer"},
				domain.ToolCall{ID: "c1", Name: "observed_tool"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.wantSeam && notified != 1 {
				t.Errorf("guest notified %d times, want 1", notified)
			}
			if !tc.wantSeam && notified != 0 {
				t.Errorf("guest notified %d times without the grant, want 0", notified)
			}
		})
	}
}

// TestDisposingTheOwnerTakesThePluginOffTheObserveSeam: withdrawing a plugin's
// contributions must withdraw its watching too — a plugin whose tools are gone
// but which still sees every call is a plugin the deployment believes it has
// disabled.
func TestDisposingTheOwnerTakesThePluginOffTheObserveSeam(t *testing.T) {
	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:legion-observer")
	registry := observingRegistry()

	notified := 0
	guest := guestCallerFunc(func(_ context.Context, op int32, _ []byte) ([]byte, error) {
		if op == abi.OpObserveToolResult {
			notified++
		}
		return []byte(`{"call_id":"","success":true,"output":"ok","error":""}`), nil
	})
	contributeTools(ledger, owner,
		observedToolSpec(registry, perm.Extensions{Observe: true}, observeDeps(adapter.NewMemoryEventBus(), nil)),
		guest, func(func() error) {})

	agent := domain.Agent{ID: "a", Role: "developer"}
	call := domain.ToolCall{ID: "c1", Name: "observed_tool"}
	if _, err := registry.Execute(context.Background(), agent, call); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := ledger.DisposeOwner(owner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	// The plugin's tool went with the owner, so put a host-side tool of the
	// same name back to drive one more call through the seam.
	registry.Register("observed_tool", tool.HandlerFunc(
		func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{Success: true}, nil
		}))
	if _, err := registry.Execute(context.Background(), agent, call); err != nil {
		t.Fatalf("Execute after disposal: %v", err)
	}
	if notified != 1 {
		t.Errorf("guest notified %d times, want 1: disposing the owner must take the plugin off the seam", notified)
	}
}
