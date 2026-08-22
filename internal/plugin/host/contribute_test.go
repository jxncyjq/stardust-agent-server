package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// guestCallerFunc adapts a plain function to guestCaller, so a test can stand
// in for the plugin's guest. Production always passes the pool.
type guestCallerFunc func(ctx context.Context, op int32, in []byte) ([]byte, error)

func (f guestCallerFunc) call(ctx context.Context, op int32, in []byte) ([]byte, error) {
	return f(ctx, op, in)
}

// contributionAgent is the caller every contribution test executes as. The
// registries these tests build carry no policy and no enforcer, so its only
// job is to be a non-zero identity in the audit trail.
func contributionAgent() domain.Agent {
	return domain.Agent{ID: "agent-1", CompanyID: "co-1", Role: "developer", Status: domain.AgentActive}
}

// fixtureDescriptor is the host-side descriptor for the one tool
// testdata/plugin.wasm declares. The NAME must be the fixture's declared tool
// (activation cross-checks it against the guest's manifest); everything else is
// the deployment's claim about the tool, which is exactly what Spec.Tools
// carries.
//
// Timeout is not decoration: validateSpec requires a positive one because it is
// the only bound on a call inside the guest, and it is also what bounds the
// teardown drain (see drainDeadline). It is generous here so that a slow machine
// cannot make the happy-path tests flaky.
func fixtureDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name:        fixtureProvidedTool,
		Description: "回显插件工具（测试夹具）",
		Group:       "plugins",
		RiskLevel:   "low",
		Timeout:     30 * time.Second,
	}
}

// contribution is one activated plugin plus the things a test needs to observe
// what its contribution did: the registry the tool entered, the audit log that
// registry writes to, and the ledger everything is filed in.
type contribution struct {
	ledger   *lifecycle.Ledger
	registry *tool.Registry
	audit    *adapter.MemoryAuditLog
	plugin   *Plugin
}

// newContribution activates the fixture plugin and returns the handles above.
//
// The owner is disposed in a t.Cleanup unconditionally, and that is not
// housekeeping: toolauth.Contribute writes to process-global state, so a test
// that left its contribution behind would panic the NEXT test that contributes
// the same name (and silently widen the gateable catalog for every test after
// it). Disposal is the only mechanism that undoes it, so every test that
// contributes must route through here or dispose its own owner.
func newContribution(t *testing.T) *contribution {
	t.Helper()

	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(nil, nil, nil).WithAuditLog(audit)
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Registry = registry

	p, err := Activate(context.Background(), ledger, testOwner, spec)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	return &contribution{ledger: ledger, registry: registry, audit: audit, plugin: p}
}

// execute runs the contributed fixture tool with the given arguments.
func (c *contribution) execute(t *testing.T, callID string, args map[string]string) (domain.ToolResult, error) {
	t.Helper()

	return c.registry.Execute(context.Background(), contributionAgent(), domain.ToolCall{
		ID:        callID,
		Name:      fixtureProvidedTool,
		Arguments: args,
	})
}

// TestContributedToolIsCallableAndGateable is the triple's first two thirds:
// after activation the tool is reachable through Registry.Execute AND the
// per-agent disabled_tools machinery can see it (toolauth.IsGateable). A tool
// that is callable but not gateable is an authorization bypass, not a cosmetic
// gap, which is why both halves are asserted in one test.
func TestContributedToolIsCallableAndGateable(t *testing.T) {
	c := newContribution(t)

	if !toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = false after contribution, want true: "+
			"a per-agent disabled_tools list cannot reach this tool at all", fixtureProvidedTool)
	}

	result, err := c.execute(t, "call-1", map[string]string{"text": "hi"})
	if err != nil {
		t.Fatalf("Execute(%q): %v", fixtureProvidedTool, err)
	}
	if !result.Success {
		t.Errorf("result.Success = false (error %q), want true", result.Error)
	}
	// The fixture answers "<tool>:<arguments as JSON>", so this proves the
	// arguments really travelled into the guest and its answer really came back
	// — not merely that some tool was registered under the name.
	if want := fixtureProvidedTool + `:{"text":"hi"}`; result.Output != want {
		t.Errorf("result.Output = %q, want %q", result.Output, want)
	}
	// The fixture always answers with the wrong call_id (see testdata/README.md):
	// the host owns the correlation id, and a guest must not be able to
	// misattribute its answer to another call.
	if result.CallID != "call-1" {
		t.Errorf("result.CallID = %q, want %q: the host, not the guest, owns the correlation id",
			result.CallID, "call-1")
	}

	// Both entries of the contribution live under the contribution owner, and
	// the instance owner holds the entry that reaches them (see ToolsOwner):
	// the tool and its gateable catalogue entry are revoked together, and by
	// something that cannot touch the runtime or the pool.
	assertOrderedLabels(t, c.ledger, ToolsOwner(testOwner),
		"tool:"+fixtureProvidedTool,
		gateableLabel(fixtureProvidedTool),
	)
	assertOrderedLabels(t, c.ledger, testOwner,
		ledgerLabelRuntime,
		ledgerLabelPool,
		ledgerLabelContributions,
	)
}

