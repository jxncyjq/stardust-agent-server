package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// TestWorkspaceRegistryPermitsBrowserTools guards the regression where the
// browser tools were registered onto the production workspace registry
// (NewFileReadWriteWorkspaceRegistry, used by the serve/RunTask path and by
// agent_resolver) but were never added to that registry's
// BatchRolePermissionEnforcer allowlist. The enforcer runs before the policy in
// Registry.Execute, so a developer-role browser_open call was rejected with
// ErrPermissionDenied ("permission denied") the moment the browser runtime was
// enabled — even though disabled_tools was empty and the mode was auto.
//
// The existing browser_test.go builds its registry with a nil enforcer and an
// allow-all StaticPolicy, so it cannot catch this class of bug. This test uses
// the real constructor and asserts every registered browser tool is permitted
// for the developer role (i.e. present in the enforcer allowlist).
func TestWorkspaceRegistryPermitsBrowserTools(t *testing.T) {
	reg := NewFileReadWriteWorkspaceRegistry(t.TempDir(), nil)
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: &fakeBrowserRuntime{}})

	cases := map[string]map[string]string{
		"browser_open":  {"url": "https://example.com"},
		"browser_read":  {"session_id": "sess-1"},
		"browser_click": {"session_id": "sess-1", "ref": "e1"},
		"browser_type":  {"session_id": "sess-1", "ref": "e1", "text": "hi"},
		"browser_close": {"session_id": "sess-1"},
	}
	for name, args := range cases {
		_, err := reg.Execute(context.Background(), domain.Agent{ID: "a", Role: "developer"},
			domain.ToolCall{ID: "c1", Name: name, Arguments: args})
		// The fake runtime never errors, so the only error a correctly wired
		// registry can surface here is a permission denial. Assert specifically
		// that it is NOT ErrPermissionDenied — any other (non-nil) error would be
		// an unrelated handler failure and is out of scope for this guard.
		if errors.Is(err, ErrPermissionDenied) {
			t.Errorf("%s: role %q denied by enforcer — tool is missing from the workspace registry allowlist", name, "developer")
		}
	}
}
