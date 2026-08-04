package domain

import "testing"

func TestTaskCancelledStatus(t *testing.T) {
	if TaskCancelled != "cancelled" {
		t.Fatalf("TaskCancelled = %q, want cancelled", TaskCancelled)
	}
}
