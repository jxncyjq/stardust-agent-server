package host

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// The self-description testdata/plugin.wasm returns for abi.OpManifest; see
// testdata/README.md. A rebuild that changes any of these three values breaks
// the activation tests loudly instead of letting a cross-check pass against a
// manifest nobody looked at.
const (
	fixtureManifestName    = "legion-test-plugin"
	fixtureManifestVersion = "0.1.0"
	fixtureProvidedTool    = "echo_tool"
)

// testOwner is the ledger owner every activation test files its plugin under.
const testOwner lifecycle.Owner = "plugin:legion-test-plugin"

// fixtureSpec is a Spec that activates testdata/plugin.wasm successfully: the
// host claims exactly the one tool the guest declares, and nothing is granted
// (the fixture imports no host function, so an empty Grant needs no Deps).
//
// Registry is a fresh one per call, so an activation test's contributed tools
// cannot be seen by another test's registry. The gateable catalog cannot be
// isolated that way — toolauth.Contribute is process-global — so every test that
// activates successfully MUST dispose its owner (see newContribution).
func fixtureSpec(t *testing.T) Spec {
	t.Helper()

	wasmBytes, err := fixtureWasm()
	if err != nil {
		t.Fatalf("read fixture wasm: %v", err)
	}
	return Spec{
		Name:         fixtureManifestName,
		Wasm:         wasmBytes,
		Tools:        []tool.Descriptor{fixtureDescriptor()},
		Registry:     tool.NewRegistry(nil, nil, nil),
		MaxInstances: 1,
		MemoryPages:  testMemoryPages,
	}
}

// hostcallSpec is a Spec for testdata/hostcall.wasm, which imports every
// legion host function. Its op 0 is not the manifest op, so it self-describes
// as an unsupported-op error envelope — that is what makes it useful here.
// Tools names a placeholder tool only to satisfy validateSpec's non-empty-Tools
// precondition (Minor 4): every test using hostcallSpec fails before crossCheck
// ever looks at Tools (at CheckImports, or at the manifest's missing name), so
// its actual value is irrelevant to them — and no test using it ever reaches the
// contribution step, so the placeholder never enters the gateable catalog.
func hostcallSpec(t *testing.T) Spec {
	t.Helper()

	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}
	return Spec{
		Name: testPluginName,
		Wasm: wasmBytes,
		Tools: []tool.Descriptor{{
			Name:        "unused_placeholder_tool",
			Description: "placeholder",
			Group:       "plugins",
		}},
		Registry:     tool.NewRegistry(nil, nil, nil),
		MaxInstances: 1,
		MemoryPages:  testMemoryPages,
	}
}

// watchGuestClose returns a context that records whether wazero closed a
// module instantiated with it. A rollback that only forgot its ledger entries
// without running their disposers would leave the guest module open, and the
// ledger snapshot alone cannot see that.
func watchGuestClose(ctx context.Context) (context.Context, *atomic.Bool) {
	var closed atomic.Bool
	notified := experimental.WithCloseNotifier(ctx, experimental.CloseNotifyFunc(
		func(context.Context, uint32) { closed.Store(true) }))
	return notified, &closed
}

// assertOwnerRolledBack asserts that nothing remains filed under owner. That
// the disposers really RAN (rather than being dropped) is asserted separately,
// with watchGuestClose, wherever a guest module existed to close.
func assertOwnerRolledBack(t *testing.T, ledger *lifecycle.Ledger, owner lifecycle.Owner) {
	t.Helper()

	if labels := ledger.Snapshot()[owner]; len(labels) != 0 {
		t.Fatalf("after a failed activation ledger still holds %v for owner %s, want nothing: "+
			"the rollback did not run", labels, owner)
	}
}

