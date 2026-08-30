package host

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// fakeBudget is a tool.LoopBudget standing in for the runtime's per-task
// counter: it records every name it is handed, so a test can prove call_tool
// spent the SHARED budget (and which name it spent it on) rather than a counter
// of its own, and it reports a limit the test chooses so the boundary can be
// driven without 30 round trips through wasm.
type fakeBudget struct {
	mu       sync.Mutex
	recorded []string
	counts   map[string]int
	limit    int
}

func newFakeBudget(limit int) *fakeBudget {
	return &fakeBudget{counts: map[string]int{}, limit: limit}
}

func (b *fakeBudget) Record(name string) (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorded = append(b.recorded, name)
	b.counts[name]++
	return b.counts[name], b.limit
}

func (b *fakeBudget) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.recorded...)
}

// budgetedCtx returns a context carrying budget, which is what the runtime's
// tool loop installs before it dispatches a call.
func budgetedCtx(budget tool.LoopBudget) context.Context {
	return tool.WithLoopBudget(context.Background(), budget)
}

// callToolBody marshals a call_tool request the way the fixture guest's op 76
// passes it through verbatim.
func callToolBody(t *testing.T, req callToolRequest) []byte {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal call_tool request %+v: %v", req, err)
	}
	return body
}

// newHostcallStack builds a host module from g and deps on a fresh runtime,
// compiles the host-call fixture guest against it, and returns a spawn function
// that instantiates one MORE guest each time it is called.
//
// A chain of nested call_tool calls needs one instance per level: an Instance is
// documented as unsafe for concurrent use and is not re-entered here while one of
// its own calls is still on the stack.
//
// The returned factory reports its failure as an error rather than calling
// t.Fatalf: TestCallToolDeniesAChainDeeperThanTheDepthCap invokes it from inside
// the guest's own call stack (a tool handler running underneath a wazero host
// frame), and t.Fatalf's runtime.Goexit can unwind through those frames the same
// way it would have on the overrun path this test was rewritten to avoid — see
// that test's own comment. Returning the error lets the caller propagate it the
// way an ordinary tool failure is propagated instead.
func newHostcallStack(t *testing.T, g perm.Grant, deps Deps) func() (*Instance, error) {
	t.Helper()

	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	if _, err := BuildHostModule(ctx, rt, g, deps); err != nil {
		t.Fatalf("BuildHostModule: %v", err)
	}
	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture): %v", err)
	}
	return func() (*Instance, error) {
		inst, err := NewInstance(ctx, rt, compiled)
		if err != nil {
			return nil, fmt.Errorf("NewInstance(hostcall fixture): %w", err)
		}
		t.Cleanup(func() { _ = inst.Close(context.Background()) })
		return inst, nil
	}
}

