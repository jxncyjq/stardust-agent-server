package legionplugin

// These tests cover the parts of the SDK that do not need a wasm runtime:
// decoding a call and encoding a result. The ABI itself — exports, memory,
// dispatch, and the GC hazard — is covered by guest_test.go against the real
// host, because nothing running on the host platform can prove any of that.

import (
	"strings"
	"testing"
)

func TestToolCallParsesArguments(t *testing.T) {
	call, err := parseToolCall([]byte(`{"call_id":"c1","tool":"hello","arguments":{"name":"legion"}}`))
	if err != nil {
		t.Fatalf("parseToolCall: %v", err)
	}
	if call.CallID != "c1" || call.Tool != "hello" || call.Argument("name") != "legion" {
		t.Errorf("parsed %+v, want call_id=c1 tool=hello name=legion", call)
	}
}

func TestToolCallWithoutAToolNameIsAnError(t *testing.T) {
	if _, err := parseToolCall([]byte(`{"call_id":"c1","arguments":{}}`)); err == nil {
		t.Fatal("parseToolCall with no tool = nil error, want a refusal: an empty tool name " +
			"would be dispatched as \"unknown tool\", which blames the caller for the SDK's silence")
	}
}

// TestToolCallIgnoresFieldsThisSDKDoesNotConsume protects every already-built
// plugin from the day the host adds an optional field: refusing unknown fields
// would turn that addition into a fault counted against each plugin's health.
func TestToolCallIgnoresFieldsThisSDKDoesNotConsume(t *testing.T) {
	call, err := parseToolCall([]byte(`{"tool":"hello","deadline_ms":3000,"arguments":{"a":"b"}}`))
	if err != nil {
		t.Fatalf("parseToolCall with an unknown field: %v", err)
	}
	if call.Argument("a") != "b" {
		t.Errorf("parsed %+v, want a=b", call)
	}
}

func TestArgumentOfAMissingKeyIsEmpty(t *testing.T) {
	call, err := parseToolCall([]byte(`{"tool":"hello","arguments":{}}`))
	if err != nil {
		t.Fatalf("parseToolCall: %v", err)
	}
	if got := call.Argument("name"); got != "" {
		t.Errorf("Argument on a missing key = %q, want \"\"", got)
	}
}

func TestFailResultCarriesTheMessage(t *testing.T) {
	body, err := Fail("missing required argument: name").encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"success":false`) ||
		!strings.Contains(string(body), "missing required argument: name") {
		t.Errorf("encoded %s, want success:false and the message", body)
	}
}

// TestOKResultLeavesCallIDEmpty pins that the SDK does not invent a
// correlation id: the host owns it and overwrites whatever the guest returns,
// so a guest-chosen id would only mislead whoever reads the guest's own logs.
func TestOKResultLeavesCallIDEmpty(t *testing.T) {
	body, err := OK("fine").encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"call_id":""`) {
		t.Errorf("encoded %s, want an empty call_id", body)
	}
}

// TestManifestBodyListsRegisteredToolsSorted pins both halves of the property
// the SDK exists for: the self-description comes from the registry, and it is
// byte-stable across builds (an unstable one would change a package's digest
// for no reason, and digests are what a deployment pins).
func TestManifestBodyListsRegisteredToolsSorted(t *testing.T) {
	restore := registry
	t.Cleanup(func() { registry = restore })

	registry.name = "p"
	registry.version = "1.0.0"
	registry.tools = map[string]Handler{
		"zeta":  func(ToolCall) ToolResult { return OK("") },
		"alpha": func(ToolCall) ToolResult { return OK("") },
	}

	if got := string(manifestBody()); got != `{"name":"p","version":"1.0.0","provides":["alpha","zeta"]}` {
		t.Errorf("manifestBody() = %s, want the tools sorted", got)
	}
}

// TestDispatchAnswersAnUnknownToolInsteadOfPanicking: the host only dispatches
// tools it registered, so this is a contract violation — and it still gets an
// answer, because a panic here would trap the module and take every other call
// in flight with it.
func TestDispatchAnswersAnUnknownToolInsteadOfPanicking(t *testing.T) {
	restore := registry
	t.Cleanup(func() { registry = restore })
	registry.tools = map[string]Handler{}

	body := dispatch([]byte(`{"tool":"nobody_registered_this","arguments":{}}`))
	if !strings.Contains(string(body), "unknown tool: nobody_registered_this") {
		t.Errorf("dispatch answered %s, want it to name the unknown tool", body)
	}
}

func TestServeRefusesTwoToolsWithTheSameName(t *testing.T) {
	restore := registry
	t.Cleanup(func() { registry = restore })
	registry.tools = map[string]Handler{}

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Error("Serve with a duplicate tool name did not panic; the host's registry refuses " +
				"duplicates too, later and less clearly")
		}
	}()
	handler := func(ToolCall) ToolResult { return OK("") }
	Serve("p", "1.0.0", Tool{Name: "same", Handler: handler}, Tool{Name: "same", Handler: handler})
}