// TestActivateFilesRuntimeAndInstance covers the happy path: the manifest the
// guest declares is returned, the plugin answers a call, and every resource
// activation created — runtime, instance, instance pool, and the two entries per
// contributed tool — is filed under the owner so a later DisposeOwner can revoke
// them.
func TestActivateFilesRuntimeAndInstance(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	ledger := lifecycle.NewLedger()

	p, err := Activate(ctx, ledger, testOwner, fixtureSpec(t))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })

	if p.Name != fixtureManifestName {
		t.Errorf("Plugin.Name = %q, want %q", p.Name, fixtureManifestName)
	}
	if p.Manifest.Name != fixtureManifestName || p.Manifest.Version != fixtureManifestVersion {
		t.Errorf("Plugin.Manifest = %+v, want name %q version %q",
			p.Manifest, fixtureManifestName, fixtureManifestVersion)
	}
	if len(p.Manifest.Provides) != 1 || p.Manifest.Provides[0] != fixtureProvidedTool {
		t.Errorf("Plugin.Manifest.Provides = %v, want [%q]", p.Manifest.Provides, fixtureProvidedTool)
	}
	if p.pool == nil {
		t.Fatal("Plugin.pool is nil")
	}

	// The plugin must be usable, not merely activated: this call runs on an
	// instance the pool builds for it, so a plugin whose guest no longer answers
	// would make the happy path a shell.
	out, err := p.pool.call(ctx, opEcho, []byte(`{"name":"legion","n":21}`))
	if err != nil {
		t.Fatalf("call(opEcho) on the activated plugin: %v", err)
	}
	if !strings.Contains(string(out), `"doubled":42`) {
		t.Errorf("call(opEcho) = %s, want it to contain \"doubled\":42", out)
	}

	labels := ledger.Snapshot()[testOwner]
	want := []string{
		ledgerLabelRuntime,
		ledgerLabelInstance,
		ledgerLabelPool,
		"tool:" + fixtureProvidedTool,
		gateableLabel(fixtureProvidedTool),
	}
	if len(labels) != len(want) {
		t.Fatalf("ledger.Snapshot()[%s] = %v, want %v", testOwner, labels, want)
	}
	for i, label := range want {
		if labels[i] != label {
			t.Errorf("ledger.Snapshot()[%s][%d] = %q, want %q", testOwner, i, labels[i], label)
		}
	}

	if err := ledger.DisposeOwner(testOwner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if labels := ledger.Snapshot()[testOwner]; len(labels) != 0 {
		t.Errorf("after DisposeOwner ledger still holds %v", labels)
	}
	if !guestClosed.Load() {
		t.Error("after DisposeOwner no guest module was closed: the disposers were dropped rather than run")
	}
	if _, err := p.pool.call(context.Background(), opEcho, nil); err == nil {
		t.Error("the plugin still answers a call after DisposeOwner, want an error: its pool was not drained")
	}
}

// TestActivateFilesTheInstanceBeforeReadingTheManifest pins the ordering the
// rollback tests depend on, which their own assertions cannot see: closing the
// plugin's runtime closes the guest module too, so "mismatch left nothing
// filed" looks identical whether the instance was filed before the cross-check
// or after it. This test looks at the ledger from INSIDE the manifest read —
// wazero notifies a function listener before plugin_invoke runs — so moving
// the cross-check (and the read that feeds it) ahead of the instance's
// ledger.Add fails here.
func TestActivateFilesTheInstanceBeforeReadingTheManifest(t *testing.T) {
	ledger := lifecycle.NewLedger()

	var mu sync.Mutex
	var atInvoke []string
	seen := false
	listener := experimental.FunctionListenerFunc(func(
		_ context.Context, _ api.Module, _ api.FunctionDefinition, params []uint64, _ experimental.StackIterator,
	) {
		mu.Lock()
		defer mu.Unlock()
		if seen {
			return // only the first call — the manifest read — is the interesting one
		}
		// Confirm this really is the manifest read (params[0] is the op wazero
		// received, packed the same way Instance.Invoke sends it) before
		// recording anything: a future Activate that invokes some other op
		// first must not be able to keep this test green while pinning the
		// wrong call. If op never matches, seen stays false and the "listener
		// never fired" guard below catches it.
		if len(params) == 0 || params[0] != uint64(uint32(abi.OpManifest)) {
			return
		}
		seen = true
		atInvoke = ledger.Snapshot()[testOwner]
	})
	ctx := experimental.WithFunctionListenerFactory(context.Background(),
		experimental.FunctionListenerFactoryFunc(func(def api.FunctionDefinition) experimental.FunctionListener {
			// A Rust release build carries no name section, so Name() is empty
			// for every guest function; the export name is what identifies
			// plugin_invoke.
			for _, exported := range def.ExportNames() {
				if exported == abi.ExportInvoke {
					return listener
				}
			}
			return nil
		}))

	p, err := Activate(ctx, ledger, testOwner, fixtureSpec(t))
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = ledger.DisposeOwner(testOwner) })
	if p.pool == nil {
		t.Fatal("Plugin.pool is nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatal("the listener never fired for plugin_invoke: this test cannot see the ordering it claims to pin")
	}
	found := false
	for _, label := range atInvoke {
		if label == ledgerLabelInstance {
			found = true
		}
	}
	if !found {
		t.Errorf("at the manifest read the ledger held %v, want it to already hold %q: "+
			"the manifest cross-check must fail with the instance filed, or its rollback path is dead code",
			atInvoke, ledgerLabelInstance)
	}
}

