package toolauth

import "testing"

func TestGateableToolsIncludesKnownToolsAndExcludesMeta(t *testing.T) {
	names := GateableToolNames()
	for _, want := range []string{
		"read_file", "search_content", "list_files", "write_file",
		"fetch_url", "delegate_task", "moa_consult", "session_search",
	} {
		if !names[want] {
			t.Errorf("GateableToolNames() missing %q", want)
		}
	}
	for _, meta := range []string{"call_tool", "load_capabilities"} {
		if names[meta] {
			t.Errorf("GateableToolNames() must not list meta-tool %q", meta)
		}
	}
}

func TestGateableToolsAreSortedAndDescribed(t *testing.T) {
	tools := GateableTools()
	if len(tools) == 0 {
		t.Fatal("GateableTools() is empty")
	}
	for i, tl := range tools {
		if tl.Description == "" {
			t.Errorf("GateableTools()[%d] %q has no description", i, tl.Name)
		}
		if i > 0 && tools[i-1].Name >= tl.Name {
			t.Errorf("GateableTools() not sorted at %d: %q >= %q", i, tools[i-1].Name, tl.Name)
		}
	}
}
