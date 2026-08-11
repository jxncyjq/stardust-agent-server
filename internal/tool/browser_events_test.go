package tool

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/domain"
)

// recordingRuntime 记录 Open 收到的完整 OpenReq，供透传断言使用；其余方法返回零值。
type recordingRuntime struct {
	lastOpen browser.OpenReq
}

func (r *recordingRuntime) Open(_ context.Context, req browser.OpenReq) (browser.OpenObservation, error) {
	r.lastOpen = req
	return browser.OpenObservation{SessionID: "sess-1"}, nil
}
func (r *recordingRuntime) Read(context.Context, browser.ReadReq) (browser.Observation, error) {
	return browser.Observation{}, nil
}
func (r *recordingRuntime) Click(context.Context, browser.ClickReq) (browser.Observation, error) {
	return browser.Observation{}, nil
}
func (r *recordingRuntime) Type(context.Context, browser.TypeReq) (browser.Observation, error) {
	return browser.Observation{}, nil
}
func (r *recordingRuntime) Close(context.Context, browser.CloseReq) error { return nil }
func (r *recordingRuntime) Subscribe(string) (<-chan browser.StreamEvent, func(), error) {
	return nil, func() {}, nil
}

func TestBrowserOpen_PassesTaskAndRoot(t *testing.T) {
	fake := &recordingRuntime{}
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: fake, ToolRoot: "/ws"})
	ctx := WithUserTask(context.Background(), "find login")
	_, err := reg.Execute(ctx, domain.Agent{ID: "a1", Role: "developer"}, domain.ToolCall{
		ID: "c1", Name: "browser_open", Arguments: map[string]string{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if fake.lastOpen.UserTask != "find login" || fake.lastOpen.ToolRoot != "/ws" {
		t.Fatalf("Req not populated: %+v", fake.lastOpen)
	}
}

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
