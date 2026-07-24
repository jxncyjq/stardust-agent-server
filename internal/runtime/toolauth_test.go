package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// A disabled tool must not appear in the offered native schema (eager) — the
// single effectiveTools choke point covers offer, catalog and dispatch at once.
func TestEffectiveToolsRemovesDisabledTool(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{
		Maas:          maas,
		Tools:         unchangingReadRegistry(t), // has read_file
		DisabledTools: []string{"read_file"},
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	for _, tl := range maas.requests[0].Tools {
		if tl.Name == "read_file" {
			t.Fatal("disabled read_file was still offered to the model")
		}
	}
}

// Dispatch is authoritative: even if the model names a disabled tool, executing
// it must fail loud rather than run.
func TestDispatchRejectsDisabledTool(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{
		{ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]string{"path": "x"}}}},
		{Text: "done"},
	}}
	rt := NewRuntime(Config{
		Maas:          maas,
		Tools:         unchangingReadRegistry(t),
		DisabledTools: []string{"read_file"},
	})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	// The tool round's result must be a failure carrying ErrToolNotFound, fed
	// back to the model as a tool turn (not executed).
	var sawRejection bool
	for _, req := range maas.requests {
		for _, msg := range req.Messages {
			if msg.Role == port.RoleTool && strings.Contains(msg.Content, "not found") {
				sawRejection = true
			}
		}
	}
	if !sawRejection {
		t.Fatal("dispatching a disabled tool did not surface a not-found rejection")
	}
}

func TestEffectiveToolsUnaffectedWhenNoDisabled(t *testing.T) {
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{Maas: maas, Tools: unchangingReadRegistry(t)})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "go"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	var sawRead bool
	for _, tl := range maas.requests[0].Tools {
		if tl.Name == "read_file" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Fatal("read_file missing with no disabled_tools set")
	}
}