// TestActivateProvidesMismatchRollsBack is this task's central case: the host
// claims a tool the guest does not declare. The cross-check runs after the
// instance is filed in the ledger, so the failure has something to roll back —
// and the assertion that nothing is left filed is what pins the rollback.
func TestActivateProvidesMismatchRollsBack(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	ledger := lifecycle.NewLedger()

	// A second owner's entry must survive: rolling one activation back may not
	// touch anybody else's resources.
	const other lifecycle.Owner = "plugin:other"
	ledger.Add(other, "unrelated", func() error { return nil })

	spec := fixtureSpec(t)
	spec.Tools = []tool.Descriptor{{Name: "absent_tool", Description: "not declared by the guest", Group: "plugins"}}

	p, err := Activate(ctx, ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded although the host claims a tool the guest does not declare")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}

	msg := err.Error()
	for _, want := range []string{
		"cross-check",       // which step failed
		fixtureManifestName, // which plugin
		"absent_tool",       // what the host claimed
		fixtureProvidedTool, // what the guest declared
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}

	assertOwnerRolledBack(t, ledger, testOwner)
	if !guestClosed.Load() {
		t.Error("the guest module was never closed: the rollback dropped its ledger entries " +
			"without running the disposers")
	}
	if labels := ledger.Snapshot()[other]; len(labels) != 1 {
		t.Errorf("ledger.Snapshot()[%s] = %v, want the unrelated entry to survive the rollback", other, labels)
	}
}

// TestActivateNameMismatchRollsBack covers the other half of the cross-check:
// the host and the guest disagree about the plugin's own identity.
func TestActivateNameMismatchRollsBack(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Name = "legion-test-plugin-renamed"

	const owner lifecycle.Owner = "plugin:legion-test-plugin-renamed"
	p, err := Activate(ctx, ledger, owner, spec)
	if err == nil {
		t.Fatal("Activate succeeded although the host and the guest disagree about the plugin name")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	msg := err.Error()
	for _, want := range []string{"cross-check", "legion-test-plugin-renamed", fixtureManifestName} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}

	assertOwnerRolledBack(t, ledger, owner)
	if !guestClosed.Load() {
		t.Error("the guest module was never closed after a name mismatch")
	}
}

// TestActivateInstantiationFailureRollsBack covers a failure before the
// instance exists: a module with the ABI exports but no linear memory. The
// runtime is already filed at that point, so the rollback must still leave the
// owner empty.
func TestActivateInstantiationFailureRollsBack(t *testing.T) {
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Wasm = wasmModuleExporting(t, abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke)

	p, err := Activate(context.Background(), ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded on a module with no linear memory")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	if !strings.Contains(err.Error(), "instantiate plugin module") {
		t.Errorf("error %q does not name the instantiation step", err)
	}
	assertOwnerRolledBack(t, ledger, testOwner)
}

// TestActivateCompileFailureRollsBack covers the earliest failure: bytes that
// are not a wasm module at all. The runtime is filed before compilation, so
// even this failure has something to revoke.
func TestActivateCompileFailureRollsBack(t *testing.T) {
	ledger := lifecycle.NewLedger()

	spec := fixtureSpec(t)
	spec.Wasm = []byte("this is not a wasm module")

	p, err := Activate(context.Background(), ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded on bytes that are not a wasm module")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	if !strings.Contains(err.Error(), "compile plugin module") {
		t.Errorf("error %q does not name the compile step", err)
	}
	assertOwnerRolledBack(t, ledger, testOwner)
}

// TestActivateRejectsUngrantedImports proves Activate consults CheckImports:
// the host-call fixture imports every host function, so an empty grant must be
// refused with the readable capability error rather than wazero's raw link
// error.
func TestActivateRejectsUngrantedImports(t *testing.T) {
	ledger := lifecycle.NewLedger()

	spec := hostcallSpec(t)
	spec.Grant = perm.Grant{}

	p, err := Activate(context.Background(), ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded although the guest imports host functions nothing granted")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	msg := err.Error()
	for _, want := range []string{funcHTTPRequest, "capability", "not granted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q; wazero's raw link error is not the readable one", msg, want)
		}
	}
	if strings.Contains(msg, "not exported in module") {
		t.Errorf("error %q is wazero's raw link error: CheckImports did not run first", msg)
	}
	assertOwnerRolledBack(t, ledger, testOwner)
}

