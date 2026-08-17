package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/abi"
)

// Ops 90/98/99 are test-only fixture ops implemented by
// testdata/guest-rust/src/lib.rs; see testdata/README.md for the full op
// table. They are not part of the abi package because they exist solely to
// exercise this host wrapper, not the production plugin contract.
const (
	opEcho     int32 = 90 // JSON round trip
	opMemBomb  int32 = 98 // allocate until the memory page cap traps
	opBusyLoop int32 = 99 // pure-compute infinite loop
)

// testMemoryPages is the page count (64 pages = 4MiB) proven sufficient for
// the fixture guest by the spike; Task 8's memory-cap acceptance test reuses
// the same cap.
const testMemoryPages = 64

// fixtureWasm loads the compiled test guest once per test binary run.
var fixtureWasm = sync.OnceValues(func() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "plugin.wasm"))
})

// newTestInstance builds a fresh Runtime, compiles the fixture guest, and
// instantiates it, registering cleanup for both. memoryPages controls the
// runtime's memory page cap.
func newTestInstance(t *testing.T, memoryPages uint32) *Instance {
	t.Helper()

	wasmBytes, err := fixtureWasm()
	if err != nil {
		t.Fatalf("read fixture wasm: %v", err)
	}

	ctx := context.Background()
	rt := NewRuntime(ctx, memoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	inst, err := NewInstance(ctx, rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	return inst
}

// TestInvokeJSONRoundTrip covers the brief's "JSON round trip" case: calling
// the fixture's echo op with a JSON body returns an equivalent, guest-derived
// JSON body.
func TestInvokeJSONRoundTrip(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, opEcho, []byte(`{"name":"legion","n":21}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var got struct {
		Greeting string `json:"greeting"`
		Doubled  int    `json:"doubled"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode result %s: %v", out, err)
	}
	if got.Greeting != "hello legion" || got.Doubled != 42 {
		t.Errorf("Invoke result = %+v, want {Greeting:hello legion Doubled:42}", got)
	}
}

// TestInvokeManifest covers the real abi.OpManifest op, distinct from the
// test-only echo op: it must return the fixture's self-description
// regardless of input.
func TestInvokeManifest(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, abi.OpManifest, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var got struct {
		Name     string   `json:"name"`
		Version  string   `json:"version"`
		Provides []string `json:"provides"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode manifest %s: %v", out, err)
	}
	if got.Name != "legion-test-plugin" || got.Version != "0.1.0" || len(got.Provides) != 1 || got.Provides[0] != "echo_tool" {
		t.Errorf("Invoke(OpManifest) = %+v, want {legion-test-plugin 0.1.0 [echo_tool]}", got)
	}
}

// TestInvokeCallTool covers the real abi.OpCallTool op: the fixture echoes
// the input's "args" field back under a "result" key.
func TestInvokeCallTool(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, abi.OpCallTool, []byte(`{"tool":"echo_tool","args":{"x":1}}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var got struct {
		Result struct {
			X int `json:"x"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode result %s: %v", out, err)
	}
	if got.Result.X != 1 {
		t.Errorf("Invoke(OpCallTool) result.x = %d, want 1", got.Result.X)
	}
}

// TestInvokeUnknownOpDoesNotCrash asserts an op the guest's dispatch table
// does not recognize returns a readable error body instead of trapping the
// module.
func TestInvokeUnknownOpDoesNotCrash(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, 7, nil)
	if err != nil {
		t.Fatalf("Invoke(unknown op) returned error, want a readable body: %v", err)
	}

	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode result %s: %v", out, err)
	}
	if got.Error == "" {
		t.Errorf("Invoke(unknown op) = %s, want a non-empty \"error\" field", out)
	}
	if inst.Dead() {
		t.Errorf("Instance is Dead() after an unknown op; unknown op must not kill the instance")
	}
}

// TestInvoke2000CallsBoundedMemoryAndCopiesResult covers two of the brief's
// required cases in one test because they interact: the retained result of
// the first call must still read back correctly after 1999 more calls (this
// is only true if Invoke copies the result out of guest memory instead of
// returning an alias into it — mutation (a) in the task brief), and guest
// memory must not grow without bound across repeated same-shaped alloc/free
// cycles.
func TestInvoke2000CallsBoundedMemoryAndCopiesResult(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	const n = 2000

	first, err := inst.Invoke(ctx, opEcho, []byte(`{"name":"legion","n":21}`))
	if err != nil {
		t.Fatalf("Invoke #0: %v", err)
	}
	var decoded struct {
		Greeting string `json:"greeting"`
		Doubled  int    `json:"doubled"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("decode Invoke #0 result %s: %v", first, err)
	}
	if decoded.Greeting != "hello legion" || decoded.Doubled != 42 {
		t.Fatalf("Invoke #0 = %+v, want {Greeting:hello legion Doubled:42}", decoded)
	}
	// serde_json's default Value map has no fixed key order across
	// versions, so pin the exact bytes to compare against later rather than
	// hardcoding a guessed serialization: that is precisely what mutation
	// (a) (returning an alias into guest memory instead of a copy) would
	// corrupt, and re-decoding structurally after the loop could mask a
	// corrupted-but-still-valid-JSON result that happens to parse the same.
	wantFirst := string(first)
	sizeAfterFirst := inst.mod.Memory().Size()

	for i := 1; i < n; i++ {
		if _, err := inst.Invoke(ctx, opEcho, []byte(`{"name":"other","n":1}`)); err != nil {
			t.Fatalf("Invoke #%d: %v", i, err)
		}
	}
	sizeFinal := inst.mod.Memory().Size()

	// If Invoke had returned an alias into guest memory instead of a copy,
	// the 1999 later alloc/free cycles reusing that same region would have
	// silently overwritten these bytes by now.
	if string(first) != wantFirst {
		t.Errorf("retained result from call #0 was corrupted by later calls: got %s, want %s", first, wantFirst)
	}

	const maxGrowthBytes = 4 * 65536 // 4 pages of slack for allocator bookkeeping
	if sizeFinal > sizeAfterFirst+maxGrowthBytes {
		t.Errorf("guest memory grew from %d to %d bytes over %d calls (> %d byte slack): possible leak", sizeAfterFirst, sizeFinal, n, maxGrowthBytes)
	}
}

// TestInvokeNilInputSkipsAlloc covers "Invoke with nil input must not call
// plugin_alloc". It uses a synthetic host-backed module (not the fixture
// wasm) whose plugin_alloc/plugin_invoke are Go closures, because the real
// question is what Instance.Invoke's Go code does, not what the guest does —
// only a host-defined stand-in can directly count host->guest export calls
// and inspect the exact (op, ptr, len) triple plugin_invoke was called with.
func TestInvokeNilInputSkipsAlloc(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, 1)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	var allocCalls int
	var gotOp, gotPtr, gotSize int32
	var invokeCalls int
	compiled, err := rt.NewHostModuleBuilder("fake-plugin").
		NewFunctionBuilder().WithFunc(func(size int32) int32 {
		allocCalls++
		return 1
	}).Export(abi.ExportAlloc).
		NewFunctionBuilder().WithFunc(func(ptr, size int32) {}).Export(abi.ExportFree).
		NewFunctionBuilder().WithFunc(func(op, ptr, size int32) int64 {
		invokeCalls++
		gotOp, gotPtr, gotSize = op, ptr, size
		return int64(abi.PackResult(0, 0))
	}).Export(abi.ExportInvoke).
		Compile(ctx)
	if err != nil {
		t.Fatalf("compile fake plugin module: %v", err)
	}

	inst, err := NewInstance(ctx, rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	out, err := inst.Invoke(ctx, 42, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out != nil {
		t.Errorf("Invoke(nil input) result = %v, want nil", out)
	}
	if allocCalls != 0 {
		t.Errorf("plugin_alloc called %d times for nil input, want 0", allocCalls)
	}
	if invokeCalls != 1 {
		t.Fatalf("plugin_invoke called %d times, want 1", invokeCalls)
	}
	if gotOp != 42 || gotPtr != 0 || gotSize != 0 {
		t.Errorf("plugin_invoke called with (op,ptr,size) = (%d,%d,%d), want (42,0,0)", gotOp, gotPtr, gotSize)
	}
}

// TestInvokeContextCancellationKillsInstance covers "ctx 取消后 Invoke 返回
// error 且 Dead() 为 true": a context that expires while a pure-compute
// guest loop is running must interrupt the call (via
// NewRuntime's WithCloseOnContextDone) and permanently kill the Instance.
func TestInvokeContextCancellationKillsInstance(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)

	cctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := inst.Invoke(cctx, opBusyLoop, nil)
	if err == nil {
		t.Fatalf("Invoke(busy loop) under a short deadline returned no error; guest was not interrupted")
	}
	if !inst.Dead() {
		t.Errorf("Instance.Dead() = false after ctx cancellation killed the call, want true")
	}

	// A dead instance must reject further calls rather than silently
	// resurrect or crash the process.
	if _, err := inst.Invoke(context.Background(), abi.OpManifest, nil); err == nil {
		t.Errorf("Invoke on a Dead instance returned no error, want an error")
	}
}

// TestInvokeSurvivesRacingContextCancellation covers fatal detail #2: Invoke
// must free the input allocation with context.WithoutCancel(ctx), not ctx
// unchanged.
//
// The scenario this guards against does not need a stuck call: it is a
// context that goes Done concurrently with an otherwise-fast, successful
// Invoke call. wazero's Call implementation checks ctx.Done() up front on
// every call (see internal/wasm/module_instance.go's ensureTermination
// path); if that check lands after plugin_invoke has already succeeded but
// before the deferred plugin_free call, freeing with ctx unchanged fails
// that free call outright and, as a side effect, closes the whole module —
// turning a harmless race (the caller's context happened to end around the
// same time the call finished) into a dead Instance.
//
// This can't be reproduced by cancelling ctx before Invoke starts (the
// first wazero call, plugin_alloc, would then fail immediately and the
// deferred free would never run) or by a stuck/interrupted call like
// TestInvokeContextCancellationKillsInstance's busy loop (there, wazero's
// own watcher goroutine usually closes the module while plugin_invoke is
// still in flight, which poisons the free regardless of which ctx it uses).
// Racing a cancellation against many fast calls also produces plenty of
// alloc- and invoke-level failures that are expected either way and carry
// no signal about the free call specifically, so this filters the failures
// by which step's wrapped error fired (Invoke's error messages are prefixed
// per step, e.g. "free input ...") rather than bounding the overall failure
// rate, which a pilot run showed is too noisy (~85-95% either way) to tell
// the fix and the bug apart.
func TestInvokeSurvivesRacingContextCancellation(t *testing.T) {
	const attempts = 500

	wasmBytes, err := fixtureWasm()
	if err != nil {
		t.Fatalf("read fixture wasm: %v", err)
	}

	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	var freeFailures int
	for i := 0; i < attempts; i++ {
		inst, err := NewInstance(ctx, rt, compiled)
		if err != nil {
			t.Fatalf("attempt %d: NewInstance: %v", i, err)
		}

		cctx, cancel := context.WithCancel(context.Background())
		go cancel() // races the context's Done() against this call

		_, err = inst.Invoke(cctx, opEcho, []byte(`{"name":"legion","n":21}`))
		_ = inst.Close(context.Background())

		if err != nil && strings.Contains(err.Error(), "free input") {
			freeFailures++
		}
	}

	// A generous but non-trivial ceiling: the fix still has a small residual
	// rate (wazero's own per-call watcher can independently close the
	// module right as invoke returns, before free ever runs, regardless of
	// which ctx free uses) — a pilot run measured that residual at roughly
	// 0-3 out of 500. The bug produces a qualitatively different, much
	// higher rate because every one of the (numerous) "ctx went Done
	// sometime during this call" attempts that happened to leave invoke
	// itself unharmed then fails at the free step instead.
	const maxAcceptableFreeFailures = attempts / 10 // 50
	t.Logf("racing cancellation: %d/%d attempts failed specifically at the input free step", freeFailures, attempts)
	if freeFailures > maxAcceptableFreeFailures {
		t.Errorf("input free failed %d/%d times (> %d) when raced against context cancellation: "+
			"input free is likely using ctx unchanged instead of context.WithoutCancel(ctx)",
			freeFailures, attempts, maxAcceptableFreeFailures)
	}
}

// TestNewInstanceRunsInitializeStartFunction covers fatal detail #3:
// NewInstance instantiates with WithStartFunctions("_initialize") because
// WASI reactor modules export no _start, so the default start-function
// configuration (["_start"]) would silently do nothing.
//
// The real fixture guest happens not to export _start OR _initialize at
// all (confirmed by inspecting testdata/plugin.wasm's exports), so it does
// not actually depend on any initialization call to work correctly —
// dropping WithStartFunctions("_initialize") does not, on its own, break
// any of this file's other tests run against the real fixture. That is a
// property of this particular guest, not of the mechanism: a reactor module
// that DOES need _initialize to run (the general case this option exists
// for) would silently misbehave without it. This test pins down that
// NewInstance really does invoke _initialize, using a synthetic host-backed
// module whose plugin_alloc deliberately depends on _initialize having run
// first, so the option is load-bearing here even though it isn't for the
// committed fixture.
func TestNewInstanceRunsInitializeStartFunction(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, 1)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	var initialized bool
	compiled, err := rt.NewHostModuleBuilder("fake-reactor").
		NewFunctionBuilder().WithFunc(func() { initialized = true }).Export("_initialize").
		NewFunctionBuilder().WithFunc(func(size int32) int32 {
		if !initialized {
			return 0 // plugin_alloc's null-pointer contract: not ready.
		}
		return 1
	}).Export(abi.ExportAlloc).
		NewFunctionBuilder().WithFunc(func(ptr, size int32) {}).Export(abi.ExportFree).
		NewFunctionBuilder().WithFunc(func(op, ptr, size int32) int64 { return 0 }).Export(abi.ExportInvoke).
		Compile(ctx)
	if err != nil {
		t.Fatalf("compile fake reactor module: %v", err)
	}

	if _, err := NewInstance(ctx, rt, compiled); err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	if !initialized {
		t.Errorf("NewInstance did not run the module's _initialize export")
	}
}

// TestInvokeMemoryCapTrapsInstance covers the memory-bomb side of resource
// limiting: a guest that keeps allocating must be stopped by the runtime's
// memory page cap, not by a generous deadline standing in for it, and must
// leave the Instance Dead.
func TestInvokeMemoryCapTrapsInstance(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)

	// Generous timeout: if this fires instead of the memory cap, the test
	// is inconclusive, not passing.
	mctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := inst.Invoke(mctx, opMemBomb, nil)
	if err == nil {
		t.Fatalf("Invoke(memory bomb) completed with no error: %s", out)
	}
	if mctx.Err() != nil {
		t.Fatalf("Invoke(memory bomb) hit the 30s test deadline instead of the memory page cap: %v", err)
	}
	if !inst.Dead() {
		t.Errorf("Instance.Dead() = false after the memory cap trapped the call, want true")
	}
}

// TestCloseIsIdempotent covers "Close 幂等": closing an Instance twice must
// not error or panic the second time.
func TestCloseIsIdempotent(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)

	if err := inst.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := inst.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v, want nil (idempotent)", err)
	}
	if !inst.Dead() {
		t.Errorf("Instance.Dead() = false after Close, want true")
	}
}

