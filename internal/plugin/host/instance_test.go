package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
)

// Ops 90-99 are test-only fixture ops implemented by
// testdata/guest-rust/src/lib.rs; see testdata/README.md for the full op
// table. They are not part of the abi package because they exist solely to
// exercise this host wrapper, not the production plugin contract.
const (
	opEcho        int32 = 90 // JSON round trip
	opProbe       int32 = 91 // report the guest's host-observable instrumentation
	opArmSlowFree int32 = 92 // make the next plugin_free call spin
	opBogusResult int32 = 93 // return a result pointer outside linear memory
	opEmptyResult int32 = 94 // return PackResult(0, 0): the contract-legal "no body" outcome
	opUnknown     int32 = 97 // reserved: deliberately not implemented by the guest
	opMemBomb     int32 = 98 // allocate until the memory page cap traps
	opBusyLoop    int32 = 99 // pure-compute infinite loop
)

// testMemoryPages is the page count (64 pages = 4MiB) proven sufficient for
// the fixture guest by the spike; Task 8's memory-cap acceptance test reuses
// the same cap.
const testMemoryPages = 64

// fixtureWasm loads the compiled test guest once per test binary run.
var fixtureWasm = sync.OnceValues(func() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "plugin.wasm"))
})

// newTestFixture builds a fresh Runtime and compiles the fixture guest
// against it, registering cleanup for the runtime. memoryPages controls the
// runtime's memory page cap.
func newTestFixture(t *testing.T, memoryPages uint32) (wazero.Runtime, wazero.CompiledModule) {
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
	return rt, compiled
}

// newTestInstance builds a fresh Runtime, compiles the fixture guest, and
// instantiates it, registering cleanup for both. memoryPages controls the
// runtime's memory page cap.
func newTestInstance(t *testing.T, memoryPages uint32) *Instance {
	t.Helper()

	rt, compiled := newTestFixture(t, memoryPages)

	inst, err := NewInstance(context.Background(), rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	return inst
}

// probeReport is the body the fixture's opProbe returns: the guest-side
// instrumentation the host cannot otherwise see. alloc_calls/free_calls count
// every plugin_alloc/plugin_free entry (including the guest's own allocation
// for a response body), snapshotted before the response for this very op is
// allocated; in_ptr/in_len are the request pointer and length plugin_invoke
// was handed for this call.
type probeReport struct {
	Initialized   bool  `json:"initialized"`
	AllocCalls    int   `json:"alloc_calls"`
	FreeCalls     int   `json:"free_calls"`
	SlowFreeCalls int   `json:"slow_free_calls"`
	InPtr         int64 `json:"in_ptr"`
	InLen         int64 `json:"in_len"`
}

// readProbe invokes opProbe with a nil body — which by contract allocates
// nothing host-side, so it does not perturb the counters it reads — and
// decodes the guest's report.
func readProbe(t *testing.T, inst *Instance) probeReport {
	t.Helper()

	out, err := inst.Invoke(context.Background(), opProbe, nil)
	if err != nil {
		t.Fatalf("Invoke(opProbe): %v", err)
	}
	var got probeReport
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode probe report %s: %v", out, err)
	}
	return got
}

