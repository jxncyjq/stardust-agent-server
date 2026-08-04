package tool

import (
	"strings"
	"testing"
)

// Browser.Enabled=false（或 Runtime=nil）时，RegisterBrowserTools 是 no-op：
// 注册表里不应出现任何 browser_* 工具。这守护 Task 9 生产接线的“默认关”契约。
func TestRegisterBrowserToolsNoopWhenDisabled(t *testing.T) {
	reg := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: false, Runtime: &fakeBrowserRuntime{}})
	for _, d := range reg.Descriptors() {
		if strings.HasPrefix(d.Name, "browser_") {
			t.Fatalf("browser tool %q registered while disabled", d.Name)
		}
	}
	// Runtime=nil 也必须 no-op（防生产接线在 Enabled=true 但构造失败时半注册）
	reg2 := NewRegistry(NewStaticPolicy(DecisionAllow), nil, NoopGuardrails{})
	RegisterBrowserTools(reg2, BrowserToolOptions{Enabled: true, Runtime: nil})
	if len(reg2.Descriptors()) != 0 {
		t.Fatalf("expected no tools when Runtime is nil, got %d", len(reg2.Descriptors()))
	}
}
