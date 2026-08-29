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

// The decide seam is the first place a plugin's answer CHANGES what the host
// does. These tests pin the two properties that make that safe: it can only
// refuse, and every way of failing to answer is itself a refusal.

func decideDeps(bus *adapter.MemoryEventBus, onFault func(context.Context, string, string, string)) Deps {
	return Deps{
		PluginName: "legion-gatekeeper",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events:     bus,
		OnFault:    onFault,
	}
}

func TestPluginDeciderSendsTheCallAndReadsTheVerdict(t *testing.T) {
	var got guestToolDecisionRequest
	guest := guestCallerFunc(func(_ context.Context, op int32, in []byte) ([]byte, error) {
		if op != abi.OpDecideToolCall {
			t.Errorf("guest received op %d, want %d", op, abi.OpDecideToolCall)
		}
		if err := json.Unmarshal(in, &got); err != nil {
			t.Fatalf("decode decision request %q: %v", in, err)
		}
		return []byte(`{"decision":"deny","reason":"writes are frozen"}`), nil
	})
	decider := newPluginDecider(decideDeps(adapter.NewMemoryEventBus(), nil), guest)

	verdict := decider.Decide(context.Background(),
		domain.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]string{"path": "/tmp/x"}})

	if got.CallID != "c1" || got.Tool != "write_file" || got.Arguments["path"] != "/tmp/x" {
		t.Errorf("request = %+v, want the call it was asked about", got)
	}
	if verdict.Decision != tool.DecisionDeny || verdict.Reason != "writes are frozen" {
		t.Errorf("verdict = %+v, want a deny carrying the guest's reason", verdict)
	}
}

func TestPluginDeciderPassesAnAllowThrough(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return []byte(`{"decision":"allow","reason":""}`), nil
	})
	faults := 0
	decider := newPluginDecider(decideDeps(adapter.NewMemoryEventBus(),
		func(context.Context, string, string, string) { faults++ }), guest)

	verdict := decider.Decide(context.Background(), domain.ToolCall{ID: "c1", Name: "read_file"})

	if verdict.Decision != tool.DecisionAllow {
		t.Errorf("verdict = %+v, want allow", verdict)
	}
	if faults != 0 {
		t.Errorf("a plugin that answered normally produced %d faults, want 0", faults)
	}
}

// TestPluginDeciderFailsClosed is the decision this seam was designed around:
// every way of not answering is a REFUSAL. fail-open would make the control
// something an attacker switches off by making the plugin hang or crash.
func TestPluginDeciderFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     []byte
		callErr  error
		wantCat  string
		wantWord string
	}{
		{name: "trap", callErr: errors.New("guest trapped"), wantCat: CategoryTrap, wantWord: "trapped"},
		{name: "empty body", body: nil, wantCat: CategoryABI, wantWord: "no body"},
		{name: "unreadable body", body: []byte(`not json`), wantCat: CategoryABI, wantWord: "decode"},
		{name: "unknown decision", body: []byte(`{"decision":"maybe","reason":""}`),
			wantCat: CategoryABI, wantWord: "maybe"},
		{name: "unknown field", body: []byte(`{"decision":"allow","override":true}`),
			wantCat: CategoryABI, wantWord: "override"},
		{name: "trailing data", body: []byte(`{"decision":"allow"}{"decision":"allow"}`),
			wantCat: CategoryABI, wantWord: "trailing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
				return tc.body, tc.callErr
			})
			bus := adapter.NewMemoryEventBus()
			var categories []string
			decider := newPluginDecider(decideDeps(bus, func(_ context.Context, category, _, _ string) {
				categories = append(categories, category)
			}), guest)

			verdict := decider.Decide(context.Background(), domain.ToolCall{ID: "c1", Name: "write_file"})

			if verdict.Decision != tool.DecisionDeny {
				t.Errorf("verdict = %+v, want deny: a decision point that cannot answer must refuse", verdict)
			}
			if !strings.Contains(verdict.Reason, tc.wantWord) {
				t.Errorf("reason = %q, want it to contain %q so an operator can tell WHY", verdict.Reason, tc.wantWord)
			}
			if len(categories) != 1 || categories[0] != tc.wantCat {
				t.Errorf("fault categories = %v, want [%s]", categories, tc.wantCat)
			}
			events, err := bus.Events()
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if len(events) != 1 || !strings.Contains(events[0].Message, "decide:write_file") {
				t.Errorf("events = %+v, want one naming the decision that failed", events)
			}
		})
	}
}

