package observability

import (
	"testing"
	"time"
)

func TestMetricsRecorderSnapshot(t *testing.T) {
	startedAt := time.Date(2026, 5, 14, 8, 30, 0, 0, time.UTC)
	recorder := NewMetricsRecorder(func() time.Time { return startedAt })

	recorder.IncTaskStatus("submitted")
	recorder.IncTaskStatus("submitted")
	recorder.IncTaskStatus("done")
	recorder.IncHTTPStatus(200)
	recorder.IncHTTPStatus(201)
	recorder.IncHTTPStatus(201)
	recorder.IncModelCall("success")
	recorder.IncApproval("created")
	recorder.IncWorkflowRun("waiting")

	snapshot := recorder.Snapshot()
	if !snapshot.StartedAt.Equal(startedAt) {
		t.Fatalf("Snapshot().StartedAt = %v, want %v", snapshot.StartedAt, startedAt)
	}
	if snapshot.Tasks["submitted"] != 2 || snapshot.Tasks["done"] != 1 {
		t.Fatalf("Snapshot().Tasks = %#v, want submitted=2 done=1", snapshot.Tasks)
	}
	if snapshot.HTTPStatus["200"] != 1 || snapshot.HTTPStatus["201"] != 2 {
		t.Fatalf("Snapshot().HTTPStatus = %#v, want 200=1 201=2", snapshot.HTTPStatus)
	}
	if snapshot.ModelCalls["success"] != 1 {
		t.Fatalf("Snapshot().ModelCalls = %#v, want success=1", snapshot.ModelCalls)
	}
	if snapshot.Approvals["created"] != 1 {
		t.Fatalf("Snapshot().Approvals = %#v, want created=1", snapshot.Approvals)
	}
	if snapshot.WorkflowRuns["waiting"] != 1 {
		t.Fatalf("Snapshot().WorkflowRuns = %#v, want waiting=1", snapshot.WorkflowRuns)
	}

	snapshot.Tasks["submitted"] = 99
	if got := recorder.Snapshot().Tasks["submitted"]; got != 2 {
		t.Fatalf("Snapshot() mutable copy changed recorder task submitted = %d, want 2", got)
	}
}
