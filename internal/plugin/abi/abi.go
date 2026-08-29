// Package abi defines the wire contract between the Legion host and a WASM
// plugin guest module: the export names the guest must implement, the host
// import module name, the encoding of plugin_invoke's packed return value,
// and the operation codes carried in that call.
package abi

const (
	// ExportAlloc is the name of the guest-exported function
	// "plugin_alloc(size i32) -> i32" that the host calls to reserve size
	// bytes inside the guest's linear memory and receive back the pointer
	// to the reserved region.
	ExportAlloc = "plugin_alloc"

	// ExportFree is the name of the guest-exported function
	// "plugin_free(ptr i32, size i32)" that the host calls to release a
	// region of the guest's linear memory previously returned by
	// plugin_alloc.
	ExportFree = "plugin_free"

	// ExportInvoke is the name of the guest-exported function
	// "plugin_invoke(op i32, ptr i32, len i32) -> i64" that the host calls
	// to dispatch an operation (see the Op* constants) against a request
	// body already written into the guest's linear memory at ptr..ptr+len.
	// The returned i64 is encoded per PackResult.
	ExportInvoke = "plugin_invoke"
)

// HostModuleName is the wasm import module name under which the host
// exposes its functions to a guest plugin module.
const HostModuleName = "legion"

const (
	// OpManifest is the plugin_invoke operation the host uses to read the
	// plugin's self-description. The host invokes it with a nil body; the
	// guest responds with a JSON document shaped
	// {"name","version","provides":[…]}. There is no separate
	// "plugin_manifest" export — self-description is this op.
	OpManifest int32 = 0

	// OpCallTool is the plugin_invoke operation the host uses to ask the
	// guest to execute one of the tools the plugin contributes.
	OpCallTool int32 = 1

	// OpObserveToolResult is the host telling a guest, read-only, that a tool
	// call answered. The body is a JSON observation (host.guestToolObservation:
	// the call and its result together); the guest's answer is READ AND
	// DISCARDED.
	//
	// It is the first op the HOST initiates for reasons other than running the
	// guest's own tool, which is why the guest may simply not implement it: an
	// unknown op answers with a small error body by convention, and the host
	// discards that like any other answer.
	OpObserveToolResult int32 = 2

	// OpDecideToolCall is the host asking a guest whether a tool call may be
	// dispatched. The body is a JSON call (host.guestToolDecisionRequest, the
	// same shape as OpCallTool's) and the answer is
	// {"decision":"allow"|"deny","reason":…}.
	//
	// Unlike OpObserveToolResult the answer MATTERS, and the failure mode is
	// therefore the opposite one: an answer the host cannot read is a
	// REFUSAL, not a shrug (see host.pluginDecider for the fail-closed
	// argument). A guest that does not implement this op must not be granted
	// the decide extension — activation cross-checks exactly that.
	OpDecideToolCall int32 = 3
)

// PackResult encodes a pointer and length into the i64 return value of
// plugin_invoke: the high 32 bits hold ptr, the low 32 bits hold length.
// A length of 0 means "no return body" and is not an error condition;
// PackResult(0, 0) is the zero value.
func PackResult(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// UnpackResult decodes a plugin_invoke i64 return value produced by
// PackResult back into its pointer and length halves.
func UnpackResult(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed)
}
