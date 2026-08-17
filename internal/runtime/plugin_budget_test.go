package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// opCallHostCallTool is the host-call fixture guest's test-only op that hands its
// request body straight to the call_tool host function and returns the host's
// answer verbatim (internal/plugin/host/testdata/README.md's op table).
//
// Production's contributed-tool handler enters a guest at abi.OpCallTool instead;
// this fixture answers that op with "unsupported op", so the plugin-backed tool
// below drives op 76. Everything the contract under test depends on is the real
// thing: a real wazero runtime, the real `legion` host module, the real call_tool
// host function, and the real tool registry it dispatches back into.
const opCallHostCallTool int32 = 76

// hostcallFixture is the compiled host-call guest, borrowed from the plugin host
// package's testdata. It is read across package directories on purpose: the point
// of this test is that TWO packages agree on one counter, so proving it needs the
// runtime's real tool loop on one side and a real plugin guest on the other.
func hostcallFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "plugin", "host", "testdata", "hostcall.wasm")
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host-call fixture %s: %v", path, err)
	}
	return wasmBytes
}

// pluginKV is the plugin-private store the kv capability needs. The fixture guest
// imports every host function, so a full grant — and therefore this — is required
// to instantiate it at all.
type pluginKV struct {
	mu   sync.Mutex
	data map[string]string
}

func newPluginKV() *pluginKV { return &pluginKV{data: map[string]string{}} }

func (s *pluginKV) Get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, found := s.data[key]
	return value, found, nil
}

func (s *pluginKV) Put(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

// pluginThenDirectMaas asks for the plugin-backed tool for the first pluginRounds
// rounds — each of those makes the plugin call probe_tool through call_tool — and
// then asks for probe_tool itself, round after round, until something stops it.
//
// Every round carries different arguments, so neither the name+arguments repeat
// guard nor the consecutive-streak guard can trip: the only thing that can end
// this run is the per-tool-NAME loop cap, which is exactly the counter under test.
type pluginThenDirectMaas struct {
	pluginRounds int
	calls        int
}

func (m *pluginThenDirectMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.calls++
	name := "probe_tool"
	if m.calls <= m.pluginRounds {
		name = "plugin_probe"
	}
	return port.InferenceResponse{
		Text: "working",
		ToolCalls: []domain.ToolCall{{
			ID:        fmt.Sprintf("c%d", m.calls),
			Name:      name,
			Arguments: map[string]string{"n": fmt.Sprint(m.calls)},
		}},
	}, nil
}

// TestPluginCallToolSpendsTheModelsSharedToolBudget is contract 1's central
// claim, proved through the runtime's real wiring rather than against the seam in
// isolation: a tool call a plugin makes through call_tool consumes the SAME
// per-task allowance the model's own calls consume.
//
// The arithmetic is what makes it a proof. The plugin drives probe_tool
// pluginRounds times; the model then asks for probe_tool every round until the
// loop cap cuts the loop. If the counter is shared, the model gets
// toolLoopCap-pluginRounds of its own calls and no more — its remaining allowance
// was reduced by exactly what the plugin spent. If call_tool counted anywhere
// else, the model would get a full toolLoopCap.
func TestPluginCallToolSpendsTheModelsSharedToolBudget(t *testing.T) {
	const (
		pluginName   = "budget-fixture"
		pluginRounds = 5
	)
	ctx := context.Background()

	var (
		mu      sync.Mutex
		origins []string
	)
	audit := adapter.NewMemoryAuditLog()
	events := adapter.NewMemoryEventBus()
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{}).
		WithAuditLog(audit)
	registry.RegisterDescriptor(
		tool.Descriptor{Name: "probe_tool", Group: "test"},
		tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			mu.Lock()
			origins = append(origins, tool.CallOriginFrom(ctx))
			mu.Unlock()
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
		}),
	)

	// The plugin side: a real wazero runtime, the real host module, a real guest.
	wasmRuntime := host.NewRuntime(ctx, 64)
	t.Cleanup(func() { _ = wasmRuntime.Close(context.Background()) })
	grant := perm.Grant{Log: true, Config: true, KV: true, HTTP: true, FS: true, Tool: true}
	if _, err := host.BuildHostModule(ctx, wasmRuntime, grant, host.Deps{
		PluginName: pluginName,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:     json.RawMessage(`{}`),
		KV:         newPluginKV(),
		HTTP:       http.DefaultClient,
		FS:         port.NewWorkspacePathGuard(t.TempDir()),
		Events:     events,
		Tools:      registry,
		Agent:      domain.Agent{ID: "a", Role: "developer", Status: domain.AgentActive},
	}); err != nil {
		t.Fatalf("BuildHostModule: %v", err)
	}
	compiled, err := host.Compile(ctx, wasmRuntime, hostcallFixture(t))
	if err != nil {
		t.Fatalf("Compile(host-call fixture): %v", err)
	}
	instance, err := host.NewInstance(ctx, wasmRuntime, compiled)
	if err != nil {
		t.Fatalf("NewInstance(host-call fixture): %v", err)
	}
	t.Cleanup(func() { _ = instance.Close(context.Background()) })

	// The plugin-backed tool: it marks the plugin's call origin and enters the
	// guest, which calls the call_tool host function — the same two steps
	// internal/plugin/host's contributed-tool handler takes.
	request, err := json.Marshal(map[string]any{"tool": "probe_tool", "arguments": map[string]string{"via": "plugin"}})
	if err != nil {
		t.Fatalf("marshal call_tool request: %v", err)
	}
	registry.RegisterDescriptor(
		tool.Descriptor{Name: "plugin_probe", Group: "test"},
		tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			guestCtx := tool.WithCallOrigin(ctx, "plugin:"+pluginName)
			out, err := instance.Invoke(guestCtx, opCallHostCallTool, request)
			if err != nil {
				return domain.ToolResult{}, fmt.Errorf("invoke plugin guest: %w", err)
			}
			var result domain.ToolResult
			if err := json.Unmarshal(out, &result); err != nil {
				return domain.ToolResult{}, fmt.Errorf("decode call_tool answer %s: %w", out, err)
			}
			if !result.Success {
				return domain.ToolResult{}, fmt.Errorf("the plugin's call_tool was refused: %s", out)
			}
			return domain.ToolResult{CallID: call.ID, Success: true, Output: result.Output}, nil
		}),
	)

	maas := &pluginThenDirectMaas{pluginRounds: pluginRounds}
	runner := NewRuntime(Config{Maas: maas, Tools: registry, Events: events, MaxToolRounds: 1000})
	if _, err := runner.RunTask(ctx,
		domain.Agent{ID: "a", CompanyID: "co", Role: "developer", Status: domain.AgentActive},
		domain.Task{ID: "t1", CompanyID: "co", AgentID: "a", Status: domain.TaskRunning, Input: "go"},
	); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	mu.Lock()
	seen := append([]string(nil), origins...)
	mu.Unlock()

	byOrigin := map[string]int{}
	for _, origin := range seen {
		byOrigin[origin]++
	}
	if got := byOrigin["plugin:"+pluginName]; got != pluginRounds {
		t.Errorf("the plugin drove probe_tool %d times, want %d (origins: %v)", got, pluginRounds, byOrigin)
	}
	wantModelCalls := toolLoopCap - pluginRounds
	if got := byOrigin[tool.OriginAgent]; got != wantModelCalls {
		t.Errorf("the model got %d probe_tool calls, want %d: the plugin's %d must come out of the SAME "+
			"per-task allowance (cap %d, origins: %v)",
			got, wantModelCalls, pluginRounds, toolLoopCap, byOrigin)
	}
	if len(seen) != toolLoopCap {
		t.Errorf("probe_tool ran %d times in total, want %d (the whole shared allowance)", len(seen), toolLoopCap)
	}

	published, err := events.Events()
	if err != nil {
		t.Fatalf("read published events: %v", err)
	}
	broken := 0
	for _, ev := range published {
		if ev.Type == "tool_loop_broken" && strings.Contains(ev.Message, "probe_tool") {
			broken++
		}
	}
	if broken != 1 {
		t.Errorf("published %d tool_loop_broken events naming probe_tool, want 1", broken)
	}

	// The audit trail must be able to tell the two writers apart, or a forensic
	// pass over a time window cannot say whose calls exhausted the budget.
	auditEvents, err := audit.Events()
	if err != nil {
		t.Fatalf("read audit events: %v", err)
	}
	auditByOrigin := map[string]int{}
	for _, ev := range auditEvents {
		if ev.SubjectID != "probe_tool" || ev.Action != "tool_executed" {
			continue
		}
		auditByOrigin[ev.Origin]++
	}
	if got := auditByOrigin["plugin:"+pluginName]; got != pluginRounds {
		t.Errorf("audit shows %d probe_tool calls with origin plugin:%s, want %d (all: %v)",
			got, pluginName, pluginRounds, auditByOrigin)
	}
	if got := auditByOrigin[tool.OriginAgent]; got != wantModelCalls {
		t.Errorf("audit shows %d probe_tool calls with origin %q, want %d (all: %v)",
			got, tool.OriginAgent, wantModelCalls, auditByOrigin)
	}
}

