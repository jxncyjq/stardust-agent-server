package tool

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

type recordingSink struct {
	openedID, openedTask, openedURL string
	closedID                        string
}

func (r *recordingSink) SessionOpened(_ context.Context, sessionID, taskID, url string) {
	r.openedID, r.openedTask, r.openedURL = sessionID, taskID, url
}
func (r *recordingSink) SessionClosed(_ context.Context, sessionID string) { r.closedID = sessionID }

func TestBrowserOpenEmitsSessionOpened(t *testing.T) {
	sink := &recordingSink{}
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: &fakeBrowserRuntime{}, Events: sink})
	res, err := reg.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.ToolCall{ID: "c1", Name: "browser_open", Arguments: map[string]string{"url": "https://example.com"}})
	if err != nil || !res.Success {
		t.Fatalf("open failed: %v %q", err, res.Error)
	}
	if sink.openedID != "sess-1" || sink.openedURL != "https://example.com" {
		t.Fatalf("SessionOpened not called correctly: %+v", sink)
	}
}

func TestBrowserCloseEmitsSessionClosed(t *testing.T) {
	sink := &recordingSink{}
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: &fakeBrowserRuntime{}, Events: sink})
	_, _ = reg.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.ToolCall{ID: "c2", Name: "browser_close", Arguments: map[string]string{"session_id": "sess-9"}})
	if sink.closedID != "sess-9" {
		t.Fatalf("SessionClosed not called: %+v", sink)
	}
}

// Events 为 nil 时不 panic（不破坏现有）。
func TestBrowserToolsNilEventsSink(t *testing.T) {
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: &fakeBrowserRuntime{}, Events: nil})
	_, err := reg.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.ToolCall{ID: "c3", Name: "browser_open", Arguments: map[string]string{"url": "https://x"}})
	if err != nil {
		t.Fatalf("nil Events should not error: %v", err)
	}
}