// TestDisposeOwnerRemovesTheContributedTool is the revocation half: one
// DisposeOwner must take the tool out of the registry, out of the gateable
// catalog and out of the ledger. The gateable assertion is the one that has
// been got wrong before (PR #80's C3): a contribution whose gateable entry is
// never revoked leaves a name that no agent can disable and no owner can
// reclaim.
func TestDisposeOwnerRemovesTheContributedTool(t *testing.T) {
	c := newContribution(t)

	// Prove the tool answered BEFORE disposal, so "not found" afterwards is a
	// revocation and not a contribution that never happened.
	if _, err := c.execute(t, "call-1", map[string]string{"text": "hi"}); err != nil {
		t.Fatalf("Execute before disposal: %v", err)
	}

	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	if _, err := c.execute(t, "call-2", map[string]string{"text": "hi"}); !errors.Is(err, tool.ErrToolNotFound) {
		t.Errorf("Execute after disposal error = %v, want %v", err, tool.ErrToolNotFound)
	}
	if toolauth.IsGateable(fixtureProvidedTool) {
		t.Errorf("toolauth.IsGateable(%q) = true after DisposeOwner, want false: "+
			"the gateable entry outlived the plugin that contributed it", fixtureProvidedTool)
	}
	if snapshot := c.ledger.Snapshot(); len(snapshot) != 0 {
		t.Errorf("ledger.Snapshot() = %v after DisposeOwner, want empty", snapshot)
	}
}

// TestDisposeOwnerStopsTheGuestFromBeingCalled pins the disposal ORDER the
// ledger's reverse-order contract gives us: the pool is drained before the
// runtime is closed, so a call that arrives afterwards is refused by the pool
// rather than reaching a closed wazero module.
func TestDisposeOwnerStopsTheGuestFromBeingCalled(t *testing.T) {
	c := newContribution(t)

	if err := c.ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}

	_, err := c.plugin.pool.call(context.Background(), abi.OpCallTool, []byte(`{"tool":"echo_tool"}`))
	if err == nil {
		t.Fatal("pool.call succeeded after DisposeOwner, want an error: the pool was not drained")
	}
	if !errors.Is(err, errPoolDraining) {
		t.Errorf("pool.call error = %v, want it to wrap %v", err, errPoolDraining)
	}
}

// TestContributedToolAttributesGuestCallsToThePlugin is the triple's third
// part: the ctx the handler hands the guest carries the "plugin:<name>" call
// origin, so a tool call the plugin drives back into the host is recorded as
// the plugin's and not as the agent's own work.
//
// The guest is substituted here rather than driven through wazero, because a
// real guest reaches the registry through the call_tool host function, which
// marks the origin itself (see hostCalls.callTool) — so a real-guest test would
// stay green even if this handler never marked anything, and would assert
// nothing about the handler. The double does exactly what call_tool does with
// the ctx it is given: it executes a tool. The audit event that comes out is
// what the origin is for.
func TestContributedToolAttributesGuestCallsToThePlugin(t *testing.T) {
	audit := adapter.NewMemoryAuditLog()
	registry := tool.NewRegistry(nil, nil, nil).WithAuditLog(audit)
	// The tool the "guest" calls back into. It is NOT the contributed tool: the
	// audit event under test is the one the plugin's own nested call produces.
	registry.Register("host_probe", tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "probed"}, nil
	}))

	ledger := lifecycle.NewLedger()
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	guest := guestCallerFunc(func(ctx context.Context, _ int32, _ []byte) ([]byte, error) {
		if _, err := registry.Execute(ctx, contributionAgent(), domain.ToolCall{
			ID:   "nested-1",
			Name: "host_probe",
		}); err != nil {
			return nil, err
		}
		return []byte(`{"success":true,"output":"ok"}`), nil
	})

	spec := Spec{Name: testPluginName, Tools: []tool.Descriptor{fixtureDescriptor()}, Registry: registry}
	contributeTools(ledger, testOwner, spec, guest, func(func() error) {})

	if _, err := registry.Execute(context.Background(), contributionAgent(), domain.ToolCall{
		ID:   "call-1",
		Name: fixtureProvidedTool,
	}); err != nil {
		t.Fatalf("Execute(%q): %v", fixtureProvidedTool, err)
	}

	events, err := audit.Events()
	if err != nil {
		t.Fatalf("audit.Events(): %v", err)
	}
	wantOrigin := "plugin:" + testPluginName
	found := false
	for _, event := range events {
		if event.SubjectID != "host_probe" {
			continue
		}
		found = true
		if event.Origin != wantOrigin {
			t.Errorf("audit event for the plugin's nested call has Origin = %q, want %q",
				event.Origin, wantOrigin)
		}
	}
	if !found {
		t.Fatalf("no audit event for the nested host_probe call in %+v: this test cannot see what it claims to assert", events)
	}
}