// TestRuntimeInstallsTheSharedToolBudgetOnEveryDispatch is the narrower half of
// the contract: whatever a dispatched tool call reaches, it runs under the task's
// budget. A tool that finds none would have nothing to charge, which is the state
// call_tool refuses on — so an absent budget must be impossible here, not merely
// unlikely.
func TestRuntimeInstallsTheSharedToolBudgetOnEveryDispatch(t *testing.T) {
	var (
		found  bool
		limit  int
		counts []int
	)
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, tool.NoopGuardrails{})
	registry.RegisterDescriptor(
		tool.Descriptor{Name: "probe_tool", Group: "test"},
		tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			budget, ok := tool.LoopBudgetFrom(ctx)
			found = ok
			if ok {
				// Charging the model's own tool name is what a plugin's call_tool
				// does; the count it gets back must continue the loop's own count.
				count, inForce := budget.Record("probe_tool")
				counts = append(counts, count)
				limit = inForce
			}
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
		}),
	)
	maas := &pluginThenDirectMaas{pluginRounds: 0}
	runner := NewRuntime(Config{Maas: maas, Tools: registry, MaxToolRounds: 3})
	if _, err := runner.RunTask(context.Background(),
		domain.Agent{ID: "a", Role: "developer", Status: domain.AgentActive},
		domain.Task{ID: "t1", Status: domain.TaskRunning, Input: "go"},
	); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !found {
		t.Fatal("a dispatched tool call ran with no shared budget on its context")
	}
	if limit != toolLoopCap {
		t.Errorf("the installed budget reports limit %d, want toolLoopCap (%d)", limit, toolLoopCap)
	}
	// Round 1: the loop records 1, the handler's charge makes it 2. Round 2: the
	// loop records 3, the handler 4. Same counter, no reset, no second map.
	want := []int{2, 4, 6}
	if len(counts) != len(want) {
		t.Fatalf("handler charged the budget %d times (%v), want %d", len(counts), counts, len(want))
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatalf("budget counts = %v, want %v: the handler's charge must continue the loop's own count",
				counts, want)
		}
	}
}
