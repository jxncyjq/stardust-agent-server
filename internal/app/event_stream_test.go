package app

import (
	"context"
	"testing"
)

func TestRunDemoIncludesEventStreamStages(t *testing.T) {
	t.Parallel()

	result, err := New().RunDemo(context.Background())
	if err != nil {
		t.Fatalf("RunDemo() error = %v, want nil", err)
	}
	for _, want := range []string{
		"memory_prefetched",
		"inference_completed",
		"tool_executed",
		"audit_recorded",
	} {
		if !hasDemoEventType(result, want) {
			t.Errorf("RunDemo() event stream missing %q: %#v", want, result.EventStream)
		}
	}
}

func hasDemoEventType(result DemoResult, eventType string) bool {
	for _, event := range result.EventStream {
		if event.Type == eventType {
			return true
		}
	}
	return false
}