// TestASecondContributionOfTheSameToolNamePanics covers both halves of the
// duplicate-name stance. Two contributors owning one model-facing name is never
// a valid state — the registry would make which implementation the model
// reaches a load-order lottery, and the gateable catalog would show one entry
// governing whichever tool won — so both are fail-loud, and a contribution that
// dies half-way must leave nothing filed behind.
func TestASecondContributionOfTheSameToolNamePanics(t *testing.T) {
	t.Run("name already registered", func(t *testing.T) {
		first := newContribution(t)

		second := lifecycle.NewLedger()
		const secondOwner lifecycle.Owner = "plugin:second"
		t.Cleanup(func() { _ = second.DisposeOwner(secondOwner) })

		spec := fixtureSpec(t)
		spec.Registry = first.registry

		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("a second plugin contributing an already-registered tool name did not panic")
			}
			if !strings.Contains(toString(r), fixtureProvidedTool) {
				t.Errorf("panic %v does not name the conflicting tool %q", r, fixtureProvidedTool)
			}
			if labels := second.Snapshot()[secondOwner]; len(labels) != 0 {
				t.Errorf("the panicking activation left %v filed under %s, want nothing", labels, secondOwner)
			}
			if labels := second.Snapshot()[ToolsOwner(secondOwner)]; len(labels) != 0 {
				t.Errorf("the panicking activation left %v filed under %s, want nothing: the rollback must "+
					"reach the contribution side too", labels, ToolsOwner(secondOwner))
			}
		}()

		_, _ = Activate(context.Background(), second, secondOwner, spec)
	})

	t.Run("name already gateable", func(t *testing.T) {
		// A gateable name with no registry entry: the registry lets the
		// registration through and toolauth is what refuses, so this is the
		// half a "registry panics first" implementation would hide.
		revoke := toolauth.Contribute(toolauth.GateableTool{
			Name:        fixtureProvidedTool,
			Description: "someone else claimed this name",
		})
		t.Cleanup(revoke)

		ledger := lifecycle.NewLedger()
		t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

		spec := fixtureSpec(t)
		spec.Registry = tool.NewRegistry(nil, nil, nil)

		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("contributing an already-gateable tool name did not panic")
			}
			if !strings.Contains(toString(r), fixtureProvidedTool) {
				t.Errorf("panic %v does not name the conflicting tool %q", r, fixtureProvidedTool)
			}
			// The tool WAS registered before toolauth refused, so this asserts
			// the rollback runs even when the contribution panics.
			if labels := ledger.Snapshot()[testOwner]; len(labels) != 0 {
				t.Errorf("the panicking activation left %v filed under %s, want nothing", labels, testOwner)
			}
			if labels := ledger.Snapshot()[ToolsOwner(testOwner)]; len(labels) != 0 {
				t.Errorf("the panicking activation left %v filed under %s, want nothing: the tool WAS "+
					"registered before toolauth refused, so its entry must have been rolled back",
					labels, ToolsOwner(testOwner))
			}
			if _, err := spec.Registry.Execute(context.Background(), contributionAgent(), domain.ToolCall{
				ID:   "call-1",
				Name: fixtureProvidedTool,
			}); !errors.Is(err, tool.ErrToolNotFound) {
				t.Errorf("after the panicking contribution Execute error = %v, want %v",
					err, tool.ErrToolNotFound)
			}
		}()

		_, _ = Activate(context.Background(), ledger, testOwner, spec)
	})
}