// TestPluginDeciderDoesNotCountACallerCancellationAsAFault: the caller walked
// away, which says nothing about the plugin. The call is still refused —
// there is no call left to allow — but the plugin's health is untouched.
func TestPluginDeciderDoesNotCountACallerCancellationAsAFault(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return nil, context.Canceled
	})
	faults := 0
	decider := newPluginDecider(decideDeps(adapter.NewMemoryEventBus(),
		func(context.Context, string, string, string) { faults++ }), guest)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verdict := decider.Decide(ctx, domain.ToolCall{ID: "c1", Name: "write_file"})

	if verdict.Decision != tool.DecisionDeny {
		t.Errorf("verdict = %+v, want deny", verdict)
	}
	if faults != 0 {
		t.Errorf("a cancelled consultation produced %d faults, want 0", faults)
	}
}

// TestPluginDeciderReportsASuccessfulAnswerToTheHealthCounter: answering —
// with either verdict — is the plugin working, and it must reset the
// consecutive-fault count exactly as a tool call does.
func TestPluginDeciderReportsASuccessfulAnswerToTheHealthCounter(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return []byte(`{"decision":"deny","reason":"no"}`), nil
	})
	deps := decideDeps(adapter.NewMemoryEventBus(), nil)
	successes := 0
	deps.OnSuccess = func(context.Context, string) { successes++ }
	decider := newPluginDecider(deps, guest)

	decider.Decide(context.Background(), domain.ToolCall{ID: "c1", Name: "write_file"})

	if successes != 1 {
		t.Errorf("OnSuccess called %d times, want 1: a plugin that refuses is a plugin that answered", successes)
	}
}

// TestContributeToolsRegistersTheDeciderOnlyWhenGranted: an ungranted
// extension is an absent registration, not a runtime check — and for this
// seam that is the difference between a plugin that can refuse the agent's
// tool calls and one that cannot.
func TestContributeToolsRegistersTheDeciderOnlyWhenGranted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extensions perm.Extensions
		wantDenied bool
	}{
		{name: "granted", extensions: perm.Extensions{Decide: true}, wantDenied: true},
		{name: "not granted", extensions: perm.Extensions{}, wantDenied: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := lifecycle.NewLedger()
			owner := lifecycle.Owner("plugin:legion-gatekeeper")
			registry := observingRegistry()

			var mu sync.Mutex
			guest := guestCallerFunc(func(_ context.Context, op int32, _ []byte) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				if op == abi.OpDecideToolCall {
					return []byte(`{"decision":"deny","reason":"never"}`), nil
				}
				return []byte(`{"call_id":"","success":true,"output":"ok","error":""}`), nil
			})
			spec := observedToolSpec(registry, tc.extensions, decideDeps(adapter.NewMemoryEventBus(), nil))
			spec.Name = "legion-gatekeeper"
			contributeTools(ledger, owner, spec, guest, func(func() error) {})
			t.Cleanup(func() {
				if err := ledger.DisposeOwner(owner); err != nil {
					t.Errorf("dispose owner: %v", err)
				}
			})

			_, err := registry.Execute(context.Background(),
				domain.Agent{ID: "a", Role: "developer"},
				domain.ToolCall{ID: "c1", Name: "observed_tool"})
			denied := errors.Is(err, tool.ErrPermissionDenied)
			if denied != tc.wantDenied {
				t.Fatalf("Execute error = %v; denied = %t, want %t", err, denied, tc.wantDenied)
			}
		})
	}
}
