package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
)

func TestModelViewShowsEventStream(t *testing.T) {
	t.Parallel()

	model := NewModel(app.DemoResult{
		TaskID: "task-1",
		Result: "done",
		EventStream: []domain.RuntimeEvent{
			{Type: "memory_prefetched", TaskID: "task-1", Message: "prefetched 1 memory entry", CreatedAt: time.Now()},
			{Type: "inference_completed", TaskID: "task-1", Message: "model completed", CreatedAt: time.Now()},
			{Type: "tool_executed", TaskID: "task-1", Message: "echo_tool completed", CreatedAt: time.Now()},
			{Type: "audit_recorded", TaskID: "task-1", Message: "tool_executed", CreatedAt: time.Now()},
		},
	})

	got := model.View()
	for _, want := range []string{
		"Event Stream:",
		"memory_prefetched",
		"inference_completed",
		"tool_executed",
		"audit_recorded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Model.View() missing %q in:\n%s", want, got)
		}
	}
}

func TestModelViewSanitizesTerminalOutput(t *testing.T) {
	t.Parallel()

	model := NewModel(app.DemoResult{
		TaskID: "task-1",
		Result: "done\x1b[31m red\x1b[0m",
		EventStream: []domain.RuntimeEvent{
			{Type: "tool", TaskID: "task-1", Message: "line1\nline2\x1b[31m"},
		},
		AuditActions: []string{"audit\x1b[31m"},
	})

	got := model.View()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("Model.View() = %q, want no ANSI escape", got)
	}
	if strings.Contains(got, "line1\nline2") {
		t.Fatalf("Model.View() = %q, want event message kept on one line", got)
	}
}
