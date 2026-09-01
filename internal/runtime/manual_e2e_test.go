package runtime

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/manualgate"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
)

// TestManualGateDenyThenResume drives a full Manual-mode round trip where the
// human denies the pending sensitive tool call: suspend on round 1, then
// resume completes without ever executing write_file.
func TestManualGateDenyThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	maas := &oneToolThenTextMaas{toolName: "write_file", toolArgs: map[string]string{"path": "out/a.txt"}}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalDenied, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if writeCalled {
		t.Fatal("denied write_file executed on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
}

// TestManualGateApproveThenResume mirrors the deny scenario but the human
// approves the ticket: resume must execute write_file and complete.
func TestManualGateApproveThenResume(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)
	var writeCalled bool
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		writeCalled = true
		return domain.ToolResult{Success: true, Output: "wrote"}, nil
	}))
	maas := &oneToolThenTextMaas{toolName: "write_file", toolArgs: map[string]string{"path": "out/a.txt"}}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}
	if _, err := apStore.Decide("s1", approval.TicketID("t1", "c1"), approval.ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v", err)
	}
	if !writeCalled {
		t.Fatal("approved write_file did not execute on resume")
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}
}

// twoDuplicateSensitiveCallsThenTextMaas issues two write_file calls in a
// single round, both carrying the SAME provider id -- the shape
// adapter.openAIToolCalls produces when the provider omits ids and both calls
// target the same function name (see disambiguateCallIDs's doc comment).
// Round 2 answers in text.
type twoDuplicateSensitiveCallsThenTextMaas struct{ calls int }

func (m *twoDuplicateSensitiveCallsThenTextMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.calls++
	if m.calls == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{
			{ID: "write_file", Name: "write_file", Arguments: map[string]string{"path": "a.txt"}},
			{ID: "write_file", Name: "write_file", Arguments: map[string]string{"path": "b.txt"}},
		}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// TestManualGateApprovesBothOfTwoDuplicateIDSensitiveCalls pins the N-I1 fix
// (task-4-review-2.md §3): when a round asks for the same sensitive tool
// twice in parallel and the provider hands back the same degraded id for
// both (see disambiguateCallIDs), the human must be able to approve BOTH
// resulting tickets and see both writes execute on resume.
//
// Before the fix, ShouldSuspend opened its approval tickets under the
// PRE-disambiguation id (both calls collided on "write_file") while dispatch
// resolved against the POST-disambiguation id ("write_file", "write_file#2"),
// so the second call's ticket could never be found: dispatch failed loud with
// "undecided sensitive call" even after a human approved everything they were
// shown. Moving disambiguateCallIDs to where st.resp is produced (generateStep)
// means ShouldSuspend already sees the settled ids, so both tickets open under
// ids dispatch can actually resolve.
func TestManualGateApprovesBothOfTwoDuplicateIDSensitiveCalls(t *testing.T) {
	dir := t.TempDir()
	cpStore := sessionstate.NewStore(dir)
	apStore := approval.NewToolGateStore(dir)
	gate := manualgate.New(apStore)

	var mu sync.Mutex
	var written []string
	reg := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }), tool.NoopGuardrails{})
	reg.RegisterDescriptor(tool.Descriptor{Name: "write_file", Sensitive: true}, tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		mu.Lock()
		written = append(written, call.Arguments["path"])
		mu.Unlock()
		return domain.ToolResult{Success: true, Output: "wrote " + call.Arguments["path"]}, nil
	}))

	maas := &twoDuplicateSensitiveCallsThenTextMaas{}
	r := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Audit: adapter.NewMemoryAuditLog(), Events: adapter.NewMemoryEventBus(),
		Tools: reg, Checkpoints: cpStore, ToolGate: gate})
	task := domain.Task{ID: "t1", SessionID: "s1", AgentID: "a1", Status: domain.TaskRunning, Mode: domain.ModeManual, Input: "go"}
	if _, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task); err != ErrSuspended {
		t.Fatalf("first run err = %v, want ErrSuspended", err)
	}

	// Both tickets must exist under the DISAMBIGUATED ids -- "write_file" and
	// "write_file#2" per disambiguateCallIDs's suffixing rule -- for the human
	// to have anything distinct to approve in the first place.
	for _, id := range []string{"write_file", "write_file#2"} {
		if _, err := apStore.Decide("s1", approval.TicketID("t1", id), approval.ApprovalApproved, ""); err != nil {
			t.Fatalf("approve ticket %q: %v", id, err)
		}
	}

	run, err := r.RunTask(context.Background(), domain.Agent{ID: "a1"}, task)
	if err != nil {
		t.Fatalf("resume run err = %v, want nil", err)
	}
	if run.Result == "" {
		t.Fatal("resume produced no final answer")
	}

	mu.Lock()
	got := append([]string(nil), written...)
	mu.Unlock()
	sort.Strings(got)
	want := []string{"a.txt", "b.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("write_file executed for paths %v, want both %v: the second call's approval ticket was never resolved", got, want)
	}
}