// TestActivateRejectsAnUnusableSelfDescription covers a guest whose op 0
// answers something that is not a manifest: the host-call fixture does not
// implement the manifest op and returns its unsupported-op envelope, which
// decodes as JSON but carries no name.
func TestActivateRejectsAnUnusableSelfDescription(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	ledger := lifecycle.NewLedger()

	env := newTestEnv(t)
	spec := hostcallSpec(t)
	spec.Grant = fullGrant()
	spec.Deps = env.deps

	p, err := Activate(ctx, ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded although the guest's self-description carries no name")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	msg := err.Error()
	for _, want := range []string{"cross-check", "name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	assertOwnerRolledBack(t, ledger, testOwner)
	if !guestClosed.Load() {
		t.Error("the guest module was never closed after an unusable self-description")
	}
}

// TestActivateReportsAFailureWhileRollingBack asserts the rollback does not
// swallow a disposer failure and does not lose the activation error either:
// both must reach the caller.
//
// The failing disposer here is one Activate itself filed (the instance's),
// not a foreign entry pre-filed under the owner: since rollback is now
// handle-scoped (Important 1) and Activate refuses to start under a
// non-empty owner, pre-filing a "hostile" entry under testOwner — as this
// test used to — would be rejected by the owner-exclusivity precondition
// before Activate ever reached the failure this test wants to exercise.
// closeInstance is swapped for the duration of the test to force that.
func TestActivateReportsAFailureWhileRollingBack(t *testing.T) {
	ctx, guestClosed := watchGuestClose(context.Background())
	ledger := lifecycle.NewLedger()

	disposeErr := errors.New("hostile close refused")
	originalCloseInstance := closeInstance
	closeInstance = func(ctx context.Context, inst *Instance) error {
		// Still release the real wazero resources — a disposer that reports a
		// failure has not thereby earned the right to leak the module it was
		// supposed to close.
		_ = originalCloseInstance(ctx, inst)
		return disposeErr
	}
	t.Cleanup(func() { closeInstance = originalCloseInstance })

	spec := fixtureSpec(t)
	spec.Tools = []tool.Descriptor{{Name: "absent_tool", Description: "not declared by the guest", Group: "plugins"}}

	p, err := Activate(ctx, ledger, testOwner, spec)
	if err == nil {
		t.Fatal("Activate succeeded although the host claims a tool the guest does not declare")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	if !errors.Is(err, disposeErr) {
		t.Errorf("error %q does not carry the disposer failure", err)
	}
	if !strings.Contains(err.Error(), "absent_tool") {
		t.Errorf("error %q lost the activation failure while reporting the rollback failure", err)
	}
	assertOwnerRolledBack(t, ledger, testOwner)
	if !guestClosed.Load() {
		t.Error("the guest module was never closed: a failing disposer must still run the real close, " +
			"not skip it because it also reports an error")
	}
}

// TestActivateRejectsAReusedOwner covers Important 1's other half: the
// fail-loud precondition. A caller that reuses an owner already holding
// entries — the realistic case being a hot-reload that forgot to dispose the
// previous activation first — must be refused loudly, naming the owner and
// what is already filed under it, rather than having Activate silently tear
// those entries down (which is what the old DisposeOwner(owner)-based
// rollback would have done on any later failure).
func TestActivateRejectsAReusedOwner(t *testing.T) {
	ledger := lifecycle.NewLedger()
	ledger.Add(testOwner, "preexisting", func() error { return nil })

	p, err := Activate(context.Background(), ledger, testOwner, fixtureSpec(t))
	if err == nil {
		t.Fatal("Activate succeeded although the owner already holds an entry")
	}
	if p != nil {
		t.Errorf("Activate returned a plugin (%+v) together with an error", p)
	}
	msg := err.Error()
	for _, want := range []string{string(testOwner), "preexisting"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	// The precondition must reject before doing anything, not roll anything
	// back: the pre-filed entry is foreign to this call and must be left
	// exactly as it was.
	if labels := ledger.Snapshot()[testOwner]; len(labels) != 1 || labels[0] != "preexisting" {
		t.Errorf("ledger.Snapshot()[%s] = %v, want only the pre-filed entry, untouched", testOwner, labels)
	}
}

// TestActivateRejectsAnInvalidSpec covers the assembly-time invariants: every
// one of these is a caller mistake that must be reported, not worked around
// with a default.
func TestActivateRejectsAnInvalidSpec(t *testing.T) {
	wasmBytes, err := fixtureWasm()
	if err != nil {
		t.Fatalf("read fixture wasm: %v", err)
	}
	valid := Spec{
		Name:         fixtureManifestName,
		Wasm:         wasmBytes,
		Tools:        []tool.Descriptor{fixtureDescriptor()},
		Registry:     tool.NewRegistry(nil, nil, nil),
		MaxInstances: 1,
		MemoryPages:  testMemoryPages,
	}

	tests := []struct {
		name      string
		nilLedger bool
		owner     lifecycle.Owner
		mutate    func(*Spec)
		want      string
	}{
		{name: "nil ledger", nilLedger: true, owner: testOwner, mutate: func(*Spec) {}, want: "ledger"},
		{name: "empty owner", owner: "", mutate: func(*Spec) {}, want: "owner"},
		{name: "empty name", owner: testOwner, mutate: func(s *Spec) { s.Name = "" }, want: "Name"},
		{name: "no wasm", owner: testOwner, mutate: func(s *Spec) { s.Wasm = nil }, want: "Wasm"},
		{name: "zero memory pages", owner: testOwner, mutate: func(s *Spec) { s.MemoryPages = 0 }, want: "MemoryPages"},
		{name: "nil tools", owner: testOwner, mutate: func(s *Spec) { s.Tools = nil }, want: "Tools"},
		{name: "empty tools slice", owner: testOwner, mutate: func(s *Spec) { s.Tools = []tool.Descriptor{} }, want: "Tools"},
		{
			name:   "tool with no name",
			owner:  testOwner,
			mutate: func(s *Spec) { s.Tools = []tool.Descriptor{{Description: "d", Group: "g"}} },
			want:   "Name",
		},
		{
			name:   "tool with no description",
			owner:  testOwner,
			mutate: func(s *Spec) { s.Tools = []tool.Descriptor{{Name: fixtureProvidedTool, Group: "g"}} },
			want:   "Description",
		},
		{
			name:   "tool with no group",
			owner:  testOwner,
			mutate: func(s *Spec) { s.Tools = []tool.Descriptor{{Name: fixtureProvidedTool, Description: "d"}} },
			want:   "Group",
		},
		{
			name:   "duplicate tool name",
			owner:  testOwner,
			mutate: func(s *Spec) { s.Tools = []tool.Descriptor{fixtureDescriptor(), fixtureDescriptor()} },
			want:   "twice",
		},
		{name: "nil registry", owner: testOwner, mutate: func(s *Spec) { s.Registry = nil }, want: "Registry"},
		{name: "zero max instances", owner: testOwner, mutate: func(s *Spec) { s.MaxInstances = 0 }, want: "MaxInstances"},
		{name: "negative max instances", owner: testOwner, mutate: func(s *Spec) { s.MaxInstances = -1 }, want: "MaxInstances"},
		{
			name:   "conflicting deps plugin name",
			owner:  testOwner,
			mutate: func(s *Spec) { s.Deps.PluginName = "someone-else" },
			want:   "PluginName",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			tc.mutate(&spec)

			ledger := lifecycle.NewLedger()
			arg := ledger
			if tc.nilLedger {
				arg = nil
			}
			p, err := Activate(context.Background(), arg, tc.owner, spec)
			if err == nil {
				t.Fatalf("Activate succeeded on an invalid spec (%s)", tc.name)
			}
			if p != nil {
				t.Errorf("Activate returned a plugin (%+v) together with an error", p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if labels := ledger.Snapshot()[tc.owner]; len(labels) != 0 {
				t.Errorf("a rejected spec left %v filed under %s", labels, tc.owner)
			}
		})
	}
}

// TestDecodeManifest covers decodeManifest's error branches directly
// (Minor 2): the empty-body case is reachable in production whenever
// Instance.Invoke returns (nil, nil) — a zero-length packed result — and no
// existing fixture's op 0 exercises it or the invalid-JSON case, so this
// table test exists precisely to give both an "确实返回 error" assertion
// without a Rust fixture rebuild.
func TestDecodeManifest(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		want    Manifest
		wantErr string
	}{
		{name: "nil body", body: nil, wantErr: "guest returned no body"},
		{name: "empty body", body: []byte{}, wantErr: "guest returned no body"},
		{name: "truncated JSON object", body: []byte("{"), wantErr: "decode self-description"},
		{name: "JSON array instead of an object", body: []byte("[]"), wantErr: "decode self-description"},
		{
			name: "valid manifest",
			body: []byte(`{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}`),
			want: Manifest{Name: "legion-test-plugin", Version: "0.1.0", Provides: []string{"echo_tool"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeManifest(tc.body)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("decodeManifest(%q) succeeded, want an error containing %q", tc.body, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("decodeManifest(%q) error = %q, want it to contain %q", tc.body, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeManifest(%q): %v", tc.body, err)
			}
			if got.Name != tc.want.Name || got.Version != tc.want.Version || strings.Join(got.Provides, ",") != strings.Join(tc.want.Provides, ",") {
				t.Errorf("decodeManifest(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

// TestDecodeManifestQuotesAndTruncatesAnUnusableBody covers Minor 3: body is
// whatever the guest wrote, bounded only by Spec.MemoryPages, and may carry
// control characters — it must never be spliced into an error raw, and a
// long body must be capped rather than inflating the error (and, once Task 6
// logs it, the log line) to match.
func TestDecodeManifestQuotesAndTruncatesAnUnusableBody(t *testing.T) {
	t.Run("control characters are escaped, not literal", func(t *testing.T) {
		body := []byte("not json\x01\n\x02")

		_, err := decodeManifest(body)
		if err == nil {
			t.Fatal("decodeManifest succeeded on non-JSON bytes")
		}
		msg := err.Error()
		if strings.ContainsRune(msg, '\x01') {
			t.Errorf("error %q contains the raw control byte instead of an escaped one", msg)
		}
		if strings.ContainsRune(msg, '\n') {
			t.Errorf("error %q contains a literal newline instead of an escaped one", msg)
		}
		if !strings.Contains(msg, `\x01`) {
			t.Errorf("error %q does not show the control byte escaped via %%q", msg)
		}
	})

	t.Run("a long body is truncated", func(t *testing.T) {
		body := append([]byte("{"), bytes.Repeat([]byte("a"), 1000)...) // invalid JSON, 1001 bytes total

		_, err := decodeManifest(body)
		if err == nil {
			t.Fatal("decodeManifest succeeded on invalid JSON")
		}
		msg := err.Error()
		if len(msg) > 600 {
			t.Errorf("error is %d bytes long; want the guest body capped rather than spliced in full: %s", len(msg), msg)
		}
		if !strings.Contains(msg, "1001 bytes") {
			t.Errorf("error %q does not report the untruncated body length", msg)
		}
	})
}
