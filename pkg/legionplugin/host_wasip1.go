//go:build wasip1

package legionplugin

// Host functions live here, and only `log` is imported unconditionally.
//
// # Why the other six are behind build tags
//
// Capabilities are a LINK-TIME fact, not a runtime switch: the host registers
// a capability's host functions only when that capability is granted, so
// importing one the deployment did not grant makes the module fail to
// INSTANTIATE — the guest never gets the chance to call it and never sees a
// DENIED to handle.
//
// So "what this module imports" must match plugin.json's `capabilities`
// exactly, and that is the easiest thing in the world to forget in source. The
// build tags make opening a capability an explicit, three-part act:
//
//  1. build with the tag: `go build -tags legion_kv …`;
//  2. add the capability name to plugin.json's `capabilities`;
//  3. include it in `agent plugins grant --capabilities …` on the deployment.
//
// Miss step 2 and the deployment cannot grant it → instantiation fails. Miss
// step 3 and the grant is refused outright (it must name the plugin's declared
// set exactly).
//
// Request and response shapes are the same on both SDKs; see
// sdk/rust/legion-plugin/src/host.rs for the table, or the reference manual's
// §3.3.

//go:wasmimport legion log
func hostLog(level int32, ptr int32, size int32)

// Host log levels: 0=debug 1=info 2=warn 3=error. An unrecognised level is not
// rounded down to info by the host — it is recorded at error and labelled, so
// a miscalling plugin shows up instead of hiding behind a plausible default.
const (
	levelDebug int32 = 0
	levelInfo  int32 = 1
	levelWarn  int32 = 2
	levelError int32 = 3
)

// LogInfo writes one line to the host log. It is the visible proof that a
// capability grant reached the guest: without `log` granted, this module would
// not have linked.
func LogInfo(message string) { hostLogString(levelInfo, message) }

// LogWarn writes one line at warn level.
func LogWarn(message string) { hostLogString(levelWarn, message) }

// LogError writes one line at error level.
func LogError(message string) { hostLogString(levelError, message) }

// LogDebug writes one line at debug level.
func LogDebug(message string) { hostLogString(levelDebug, message) }

// hostLogString pins the message bytes for the duration of the call.
//
// The host reads guest memory synchronously inside hostLog, so a []byte
// derived from the string stays reachable across it — but the conversion is
// kept in one place so the lifetime argument only has to be made once.
func hostLogString(level int32, message string) {
	if message == "" {
		// The host records an empty message as an error rather than emitting a
		// blank line, so sending one only produces noise blamed on this plugin.
		return
	}
	body := []byte(message)
	hostLog(level, int32(uintptrOf(body)), int32(len(body)))
}