// A plugin-initiated call must spend the task's SHARED per-tool budget, keyed by
// the same name the agent's own loop counts (domain.GuardedToolName). Counting it
// anywhere else — or not at all — is a channel around the task's total budget.
func TestCallToolSpendsTheSharedBudgetOnTheGuardedName(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	budget := newFakeBudget(30)

	out, err := inst.Invoke(budgetedCtx(budget), opCallCallTool, callToolBody(t, callToolRequest{
		CallID:    "call-1",
		Tool:      "echo_tool",
		Arguments: map[string]string{"text": "hi"},
	}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode call_tool response %s: %v", out, err)
	}
	if !result.Success || result.Output != "echo:hi" {
		t.Fatalf("call_tool response = %s, want the echo tool's successful result", out)
	}
	if got := budget.names(); len(got) != 1 || got[0] != "echo_tool" {
		t.Errorf("shared budget recorded %v, want exactly [echo_tool]", got)
	}
}

// The name recorded must go through domain.GuardedToolName, the one function both
// sides of the seam use: a plugin reaching a tool through the lazy call_tool
// wrapper must land on the wrapped tool's counter, exactly as the model's loop
// does, or the two writers count different strings and neither cap holds.
func TestCallToolRecordsTheWrappedNameTheModelsGuardCounts(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	budget := newFakeBudget(30)

	// The wrapper tool itself is not registered here, so this call fails after
	// the accounting; what is under test is which name the budget was charged.
	if _, err := inst.Invoke(budgetedCtx(budget), opCallCallTool, callToolBody(t, callToolRequest{
		Tool:      "call_tool",
		Arguments: map[string]string{"tool_name": "echo_tool"},
	})); err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	if got := budget.names(); len(got) != 1 || got[0] != "echo_tool" {
		t.Errorf("shared budget recorded %v, want exactly [echo_tool] (the wrapped tool)", got)
	}
}

// Once the shared allowance for a tool is spent, the plugin is refused on the
// same counter that stops the model — and the refusal is a denial, with its
// plugin/call_failed{category=denied} event, not a host error.
func TestCallToolIsDeniedOnceTheSharedBudgetIsSpent(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	budget := newFakeBudget(1)
	ctx := budgetedCtx(budget)
	body := callToolBody(t, callToolRequest{Tool: "echo_tool", Arguments: map[string]string{"text": "hi"}})

	first, err := inst.Invoke(ctx, opCallCallTool, body)
	if err != nil {
		t.Fatalf("Invoke(call_tool) #1: %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(first, &result); err != nil {
		t.Fatalf("decode call_tool response %s: %v", first, err)
	}
	if !result.Success {
		t.Fatalf("the call at the limit was refused: %s", first)
	}

	second, err := inst.Invoke(ctx, opCallCallTool, body)
	if err != nil {
		t.Fatalf("Invoke(call_tool) #2: %v", err)
	}
	got := decodeHostError(t, second)
	if got.Code != CodeDenied {
		t.Errorf("call_tool past the shared limit returned code %q, want %q (body %s)", got.Code, CodeDenied, second)
	}
	if !strings.Contains(got.Message, "echo_tool") {
		t.Errorf("denial message %q does not name the tool", got.Message)
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
	if origins := env.recordedOrigins(); len(origins) != 1 {
		t.Errorf("the tool ran %d times, want 1: the refused call must not have executed", len(origins))
	}
}

// No budget on the context means the wiring is broken, and "unlimited" is exactly
// the bypass this contract forbids: the call must be refused, loudly, naming what
// is missing — never executed on a private fallback counter.
func TestCallToolIsDeniedWhenNoSharedBudgetIsOnTheContext(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	out, err := inst.Invoke(context.Background(), opCallCallTool, callToolBody(t, callToolRequest{
		Tool:      "echo_tool",
		Arguments: map[string]string{"text": "hi"},
	}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeDenied {
		t.Errorf("call_tool with no budget returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
	if !strings.Contains(got.Message, "budget") {
		t.Errorf("refusal message %q does not name the missing budget", got.Message)
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
	if origins := env.recordedOrigins(); len(origins) != 0 {
		t.Errorf("the tool ran %d times; a call with no budget must not execute", len(origins))
	}
}

// hardChainLevelBudget bounds the recursion test below INDEPENDENTLY of the
// feature that test exercises.
//
// It exists because the first version of that test had the depth cap as its only
// termination condition. Run against an implementation where the cap was not
// written yet, the recursion instantiated a fresh wasm module per level until the
// host ran out of address space: 170 GB of virtual memory and an unclean shutdown,
// twice. A test whose only brake is the thing it is testing has no brake.
//
// So the handler below refuses to recurse past this budget and fails the test when
// it is reached. That refusal is what stops a runaway; the depth cap only decides
// whether the test PASSES. The budget is deliberately just above
// callToolDepthCap+1 — big enough that a working cap never reaches it, small
// enough that a missing cap costs a handful of instances rather than a machine.
const hardChainLevelBudget = 8

// The second counter: one call_tool chain may go callToolDepthCap levels deep and
// no further. The chain here is real — every level is a fresh guest calling the
// real host function through wazero — because the depth has to accumulate ACROSS
// the chain rather than reset at each new host call.
func TestCallToolDeniesAChainDeeperThanTheDepthCap(t *testing.T) {
	env := newTestEnv(t)

	var (
		mu      sync.Mutex
		bodies  [][]byte
		levels  int
		overrun bool
	)
	// spawn is assigned once the stack is built, which cannot happen before this
	// handler is registered: the stack is built FROM the registry the handler
	// lives in. Nothing calls it until the first Invoke below, by which time it
	// is set.
	var spawn func() (*Instance, error)
	env.deps.Tools.Register("recurse_tool", tool.HandlerFunc(
		func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			mu.Lock()
			levels++
			level := levels
			if level > hardChainLevelBudget {
				// The independent bound: stop the chain here, before instantiating
				// another guest, whatever the depth cap did or did not do.
				overrun = true
				mu.Unlock()
				return domain.ToolResult{}, fmt.Errorf(
					"recursion reached level %d, past this test's hard budget of %d", level, hardChainLevelBudget)
			}
			mu.Unlock()

			// Marshalled directly rather than through callToolBody: this handler runs
			// underneath a wazero host frame (see newHostcallStack's comment), so a
			// t.Fatalf on marshal failure here would carry the same runtime.Goexit
			// risk the factory itself was rewritten to avoid. The request is a fixed,
			// always-marshalable struct, so the error path is unreachable in
			// practice; it is still reported as an error rather than assumed away.
			reqBody, err := json.Marshal(callToolRequest{Tool: "recurse_tool"})
			if err != nil {
				return domain.ToolResult{}, fmt.Errorf("marshal recurse_tool call_tool request: %w", err)
			}
			inst, err := spawn()
			if err != nil {
				return domain.ToolResult{}, err
			}
			out, err := inst.Invoke(ctx, opCallCallTool, reqBody)
			if err != nil {
				return domain.ToolResult{}, err
			}
			mu.Lock()
			bodies = append(bodies, out)
			mu.Unlock()
			return domain.ToolResult{CallID: call.ID, Success: true, Output: string(out)}, nil
		}))

	newInstance := newHostcallStack(t, fullGrant(), env.deps)
	spawn = newInstance
	budget := newFakeBudget(100) // high enough that only the depth cap can bite

	firstInst, err := newInstance()
	if err != nil {
		t.Fatalf("newInstance(): %v", err)
	}
	if _, err := firstInst.Invoke(budgetedCtx(budget), opCallCallTool,
		callToolBody(t, callToolRequest{Tool: "recurse_tool"})); err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if overrun {
		t.Fatalf("the chain ran to level %d: this test's own budget (%d) stopped it, not the depth cap (%d)",
			levels, hardChainLevelBudget, callToolDepthCap)
	}
	if len(bodies) != callToolDepthCap {
		t.Fatalf("the chain ran %d levels, want %d: the depth cap must stop it", len(bodies), callToolDepthCap)
	}
	// bodies[0] is the innermost answer: the call one level past the cap.
	deepest := decodeHostError(t, bodies[0])
	if deepest.Code != CodeDenied {
		t.Errorf("the call past the depth cap returned code %q, want %q (body %s)",
			deepest.Code, CodeDenied, bodies[0])
	}
	if !strings.Contains(deepest.Message, "recurse_tool") {
		t.Errorf("depth denial message %q does not name the tool", deepest.Message)
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
	// The refused call never ran, so it never spent the task's allowance either:
	// the depth check comes before the budget is charged.
	if got := budget.names(); len(got) != callToolDepthCap {
		t.Errorf("shared budget recorded %d calls (%v), want %d", len(got), got, callToolDepthCap)
	}
}

// I6: a plugin's call goes through tool.Registry.Execute like any other, so the
// tool policy refuses it exactly as it refuses the agent — and the plugin is told
// it was REFUSED (a denial with its event), not that the host broke.
func TestCallToolSurfacesAPolicyRefusalAsADenial(t *testing.T) {
	env := newTestEnv(t)
	reached := false
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionDeny), nil, nil)
	registry.Register("guarded_tool", tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		reached = true
		return domain.ToolResult{Success: true}, nil
	}))
	env.deps.Tools = registry
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	out, err := inst.Invoke(budgetedCtx(newFakeBudget(30)), opCallCallTool,
		callToolBody(t, callToolRequest{Tool: "guarded_tool"}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeDenied {
		t.Errorf("a refused tool call returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
	if !strings.Contains(got.Message, "guarded_tool") {
		t.Errorf("refusal message %q does not name the tool", got.Message)
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
	if reached {
		t.Error("the tool handler ran despite the policy refusal: call_tool is not a back door")
	}
}

// The other side of that taxonomy: a tool name that does not resolve is a host
// error, NOT a denial. Nothing was refused on authorization grounds, so counting
// it as a denial would inflate exactly the number an operator reads as "this
// plugin is overstepping".
func TestCallToolReportsAnUnknownToolWithoutADenial(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	out, err := inst.Invoke(budgetedCtx(newFakeBudget(30)), opCallCallTool,
		callToolBody(t, callToolRequest{Tool: "no_such_tool"}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	if got := decodeHostError(t, out); got.Code != CodeHostError {
		t.Errorf("call_tool(unknown tool) returned code %q, want %q (body %s)", got.Code, CodeHostError, out)
	}
	if denied := deniedEvents(t, env); len(denied) != 0 {
		t.Errorf("an unresolvable tool published %d denial events, want 0: %v", len(denied), denied)
	}
}

// The audit trail must show whose call it was: a plugin-initiated call carries
// Origin "plugin:<name>" and a model-initiated one carries the agent default, so
// a forensic pass over a time window can tell them apart.
func TestCallToolIsAuditedWithThePluginOrigin(t *testing.T) {
	env := newTestEnv(t)
	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, nil).WithAuditLog(audit)
	registry.Register("shared_tool", tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "ok"}, nil
	}))
	env.deps.Tools = registry
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	if _, err := inst.Invoke(budgetedCtx(newFakeBudget(30)), opCallCallTool,
		callToolBody(t, callToolRequest{CallID: "plugin-call", Tool: "shared_tool"})); err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	// The same tool, reached the way the agent's own loop reaches it: an unmarked
	// ctx. Both rows land in one trail, and they must not read the same.
	if _, err := registry.Execute(context.Background(), env.deps.Agent,
		domain.ToolCall{ID: "model-call", Name: "shared_tool"}); err != nil {
		t.Fatalf("Execute(model call): %v", err)
	}

	events, err := audit.Events()
	if err != nil {
		t.Fatalf("read audit events: %v", err)
	}
	origins := map[string]string{}
	for _, ev := range events {
		if ev.Action != "tool_executed" {
			continue
		}
		origins[ev.RequestID] = ev.Origin
	}
	if got := origins["plugin-call"]; got != "plugin:"+testPluginName {
		t.Errorf("plugin-initiated call audited with origin %q, want %q", got, "plugin:"+testPluginName)
	}
	if got := origins["model-call"]; got != tool.OriginAgent {
		t.Errorf("model-initiated call audited with origin %q, want %q", got, tool.OriginAgent)
	}
}

// task-7-review-2 Minor 1: a path-guard refusal is an authorization-class
// refusal exactly like a policy refusal — PathGuardrails.Before wraps
// port.ErrPathOutsideWorkspace for the same reason the tool policy returns
// tool.ErrPermissionDenied, and read_file's own workspace-guard check is already
// a denial for it (hostcall.go's readFile) — so call_tool must classify it as a
// denial too, not a host error.
func TestCallToolSurfacesAPathGuardRefusalAsADenial(t *testing.T) {
	env := newTestEnv(t)
	reached := false
	registry := tool.NewRegistry(
		tool.NewStaticPolicy(tool.DecisionAllow), nil,
		tool.NewPathGuardrails(port.NewWorkspacePathGuard(env.root), "path"),
	)
	registry.Register("read_path_tool", tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		reached = true
		return domain.ToolResult{Success: true}, nil
	}))
	env.deps.Tools = registry
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	outside := filepath.Join(filepath.Dir(env.root), "outside-the-workspace.txt")
	out, err := inst.Invoke(budgetedCtx(newFakeBudget(30)), opCallCallTool,
		callToolBody(t, callToolRequest{Tool: "read_path_tool", Arguments: map[string]string{"path": outside}}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeDenied {
		t.Errorf("a path-guard refusal returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
	if !strings.Contains(got.Message, "read_path_tool") {
		t.Errorf("refusal message %q does not name the tool", got.Message)
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
	if reached {
		t.Error("the tool handler ran despite the path-guard refusal: call_tool is not a back door around it")
	}
}

// The lower-stakes sibling named in the same review item: a validateInputSchema
// failure is authorized but malformed, not an authorization refusal, so it must
// be CodeInvalidRequest — the same code call_tool's own decode/field checks
// use — and never a denial.
func TestCallToolSurfacesASchemaFailureAsInvalidRequestNotADenial(t *testing.T) {
	env := newTestEnv(t)
	reached := false
	registry := tool.NewRegistry(tool.NewStaticPolicy(tool.DecisionAllow), nil, nil)
	registry.RegisterDescriptor(tool.Descriptor{
		Name:        "schema_tool",
		InputSchema: map[string]any{"required": []string{"text"}},
	}, tool.HandlerFunc(func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		reached = true
		return domain.ToolResult{Success: true}, nil
	}))
	env.deps.Tools = registry
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	// No "text" argument: fails the required-argument check before the policy,
	// enforcer or guardrails ever run.
	out, err := inst.Invoke(budgetedCtx(newFakeBudget(30)), opCallCallTool,
		callToolBody(t, callToolRequest{Tool: "schema_tool"}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeInvalidRequest {
		t.Errorf("a schema-invalid call returned code %q, want %q (body %s)", got.Code, CodeInvalidRequest, out)
	}
	if denied := deniedEvents(t, env); len(denied) != 0 {
		t.Errorf("a schema failure published %d denial events, want 0: it is not an authorization refusal", len(denied))
	}
	if reached {
		t.Error("the tool handler ran despite the invalid input")
	}
}

// task-7-review-2 Minor 2: a guest that calls call_tool while answering
// abi.OpManifest runs under the activation ctx readManifest hands it — which
// carries neither a call_tool depth marker nor a shared per-task tool budget,
// because it is not a dispatched tool call. This pins that call_tool denies it,
// naming the missing budget, exactly as any other unbudgeted entry does (see
// TestCallToolIsDeniedWhenNoSharedBudgetIsOnTheContext) — tool calls are
// task-time only.
func TestCallToolFromInsideOpManifestIsDeniedForNoBudget(t *testing.T) {
	env := newTestEnv(t)
	hostInst := newHostcallInstance(t, fullGrant(), env.deps)

	manifestReached := false
	assertionsRan := false
	guest := guestCallerFunc(func(ctx context.Context, op int32, in []byte) ([]byte, error) {
		if op != abi.OpManifest {
			t.Fatalf("readManifest called op %d, want abi.OpManifest (%d)", op, abi.OpManifest)
		}
		manifestReached = true
		// Simulate a guest that, while answering its self-description, also
		// calls call_tool — through the SAME ctx readManifest handed this guest.
		out, err := hostInst.Invoke(ctx, opCallCallTool, callToolBody(t, callToolRequest{Tool: "echo_tool"}))
		if err != nil {
			return nil, fmt.Errorf("invoke call_tool from inside OpManifest: %w", err)
		}
		got := decodeHostError(t, out)
		if got.Code != CodeDenied {
			t.Errorf("call_tool from inside OpManifest returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
		}
		if !strings.Contains(got.Message, "budget") {
			t.Errorf("refusal message %q does not name the missing budget", got.Message)
		}
		assertionsRan = true
		return []byte(`{"name":"n","version":"v","provides":[]}`), nil
	})

	if _, err := readManifest(context.Background(), guest); err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if !manifestReached {
		t.Fatal("readManifest never called the guest at abi.OpManifest")
	}
	if !assertionsRan {
		t.Fatal("the guest's call_tool attempt never ran its assertions")
	}
	if denied := deniedEvents(t, env); len(denied) != 1 {
		t.Errorf("published %d denial events, want exactly 1: %v", len(denied), denied)
	}
}

// TestCallToolResolvesAServiceTargetToTheProvidersTool is the WIRING test: the
// resolution helper having its own tests proves nothing if call_tool never
// calls it. Everything downstream must see the resolved tool — the shared
// budget above all, since a service name counted separately would be a channel
// around the task's allowance.
func TestCallToolResolvesAServiceTargetToTheProvidersTool(t *testing.T) {
	env := newTestEnv(t)
	env.deps.Services = resolverFunc(func(service, capability string) (string, error) {
		if service != "echo-service" || capability != "say" {
			return "", fmt.Errorf("unexpected %q/%q", service, capability)
		}
		return "echo_tool", nil
	})
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	budget := newFakeBudget(30)

	out, err := inst.Invoke(budgetedCtx(budget), opCallCallTool, callToolBody(t, callToolRequest{
		CallID:    "call-1",
		Tool:      "service:echo-service/say",
		Arguments: map[string]string{"text": "hi"},
	}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode call_tool response %s: %v", out, err)
	}
	if !result.Success || result.Output != "echo:hi" {
		t.Fatalf("call_tool response = %s, want the provider's tool to have run", out)
	}
	if got := budget.names(); len(got) != 1 || got[0] != "echo_tool" {
		t.Errorf("shared budget recorded %v, want the RESOLVED name [echo_tool]", got)
	}
}

// TestCallToolRefusesAnUnresolvableServiceWithoutSpendingBudget: a name that
// resolves to nothing never reached a tool, so it must not cost the task's
// allowance — and the answer has to say it was a SERVICE that could not be
// resolved, not that some tool was missing.
func TestCallToolRefusesAnUnresolvableServiceWithoutSpendingBudget(t *testing.T) {
	env := newTestEnv(t)
	env.deps.Services = resolverFunc(func(string, string) (string, error) {
		return "", fmt.Errorf("no mounted plugin provides service %q", "echo-service")
	})
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	budget := newFakeBudget(30)

	out, err := inst.Invoke(budgetedCtx(budget), opCallCallTool, callToolBody(t, callToolRequest{
		CallID: "call-1",
		Tool:   "service:echo-service/say",
	}))
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	if !strings.Contains(string(out), "no mounted plugin provides service") {
		t.Errorf("response = %s, want the resolver's own reason", out)
	}
	if got := budget.names(); len(got) != 0 {
		t.Errorf("shared budget recorded %v for a call that never reached a tool, want nothing", got)
	}
}
