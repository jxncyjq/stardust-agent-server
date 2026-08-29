//go:build wasip1

package legionplugin

import (
	"sync"
	"unsafe"
)

// This file is the whole ABI surface: the four exports the host requires, plus
// the memory bookkeeping a Go guest needs and a Rust one does not.
//
// It builds only for wasip1. Everything else in this package compiles on the
// host platform too, which is what lets the SDK's own logic be unit-tested
// without a wasm runtime.

// live keeps every buffer plugin_alloc handed out alive until plugin_free
// releases it.
//
// This is the one failure mode a Go guest has and a Rust guest does not: after
// plugin_alloc returns, the only reference the host holds is an INTEGER
// ADDRESS, which the Go garbage collector does not see. Without this table the
// collector is free to reclaim the buffer between the allocation and the
// host's write, and the host would then write into recycled memory — silently,
// under load, in production. TestGoGuestSurvivesAGarbageCollection pins it.
var (
	liveMu sync.Mutex
	live   = map[uintptr][]byte{}
)

//go:wasmexport plugin_alloc
func pluginAlloc(size int32) int32 {
	if size <= 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	liveMu.Lock()
	live[ptr] = buf
	liveMu.Unlock()
	return int32(ptr)
}

//go:wasmexport plugin_free
func pluginFree(ptr int32, size int32) {
	if ptr <= 0 {
		return
	}
	liveMu.Lock()
	delete(live, uintptr(ptr))
	liveMu.Unlock()
}

//go:wasmexport plugin_invoke
func pluginInvoke(op int32, ptr int32, size int32) int64 {
	switch op {
	case opManifest:
		return writeOut(manifestBody())
	case opCallTool:
		return writeOut(dispatch(readIn(ptr, size)))
	case opObserveToolResult:
		return writeOut(observe(readIn(ptr, size)))
	case opDecideToolCall:
		return writeOut(decide(readIn(ptr, size)))
	default:
		// Never trap on an unknown op: a host that has moved on to an ABI
		// version this guest does not know should get an answer, not a dead
		// module.
		return writeOut([]byte(`{"error":"unsupported op"}`))
	}
}

// The ops of ABI v1, mirroring internal/plugin/abi's constants. They are
// spelled here rather than imported because a guest is compiled on its own,
// often outside this repository.
//
// opObserveToolResult only ever arrives when the deployment granted this
// plugin the "observe" extension: the host registers no observer without the
// grant, so an ungranted plugin sees this op zero times rather than seeing it
// and refusing.
const (
	opManifest          int32 = 0
	opCallTool          int32 = 1
	opObserveToolResult int32 = 2
	opDecideToolCall    int32 = 3
)

// writeOut copies body into freshly allocated guest memory and packs the
// result the ABI way: the high 32 bits are the pointer, the low 32 bits the
// length. The host reads that region and frees it through plugin_free.
func writeOut(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	ptr := pluginAlloc(int32(len(body)))
	if ptr == 0 {
		return 0
	}
	copy(guestSlice(ptr, int32(len(body))), body)
	return int64(ptr)<<32 | int64(len(body))
}

// readIn copies the request body the host wrote at (ptr, size).
//
// It COPIES rather than aliasing: the host frees that region as soon as this
// call returns, and a handler that kept a slice into it would be reading freed
// memory. An empty request is legal — the host passes (0, 0) and never calls
// plugin_alloc — so it yields nil rather than trapping.
func readIn(ptr, size int32) []byte {
	if ptr <= 0 || size <= 0 {
		return nil
	}
	body := make([]byte, size)
	copy(body, guestSlice(ptr, size))
	return body
}

// guestSlice views a region of this module's own linear memory as a slice.
//
// Turning an integer address back into a pointer is exactly what the ABI is:
// the host and the guest exchange offsets into one linear memory, and every
// address crossing that boundary arrives as an i32. On wasm32 an address IS
// the offset, so this conversion is well defined — but it is written once,
// here, so the unsafe reasoning lives in one place instead of at each use.
//
// The caller must only use the result while the region is alive: for a buffer
// from pluginAlloc that means until pluginFree, and for a host-written request
// body that means within the plugin_invoke call it arrived on.
func guestSlice(ptr, size int32) []byte {
	//nolint:govet // integer-to-pointer is the ABI; see the doc comment above.
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), size)
}

// uintptrOf returns the address of a byte slice's first element, for handing
// to a host function. The slice must be non-empty and must stay reachable for
// the whole call — the host reads it synchronously, so a local variable in the
// caller is enough.
func uintptrOf(body []byte) uintptr {
	return uintptr(unsafe.Pointer(&body[0]))
}

// LiveBuffers reports how many host-allocated buffers this module is holding
// alive right now — the size of the table described above.
//
// It is a diagnostic, and the one place the GC guard is observable from
// outside: during any call there is at least the host's request body, and a
// count that stayed at zero would mean the guard is not registering anything
// (see TestGoGuestKeepsHostBuffersAlive). A plugin author can also use it to
// spot a leak: a count that only grows across calls means something is not
// being freed.
func LiveBuffers() int {
	liveMu.Lock()
	defer liveMu.Unlock()
	return len(live)
}
