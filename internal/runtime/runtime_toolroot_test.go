package runtime

import "testing"

func TestNewRuntimeStoresToolRoot(t *testing.T) {
	rt := NewRuntime(Config{Gate: NewTaskGate(), ToolRoot: "/tmp/sandbox"})
	if rt.toolRoot != "/tmp/sandbox" {
		t.Fatalf("toolRoot = %q, want /tmp/sandbox", rt.toolRoot)
	}
}

func TestNewRuntimeEmptyToolRootOK(t *testing.T) {
	rt := NewRuntime(Config{Gate: NewTaskGate()})
	if rt.toolRoot != "" {
		t.Fatalf("empty ToolRoot must stay empty (no sandbox → no cache), got %q", rt.toolRoot)
	}
}