// wasmModuleExporting hand-encodes a minimal, valid WebAssembly binary that
// exports the named subset of the three ABI functions with the ABI's exact
// signatures and declares no memory at all.
//
// It is hand-encoded rather than built with rt.NewHostModuleBuilder because
// wazero documents that "direct use of ExportedFunction is forbidden for host
// modules" (api/wasm.go), which is exactly what NewInstance does to its
// argument, and because a host module has no linear memory — so a host module
// could not distinguish "missing export" from "missing memory" the way these
// negative cases need to.
func wasmModuleExporting(t *testing.T, names ...string) []byte {
	t.Helper()

	// Function index per export name; the three declared functions are laid
	// out in this order, each with its ABI signature.
	funcIdx := map[string]byte{abi.ExportAlloc: 0, abi.ExportFree: 1, abi.ExportInvoke: 2}

	typeSec := []byte{
		0x03,                         // three function types
		0x60, 0x01, 0x7f, 0x01, 0x7f, // (i32) -> i32          : plugin_alloc
		0x60, 0x02, 0x7f, 0x7f, 0x00, // (i32, i32) -> ()      : plugin_free
		0x60, 0x03, 0x7f, 0x7f, 0x7f, 0x01, 0x7e, // (i32,i32,i32) -> i64  : plugin_invoke
	}
	funcSec := []byte{0x03, 0x00, 0x01, 0x02} // three functions, types 0/1/2
	codeSec := []byte{
		0x03,                         // three bodies
		0x04, 0x00, 0x41, 0x01, 0x0b, // no locals; i32.const 1; end (a non-null "allocation")
		0x02, 0x00, 0x0b, // no locals; end
		0x04, 0x00, 0x42, 0x00, 0x0b, // no locals; i64.const 0; end
	}

	exportSec := []byte{byte(len(names))}
	for _, name := range names {
		idx, ok := funcIdx[name]
		if !ok {
			t.Fatalf("wasmModuleExporting: unknown ABI export %q", name)
		}
		exportSec = append(exportSec, byte(len(name)))
		exportSec = append(exportSec, name...)
		exportSec = append(exportSec, 0x00, idx) // externkind func, funcidx
	}

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // "\0asm" + version 1
	for _, sec := range []struct {
		id   byte
		body []byte
	}{
		{1, typeSec}, {3, funcSec}, {7, exportSec}, {10, codeSec}, // section order is mandatory
	} {
		if len(sec.body) > 0x7f {
			t.Fatalf("wasmModuleExporting: section %d body is %d bytes, which needs multi-byte LEB128 encoding", sec.id, len(sec.body))
		}
		out = append(out, sec.id, byte(len(sec.body)))
		out = append(out, sec.body...)
	}
	return out
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
// module. It uses opUnknown, a value inside the fixture's documented
// test-only range that the guest deliberately leaves unimplemented, rather
// than a low literal a later abi op could claim.
func TestInvokeUnknownOpDoesNotCrash(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, opUnknown, nil)
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
	sizeAfterFirst := inst.mem.Size()

	for i := 1; i < n; i++ {
		if _, err := inst.Invoke(ctx, opEcho, []byte(`{"name":"other","n":1}`)); err != nil {
			t.Fatalf("Invoke #%d: %v", i, err)
		}
	}
	sizeFinal := inst.mem.Size()

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
// plugin_alloc": it must call plugin_invoke directly with ptr=0, len=0.
//
// It runs against the real fixture rather than a synthetic host module, using
// the guest's own instrumentation (opProbe reports plugin_alloc entries and
// the ptr/len this very call received). A host module would be the wrong
// stand-in twice over: wazero documents that "direct use of ExportedFunction
// is forbidden for host modules" — which is exactly what NewInstance does to
// its argument — and a host module has no linear memory, which NewInstance
// now rejects outright.
func TestInvokeNilInputSkipsAlloc(t *testing.T) {
	nilInputInst := newTestInstance(t, testMemoryPages)

	got := readProbe(t, nilInputInst)
	if got.AllocCalls != 0 {
		t.Errorf("plugin_alloc was called %d times for a nil input, want 0", got.AllocCalls)
	}
	if got.InPtr != 0 || got.InLen != 0 {
		t.Errorf("plugin_invoke received (ptr,len) = (%d,%d) for a nil input, want (0,0)", got.InPtr, got.InLen)
	}

	// Positive control on the same probe: with a non-empty body the host does
	// allocate and does pass a real pointer, so a probe that reported zeroes
	// unconditionally could not produce this result.
	bodyInst := newTestInstance(t, testMemoryPages)
	body := []byte(`{"ignored":1}`)
	out, err := bodyInst.Invoke(context.Background(), opProbe, body)
	if err != nil {
		t.Fatalf("Invoke(opProbe) with a body: %v", err)
	}
	var withBody probeReport
	if err := json.Unmarshal(out, &withBody); err != nil {
		t.Fatalf("decode probe report %s: %v", out, err)
	}
	if withBody.AllocCalls != 1 {
		t.Errorf("plugin_alloc was called %d times for a %d byte input, want 1", withBody.AllocCalls, len(body))
	}
	if withBody.InPtr == 0 || withBody.InLen != int64(len(body)) {
		t.Errorf("plugin_invoke received (ptr,len) = (%d,%d) for a %d byte input, want (non-zero,%d)",
			withBody.InPtr, withBody.InLen, len(body), len(body))
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

// TestInvokeFreesInputAfterContextCancellation covers fatal detail #2: Invoke
// must free the input allocation with context.WithoutCancel(ctx), not ctx
// unchanged.
//
// The scenario is a context that goes Done after plugin_invoke has already
// succeeded but before Invoke's deferred input free runs. wazero checks
// ctx.Done() synchronously at the top of every call (interpreter.go's and
// call_engine.go's `if ensureTermination { select { case <-ctx.Done(): ...`),
// so freeing with ctx unchanged fails that free outright and closes the whole
// module — turning a harmless race (the caller's context ended around the
// time the call finished) into a dead Instance and a leaked guest allocation.
//
// Rather than racing cancellation against many fast calls and thresholding
// the failure rate, this widens that window deterministically: opArmSlowFree
// makes the guest's *next* plugin_free spin for a calibrated number of
// iterations. Invoke frees the result before the deferred input free, so the
// spin lands on the result free, and the cancellation scheduled during the
// spin is guaranteed to be in effect by the time the input free starts. The
// spin count is calibrated from a first, uncancelled call, so the window
// scales with the machine instead of assuming one; the assertions below then
// prove the window really was entered, so this test cannot pass vacuously.
func TestInvokeFreesInputAfterContextCancellation(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	bg := context.Background()

	// Calibration: how long does a known spin take here? (The guest cannot
	// measure real time — wazero's default ModuleConfig installs a fake
	// nanotime/nanosleep — so the host does the timing.)
	const calibrationIters = 20_000_000
	calibrationStart := time.Now()
	if _, err := inst.Invoke(bg, opArmSlowFree, armBody(calibrationIters)); err != nil {
		t.Fatalf("calibration Invoke(opArmSlowFree): %v", err)
	}
	calibration := time.Since(calibrationStart)
	if calibration <= 0 {
		calibration = time.Millisecond
	}

	// Target a spin far longer than the cancellation delay in both
	// directions: cancellation must land after plugin_invoke returned
	// (microseconds in) and before the spin ends.
	const spinTarget = 900 * time.Millisecond
	const cancelAfter = 150 * time.Millisecond
	iters := int64(float64(calibrationIters) * (float64(spinTarget) / float64(calibration)))
	if iters < calibrationIters {
		iters = calibrationIters
	}
	if iters > 4_000_000_000 {
		iters = 4_000_000_000
	}
	t.Logf("calibration: %d iterations took %v; probing with %d iterations and cancelling after %v",
		calibrationIters, calibration, iters, cancelAfter)

	cctx, cancel := context.WithCancel(bg)
	defer cancel()
	// cancelAt records when the AfterFunc callback actually ran, not just when
	// it was scheduled: on a single-P runner the guest's spin can hold the
	// only P in wazero-generated code, where Go's async preemption finds no
	// safe point, so this goroutine may not run until the spin returns. The
	// test reads cancelAt after Invoke returns, so guard it with a mutex
	// against the AfterFunc goroutine's write.
	var cancelAtMu sync.Mutex
	var cancelAt time.Time
	timer := time.AfterFunc(cancelAfter, func() {
		cancelAtMu.Lock()
		cancelAt = time.Now()
		cancelAtMu.Unlock()
		cancel()
	})
	defer timer.Stop()

	start := time.Now()
	out, err := inst.Invoke(cctx, opArmSlowFree, armBody(iters))
	returnedAt := time.Now()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Invoke returned %v; a context that goes Done after plugin_invoke succeeded must not "+
			"fail the deferred input free — is it calling free with ctx instead of context.WithoutCancel(ctx)?", err)
	}
	if inst.Dead() {
		t.Errorf("Instance.Dead() = true after a successful Invoke whose context was cancelled during cleanup, want false")
	}
	var armed struct {
		Armed     bool  `json:"armed"`
		SpinIters int64 `json:"spin_iters"`
	}
	if err := json.Unmarshal(out, &armed); err != nil {
		t.Fatalf("decode result %s: %v", out, err)
	}
	if !armed.Armed || armed.SpinIters != iters {
		t.Errorf("Invoke(opArmSlowFree) = %s, want {\"armed\":true,\"spin_iters\":%d}", out, iters)
	}

	// Positive controls: without these the test could pass while never
	// entering the window it exists to probe.
	if cctx.Err() == nil {
		t.Errorf("the probe context never reached Done, so the input free never ran under a cancelled context")
	}
	if elapsed < cancelAfter+100*time.Millisecond {
		t.Errorf("Invoke took only %v but cancellation was scheduled at %v: the guest's slow plugin_free did not "+
			"outlast the cancellation, so the input free is not proven to have run after ctx was Done", elapsed, cancelAfter)
	}
	// The two controls above establish wall-clock ordering (Invoke ran long
	// enough, and cctx ended up Done), but not causal ordering: both are
	// satisfiable even if cancel() had not actually run before the deferred
	// input free began — e.g. on a single-P runner where the AfterFunc
	// goroutine only gets scheduled once the guest's spin returns. Asserting
	// cancelAt is meaningfully before Invoke's return turns this from an
	// inference into a direct check that cancellation landed with margin to
	// spare before cleanup ran.
	cancelAtMu.Lock()
	gotCancelAt := cancelAt
	cancelAtMu.Unlock()
	if gotCancelAt.IsZero() {
		t.Errorf("cancel() never ran (the AfterFunc callback did not fire) before Invoke returned")
	} else if margin := returnedAt.Sub(gotCancelAt); margin < 100*time.Millisecond {
		t.Errorf("cancel() ran only %v before Invoke returned (returnedAt=%v, cancelAt=%v); want at least 100ms of "+
			"margin, so cancellation is not proven to have run before the deferred input free began", margin, returnedAt, gotCancelAt)
	}
	// Two spins: the calibration call and the probe call.
	if got := readProbe(t, inst); got.SlowFreeCalls != 2 {
		t.Errorf("guest reports slow_free_calls = %d, want 2: the deliberately slow plugin_free that widens "+
			"the invoke→free window did not run", got.SlowFreeCalls)
	}
}

// armBody builds opArmSlowFree's request body.
func armBody(iters int64) []byte {
	return []byte(fmt.Sprintf(`{"spin_iters":%d}`, iters))
}

// TestNewInstanceRunsInitializeStartFunction covers fatal detail #3:
// NewInstance instantiates with WithStartFunctions("_initialize") because
// WASI reactor modules export no _start, so the default start-function
// configuration (["_start"]) would silently do nothing.
//
// The fixture guest is a real WASI reactor: it exports _initialize (which
// sets a flag opProbe reports) and no _start, so dropping
// WithStartFunctions("_initialize") leaves the flag false against the real
// guest — no synthetic host module needed.
func TestNewInstanceRunsInitializeStartFunction(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)

	if got := readProbe(t, inst); !got.Initialized {
		t.Errorf("the guest reports initialized = false: NewInstance must instantiate with "+
			"WithStartFunctions(%q), otherwise wazero looks for _start, which a reactor module does not export", "_initialize")
	}
}

