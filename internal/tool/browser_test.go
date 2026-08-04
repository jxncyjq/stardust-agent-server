package tool

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/domain"
)

type fakeBrowserRuntime struct {
	lastOpenURL  string
	lastClickRef string
}

func (f *fakeBrowserRuntime) Open(_ context.Context, req browser.OpenReq) (browser.OpenObservation, error) {
	f.lastOpenURL = req.URL
	return browser.OpenObservation{
		SessionID:   "sess-1",
		Observation: browser.Observation{Text: "[e1] <button> 搜索\n"},
	}, nil
}
func (f *fakeBrowserRuntime) Read(context.Context, browser.ReadReq) (browser.Observation, error) {
	return browser.Observation{Text: "read-ok"}, nil
}
func (f *fakeBrowserRuntime) Click(_ context.Context, req browser.ClickReq) (browser.Observation, error) {
	f.lastClickRef = req.Ref
	return browser.Observation{Text: "clicked"}, nil
}
func (f *fakeBrowserRuntime) Type(context.Context, browser.TypeReq) (browser.Observation, error) {
	return browser.Observation{Text: "typed"}, nil
}
func (f *fakeBrowserRuntime) Close(context.Context, browser.CloseReq) error { return nil }

func newBrowserRegistry(t *testing.T, rt browser.RuntimeAPI) *Registry {
	t.Helper()
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: rt})
	return reg
}

func exec(t *testing.T, reg *Registry, name string, args map[string]string) domain.ToolResult {
	t.Helper()
	res, err := reg.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"},
		domain.ToolCall{ID: "c1", Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func TestBrowserOpenReturnsSessionAndObservation(t *testing.T) {
	f := &fakeBrowserRuntime{}
	reg := newBrowserRegistry(t, f)
	res := exec(t, reg, "browser_open", map[string]string{"url": "https://example.com"})
	if !res.Success {
		t.Fatalf("open failed: %q", res.Error)
	}
	if f.lastOpenURL != "https://example.com" {
		t.Fatalf("url not passed through: %q", f.lastOpenURL)
	}
	if !contains(res.Output, "sess-1") || !contains(res.Output, "搜索") {
		t.Fatalf("output missing session/observation: %q", res.Output)
	}
}

func TestBrowserClickPassesRef(t *testing.T) {
	f := &fakeBrowserRuntime{}
	reg := newBrowserRegistry(t, f)
	res := exec(t, reg, "browser_click", map[string]string{"session_id": "sess-1", "ref": "e1"})
	if !res.Success || f.lastClickRef != "e1" {
		t.Fatalf("click ref not passed: success=%v ref=%q", res.Success, f.lastClickRef)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
