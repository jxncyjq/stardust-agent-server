package legionplugin

import (
	"encoding/json"
	"fmt"
	"sort"
)

// registry is what Serve fills in and the ABI exports dispatch through.
//
// It is package state rather than a value the author threads around because
// the ABI exports are package-level functions the host calls directly: there
// is nowhere to hand an object through. One plugin, one module, one registry.
var registry = struct {
	name    string
	version string
	tools   map[string]Handler
	// observer is the ONE observer Observe may register (see observe.go). It
	// is nil for the ordinary plugin, which contributes tools and watches
	// nothing.
	observer Observer
	// decider is the ONE decider Decide may register (see decide.go). It is
	// nil for the ordinary plugin, which contributes tools and refuses
	// nothing.
	decider Decider
}{tools: map[string]Handler{}}

// Serve records this plugin's identity and its tools. Call it exactly once,
// from init — see the package doc for why not from main.
//
// It PANICS on the three things that would otherwise produce a plugin that
// mounts and then misbehaves: no name, no tools, or two tools sharing a name.
// A panic during _initialize fails instantiation, which the host reports with
// the plugin named; the alternative — mounting a plugin whose registry does
// not match its self-description — fails later, further away, and is much
// harder to explain.
func Serve(name, version string, tools ...Tool) {
	if name == "" {
		panic("legionplugin: Serve: name is empty; the deployment entry is keyed by it")
	}
	if len(tools) == 0 {
		panic("legionplugin: Serve: no tools; a plugin exists to contribute tools")
	}
	registry.name = name
	registry.version = version
	for _, tool := range tools {
		if tool.Name == "" {
			panic("legionplugin: Serve: a tool has no name")
		}
		if tool.Handler == nil {
			panic(fmt.Sprintf("legionplugin: Serve: tool %q has a nil handler", tool.Name))
		}
		if _, taken := registry.tools[tool.Name]; taken {
			panic(fmt.Sprintf("legionplugin: Serve: tool %q registered twice; the host's registry "+
				"refuses duplicate names too, later and less clearly", tool.Name))
		}
		registry.tools[tool.Name] = tool.Handler
	}
}

// manifestBody renders op 0's answer from what Serve registered.
//
// The tool list is sorted so the document is byte-stable across builds: an
// unstable self-description would make a plugin package's digest change for no
// reason, and digests are what a deployment pins.
func manifestBody() []byte {
	provides := make([]string, 0, len(registry.tools))
	for name := range registry.tools {
		provides = append(provides, name)
	}
	sort.Strings(provides)

	body, err := json.Marshal(manifestDoc{
		Name:       registry.name,
		Version:    registry.version,
		Provides:   provides,
		Extensions: registeredExtensions(),
	})
	if err != nil {
		// Unreachable for these field types (strings and a string slice), and
		// still not papered over: answering op 0 with an empty body would make
		// activation's cross-check refuse the plugin with a message about a
		// missing tool rather than about this.
		panic(fmt.Sprintf("legionplugin: encode manifest: %v", err))
	}
	return body
}

// dispatch is op 1's body: decode the call, find the tool, answer.
//
// Every failure here is a RESULT, never a panic. A panic would trap the module
// and cost every other call in flight on this instance; a failed result is an
// answer the model can read.
func dispatch(request []byte) []byte {
	call, err := parseToolCall(request)
	if err != nil {
		return mustEncode(Fail(err.Error()))
	}
	handler, ok := registry.tools[call.Tool]
	if !ok {
		// The host only dispatches tools it registered, so this is a contract
		// violation rather than a routine miss — but it still gets an answer
		// instead of a trap.
		return mustEncode(Fail("unknown tool: " + call.Tool))
	}
	return mustEncode(handler(call))
}

// mustEncode renders a result, falling back to a hand-built failure document
// if even that fails.
//
// This is NOT swallowing an error: the fallback is itself a failed result that
// names the encoding problem, so the model is told the call failed. Returning
// nothing would reach the host as "no body", which it reports as an ABI
// violation — true, but it would name the SDK's encoder as a guest bug without
// saying which one.
func mustEncode(result ToolResult) []byte {
	body, err := result.encode()
	if err != nil {
		return []byte(`{"call_id":"","success":false,"output":"","error":"legionplugin: could not encode the tool result"}`)
	}
	return body
}