// toString renders a recovered panic value for an assertion message.
func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestContributedToolPassesOnTheGuestsOwnFailure covers the difference between
// "the tool failed" and "the plugin is broken": a ToolResult the guest marked
// unsuccessful is the tool's answer and must reach the caller as a result, not
// as a Go error.
func TestContributedToolPassesOnTheGuestsOwnFailure(t *testing.T) {
	c := newContribution(t)

	result, err := c.execute(t, "call-1", map[string]string{"fail": "upstream refused"})
	if err != nil {
		t.Fatalf("Execute returned a Go error %v for a result the tool itself marked failed", err)
	}
	if result.Success {
		t.Error("result.Success = true, want false")
	}
	if result.Error != "upstream refused" {
		t.Errorf("result.Error = %q, want %q", result.Error, "upstream refused")
	}
}

// TestContributedToolReportsAnUndecodableGuestAnswer is the fail-loud case
// end-to-end: the guest answers with valid JSON that is not a ToolResult, and
// the handler must report it. Handing the caller an empty ToolResult instead
// would present a plugin whose answer nobody could read as a tool that
// succeeded and produced nothing.
func TestContributedToolReportsAnUndecodableGuestAnswer(t *testing.T) {
	c := newContribution(t)

	result, err := c.execute(t, "call-1", map[string]string{"malformed": "1"})
	if err == nil {
		t.Fatalf("Execute succeeded on an undecodable guest answer, result = %+v", result)
	}
	if !strings.Contains(err.Error(), fixtureProvidedTool) {
		t.Errorf("error %q does not name the tool %q", err, fixtureProvidedTool)
	}
}

// TestContributedToolReportsAGuestCallFailure asserts a failure to reach the
// guest at all is reported, wrapped with the plugin and tool it happened on.
func TestContributedToolReportsAGuestCallFailure(t *testing.T) {
	registry := tool.NewRegistry(nil, nil, nil)
	ledger := lifecycle.NewLedger()
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	boom := errors.New("guest is gone")
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) { return nil, boom })

	spec := Spec{Name: testPluginName, Tools: []tool.Descriptor{fixtureDescriptor()}, Registry: registry}
	contributeTools(ledger, testOwner, spec, guest, func(func() error) {})

	_, err := registry.Execute(context.Background(), contributionAgent(), domain.ToolCall{
		ID:   "call-1",
		Name: fixtureProvidedTool,
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), testPluginName) || !strings.Contains(err.Error(), fixtureProvidedTool) {
		t.Errorf("error %q does not name both the plugin %q and the tool %q",
			err, testPluginName, fixtureProvidedTool)
	}
}

// TestContributedToolWorksAfterTheActivationContextIsCancelled pins that the
// pool's factory does not inherit the activation's cancellation. Pooled
// instances are built lazily, on the first call that needs them — long after
// Activate returned — so a factory holding the activation ctx would either fail
// on arrival or hand out an instance wazero closes immediately.
func TestContributedToolWorksAfterTheActivationContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	registry := tool.NewRegistry(nil, nil, nil)
	ledger := lifecycle.NewLedger()
	spec := fixtureSpec(t)
	spec.Registry = registry

	if _, err := Activate(ctx, ledger, testOwner, spec); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	cancel()

	result, err := registry.Execute(context.Background(), contributionAgent(), domain.ToolCall{
		ID:        "call-1",
		Name:      fixtureProvidedTool,
		Arguments: map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("Execute after the activation context was cancelled: %v", err)
	}
	if !result.Success {
		t.Errorf("result.Success = false (error %q), want true", result.Error)
	}
}

// TestDecodeToolResult covers decodeToolResult's error branches directly: each
// of them is a guest answer this host must refuse rather than pass on as an
// empty-but-successful result.
func TestDecodeToolResult(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "no body", body: "", want: "no body"},
		{name: "not json", body: "not json", want: "decode"},
		{name: "not an object", body: `["ok"]`, want: "decode"},
		{name: "unknown field", body: `{"result":{"x":1}}`, want: "decode"},
		{name: "trailing data", body: `{"success":true}{"success":true}`, want: "trailing"},
		{name: "failure with no reason", body: `{"success":false}`, want: "no reason"},
		{name: "empty object", body: `{}`, want: "no reason"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := decodeToolResult([]byte(tc.body))
			if err == nil {
				t.Fatalf("decodeToolResult(%q) = %+v, want an error", tc.body, result)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	t.Run("valid result", func(t *testing.T) {
		result, err := decodeToolResult([]byte(`{"call_id":"c1","success":true,"output":"done"}`))
		if err != nil {
			t.Fatalf("decodeToolResult on a valid result: %v", err)
		}
		if !result.Success || result.Output != "done" || result.CallID != "c1" {
			t.Errorf("decodeToolResult = %+v, want {c1 true done }", result)
		}
	})
}
