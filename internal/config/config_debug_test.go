package config

import (
	"encoding/json"
	"testing"
)

// TestRuntimeConfigDebugParses verifies the runtime.debug toggle is read from
// the config file. It is an optional switch (absent = off), so its zero value
// is a legitimate "disabled" state, not a swallowed error.
func TestRuntimeConfigDebugParses(t *testing.T) {
	var rc RuntimeConfig
	if err := json.Unmarshal([]byte(`{"debug":true,"lazy_tools":false}`), &rc); err != nil {
		t.Fatalf("unmarshal runtime config: %v", err)
	}
	if !rc.Debug {
		t.Fatalf("RuntimeConfig.Debug = false, want true from {\"debug\":true}")
	}

	var off RuntimeConfig
	if err := json.Unmarshal([]byte(`{}`), &off); err != nil {
		t.Fatalf("unmarshal empty runtime config: %v", err)
	}
	if off.Debug {
		t.Fatalf("RuntimeConfig.Debug = true for empty config, want false (absent = off)")
	}
}