// TestFixtureExportsAndImportsArePinned pins the committed fixture's contract
// so that rebuilding testdata/plugin.wasm (possibly with a different rustc)
// cannot silently move the ground other tests stand on: _initialize must be
// exported (fatal detail #3), _start must not be, the three ABI functions
// must be exported, a linear memory must be exported (NewInstance requires
// one), and nothing may be imported from the host module — Task 2's runtime
// registers no host functions, so a guest importing them could not
// instantiate at all.
func TestFixtureExportsAndImportsArePinned(t *testing.T) {
	_, compiled := newTestFixture(t, testMemoryPages)

	gotExports := make([]string, 0, len(compiled.ExportedFunctions()))
	for name := range compiled.ExportedFunctions() {
		gotExports = append(gotExports, name)
	}
	sort.Strings(gotExports)
	wantExports := []string{"_initialize", abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke}
	sort.Strings(wantExports)
	if strings.Join(gotExports, ",") != strings.Join(wantExports, ",") {
		t.Errorf("fixture exports %v, want exactly %v", gotExports, wantExports)
	}

	gotMemories := make([]string, 0, len(compiled.ExportedMemories()))
	for name := range compiled.ExportedMemories() {
		gotMemories = append(gotMemories, name)
	}
	if len(gotMemories) != 1 {
		t.Errorf("fixture exports %v memories, want exactly one (NewInstance requires the guest's linear memory)", gotMemories)
	}

	// The imported *function list* is rustc's business and may change; the
	// module it comes from is the contract.
	for _, def := range compiled.ImportedFunctions() {
		module, name, _ := def.Import()
		if module != "wasi_snapshot_preview1" {
			t.Errorf("fixture imports %s.%s; Task 2's runtime provides only wasi_snapshot_preview1 "+
				"(host functions start at Task 3)", module, name)
		}
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

// TestNewInstanceMissingExportFails covers the "missing export" error path of
// NewInstance for each of the three ABI exports: instantiation must fail and
// the error must name the export that is missing, so a copy-paste swap
// between the three branches cannot hide.
func TestNewInstanceMissingExportFails(t *testing.T) {
	all := []string{abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke}

	for _, missing := range all {
		t.Run("missing_"+missing, func(t *testing.T) {
			present := make([]string, 0, len(all)-1)
			for _, name := range all {
				if name != missing {
					present = append(present, name)
				}
			}

			ctx := context.Background()
			rt := NewRuntime(ctx, 1)
			t.Cleanup(func() { _ = rt.Close(context.Background()) })

			compiled, err := Compile(ctx, rt, wasmModuleExporting(t, present...))
			if err != nil {
				t.Fatalf("Compile incomplete module: %v", err)
			}

			_, err = NewInstance(ctx, rt, compiled)
			if err == nil {
				t.Fatalf("NewInstance with a missing %s export returned no error", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("NewInstance error = %v, want it to name the missing export %q", err, missing)
			}
			if !strings.Contains(err.Error(), "missing export") {
				t.Errorf("NewInstance error = %v, want it to say which kind of thing is missing", err)
			}
			// The module also has no memory; the export check must be the one
			// that fires, because it is the more specific fault.
			for _, name := range present {
				if strings.Contains(err.Error(), name) {
					t.Errorf("NewInstance error = %v, want it to name only the missing export, not %q", err, name)
				}
			}
		})
	}
}

// TestNewInstanceWithoutMemoryFails covers NewInstance's memory requirement:
// a guest exporting all three ABI functions but no linear memory must be
// rejected with an error, because wazero's api.Module.Memory returns a nil
// *MemoryInstance boxed in a non-nil interface for such a module and the
// first Read/Write on it would panic the host process instead.
func TestNewInstanceWithoutMemoryFails(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, 1)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	compiled, err := Compile(ctx, rt, wasmModuleExporting(t, abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke))
	if err != nil {
		t.Fatalf("Compile memory-less module: %v", err)
	}

	inst, err := NewInstance(ctx, rt, compiled)
	if err == nil {
		t.Fatalf("NewInstance with a memory-less guest returned no error (Instance = %+v)", inst)
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("NewInstance error = %v, want it to name the missing linear memory", err)
	}
}

// TestDeadReportsAModuleClosedByWazero covers Dead's second case: wazero can
// close the module without any Invoke call reporting an error (a runtime
// built with WithCloseOnContextDone closes it when the caller's context
// completes), and Dead must report that, because Task 5's pool decides
// whether to hand an Instance out on the strength of Dead() == false.
func TestDeadReportsAModuleClosedByWazero(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	if inst.Dead() {
		t.Fatalf("freshly instantiated Instance.Dead() = true, want false")
	}
	// Close the module behind the Instance's back, exactly as wazero does on
	// a context completion that no call turned into an error.
	if err := inst.mod.Close(ctx); err != nil {
		t.Fatalf("close the module directly: %v", err)
	}

	if !inst.Dead() {
		t.Errorf("Instance.Dead() = false for a module wazero has already closed, want true")
	}
}

// TestCloseReleasesResourcesOfADeadInstance covers Close's "always call
// through to wazero" rule: a guest trap marks the Instance dead but leaves
// the wazero module wide open, so an early return on the dead flag would
// leak the module's resources for exactly the instances a pool discards most
// often. The observable proof is wazero's experimental CloseNotifier, which
// fires from the same resource-release path Close must reach.
func TestCloseReleasesResourcesOfADeadInstance(t *testing.T) {
	bg := context.Background()
	rt, compiled := newTestFixture(t, testMemoryPages)

	var closeNotifications atomic.Int32
	notifyCtx := experimental.WithCloseNotifier(bg, experimental.CloseNotifyFunc(
		func(context.Context, uint32) { closeNotifications.Add(1) }))

	inst, err := NewInstance(notifyCtx, rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}

	// Generous timeout: if this fires instead of the memory cap, the test is
	// inconclusive, not passing.
	mctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if _, err := inst.Invoke(mctx, opMemBomb, nil); err == nil {
		t.Fatalf("Invoke(memory bomb) completed with no error")
	}
	if mctx.Err() != nil {
		t.Fatalf("Invoke(memory bomb) hit the 30s test deadline instead of the memory page cap")
	}
	if !inst.Dead() {
		t.Fatalf("Instance.Dead() = false after a guest trap, want true")
	}
	if inst.mod.IsClosed() {
		t.Fatalf("wazero closed the module on a guest trap, so this test no longer builds the " +
			"dead-but-open instance it needs; find another way to produce one")
	}
	if got := closeNotifications.Load(); got != 0 {
		t.Fatalf("module resources were released before Close (CloseNotifier fired %d times)", got)
	}

	if err := inst.Close(bg); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inst.mod.IsClosed() {
		t.Errorf("Close left the module of an already-dead Instance open")
	}
	if got := closeNotifications.Load(); got != 1 {
		t.Errorf("CloseNotifier fired %d times after Close, want 1: Close did not release wazero's module resources", got)
	}
	// Still idempotent, now against wazero's own idempotency.
	if err := inst.Close(bg); err != nil {
		t.Errorf("second Close: %v, want nil (idempotent)", err)
	}
	if got := closeNotifications.Load(); got != 1 {
		t.Errorf("CloseNotifier fired %d times after a second Close, want 1", got)
	}
}

// TestInvokeUnreadableResultKillsInstance covers the result-read error path:
// a guest returning a result pointer outside its own linear memory must
// produce an error, must have its (bogus) result region handed back to
// plugin_free rather than leaked, and must leave the Instance Dead so a pool
// cannot keep serving from a guest whose result pointers are wrong.
func TestInvokeUnreadableResultKillsInstance(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, opBogusResult, nil)
	if err == nil {
		t.Fatalf("Invoke(opBogusResult) returned no error (result %q)", out)
	}
	if !strings.Contains(err.Error(), "read result") {
		t.Errorf("Invoke error = %v, want it to name the failed result read", err)
	}
	if !inst.Dead() {
		t.Errorf("Instance.Dead() = false after the guest returned an out-of-range result pointer, want true")
	}

	// The module itself is still open (the guest misbehaved; it did not
	// trap), so the guest can still be asked what the host did to it: the
	// result region must have been passed to plugin_free.
	if got := readProbe(t, inst); got.FreeCalls != 1 {
		t.Errorf("guest reports free_calls = %d, want 1: the unreadable result allocation was not freed", got.FreeCalls)
	}
}

// TestInvokeEmptyResultReturnsNilNil covers Invoke's outLen == 0 branch
// (instance.go's "return nil, nil"): abi.PackResult(0, 0) is a
// contract-legal "no return body" outcome, not an error, and a call
// producing it must not disturb the Instance.
func TestInvokeEmptyResultReturnsNilNil(t *testing.T) {
	inst := newTestInstance(t, testMemoryPages)
	ctx := context.Background()

	out, err := inst.Invoke(ctx, opEmptyResult, nil)
	if err != nil {
		t.Fatalf("Invoke(opEmptyResult): %v", err)
	}
	if out != nil {
		t.Errorf("Invoke(opEmptyResult) = %q, want nil", out)
	}
	if inst.Dead() {
		t.Errorf("Instance.Dead() = true after an empty-result call, want false")
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
