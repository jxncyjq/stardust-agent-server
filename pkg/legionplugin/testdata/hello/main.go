// Command hello is the Go SDK's example guest: the smallest plugin that closes
// the loop — it reads an argument, calls back into the host through the log
// capability, and answers with a ToolResult.
//
// It is built by guest_test.go rather than committed, because a Go guest is
// ~1.9 MB and a committed artifact would go stale exactly when this test needs
// to be honest: the moment the SDK changes.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o hello.wasm .
package main

import (
	"strconv"

	"github.com/stardust/legion-agent/pkg/legionplugin"
)

// init, not main: a plugin is a WASI reactor. The host instantiates it with
// WithStartFunctions("_initialize") — which runs package initialization — and
// then calls the exports. Nothing ever calls main.
func init() {
	legionplugin.Serve("legion-hello-go", "0.1.0",
		legionplugin.Tool{Name: "hello_echo", Handler: helloEcho},
		// live_buffers exists for the SDK's own test: it reports the size of
		// the table that keeps host-allocated buffers alive against Go's GC.
		// A real plugin would not ship it — but a plugin author debugging a
		// leak might add exactly this.
		legionplugin.Tool{Name: "live_buffers", Handler: liveBuffers},
	)
}

// helloEcho greets by name, logging through the host on the way.
//
// A missing argument is a FAILED RESULT that names what was missing, never a
// panic: a panic traps the whole module, costing the instance's state and
// every call in flight on it, and counts as a fault against the plugin's
// health. Quietly greeting nobody would be worse still — a plugin that always
// "succeeds" while doing nothing.
func helloEcho(call legionplugin.ToolCall) legionplugin.ToolResult {
	name := call.Argument("name")
	if name == "" {
		return legionplugin.Fail("missing required argument: name")
	}
	legionplugin.LogInfo("hello_echo called with name=" + name)
	return legionplugin.OK("hello, " + name + "!")
}

// liveBuffers answers with the number of host-allocated buffers this module is
// currently holding alive. During any call that is at least one: the request
// body the host wrote through plugin_alloc.
func liveBuffers(legionplugin.ToolCall) legionplugin.ToolResult {
	return legionplugin.OK(strconv.Itoa(legionplugin.LiveBuffers()))
}

func main() {}
