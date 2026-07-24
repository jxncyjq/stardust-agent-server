package serve_test

import (
	"testing"

	"github.com/stardust/legion-agent/serve"
)

// GateableTools is the seam legionAgentGUI (a separate module that cannot import
// internal/toolauth) reads to render its per-agent tool-authorization checklist,
// so it must carry names and descriptions and exclude the always-resident
// meta-tools.
func TestGateableToolsExposesNamedToolsWithoutMeta(t *testing.T) {
	tools := serve.GateableTools()

	if len(tools) == 0 {
		t.Fatal("GateableTools() returned nothing")
	}
	var sawWrite bool
	for i, tl := range tools {
		if tl.Name == "" || tl.Description == "" {
			t.Errorf("GateableTools()[%d] missing name/description: %+v", i, tl)
		}
		if tl.Name == "write_file" {
			sawWrite = true
		}
		if tl.Name == "call_tool" || tl.Name == "load_capabilities" {
			t.Errorf("GateableTools() leaked meta-tool %q", tl.Name)
		}
	}
	if !sawWrite {
		t.Error("GateableTools() missing write_file")
	}
}
