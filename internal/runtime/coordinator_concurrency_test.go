package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// TestCoordinatorRunsTasksConcurrently enqueues many tasks and drives the
// coordinator with repeated heartbeats; with bounded worker goroutines all
// tasks must reach a terminal state. Run under the race detector in a cgo-capable
// environment (WSL): GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go test -race.
func TestCoordinatorRunsTasksConcurrently(t *testing.T) {
	sched := task.NewScheduler()
	coord := newTestCoordinator(t, sched, 4)

	const n = 50
	ctx := context.Background()
	for i := range n {
		id := fmt.Sprintf("t-%d", i)
		if err := sched.Add(ctx, domain.Task{ID: id, AgentID: "default-agent", Status: domain.TaskPending, Input: "x"}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	// drain keeps calling Heartbeat and polling scheduler state until every
	// task is terminal. Heartbeat's own dispatched=false return only means
	// "workers busy or nothing pending at this instant" — since the stub
	// runner completes near-instantly, the spawned goroutines racing to
	// release worker slots against this loop means dispatched can go false
	// while pending tasks still remain (observed: relying on it alone left
	// tasks stuck Pending after only 2 of 13 dispatch waves). Polling actual
	// task status is the only way to know draining is truly done.
	allTerminal := func() bool {
		tasks, err := sched.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, tk := range tasks {
			switch tk.Status {
			case domain.TaskDone, domain.TaskFailed, domain.TaskSuspended:
			default:
				return false
			}
		}
		return true
	}
	drain := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, err := coord.Heartbeat(ctx); err != nil {
				t.Fatalf("heartbeat: %v", err)
			}
			if allTerminal() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("tasks did not all reach terminal state before deadline")
	}
	drain()
	coord.Wait()

	tasks, err := sched.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		switch tk.Status {
		case domain.TaskDone, domain.TaskFailed, domain.TaskSuspended:
		default:
			t.Fatalf("task %s not terminal: %s", tk.ID, tk.Status)
		}
	}
}