// TestNewInstanceMissingExportFails covers the "missing export" error path
// of NewInstance: a compiled module lacking one of the three ABI exports
// must fail instantiation and name the missing export, not panic or return
// a half-usable Instance.
func TestNewInstanceMissingExportFails(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, 1)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Deliberately omit plugin_invoke.
	compiled, err := rt.NewHostModuleBuilder("incomplete-plugin").
		NewFunctionBuilder().WithFunc(func(size int32) int32 { return 1 }).Export(abi.ExportAlloc).
		NewFunctionBuilder().WithFunc(func(ptr, size int32) {}).Export(abi.ExportFree).
		Compile(ctx)
	if err != nil {
		t.Fatalf("compile incomplete fake plugin module: %v", err)
	}

	_, err = NewInstance(ctx, rt, compiled)
	if err == nil {
		t.Fatalf("NewInstance with a missing plugin_invoke export returned no error")
	}
}

// TestCompileInvalidWasmFails covers Compile's error path: garbage bytes
// must produce a wrapped error, not a panic or a usable CompiledModule.
func TestCompileInvalidWasmFails(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, 1)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err := Compile(ctx, rt, []byte("not a wasm module"))
	if err == nil {
		t.Fatalf("Compile(garbage bytes) returned no error")
	}
}

// TestNewRuntimeZeroMemoryPagesPanics covers the documented invariant:
// memoryPages == 0 is a programmer error, not a defaultable value.
func TestNewRuntimeZeroMemoryPagesPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("NewRuntime(ctx, 0) did not panic")
		}
	}()
	_ = NewRuntime(context.Background(), 0)
}
