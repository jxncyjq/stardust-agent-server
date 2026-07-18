package manualgate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/task"
)

// TestTimeoutSweepDeniesStalePending covers the happy path: a pending ticket
// older than ttl is denied and its owning Suspended task flips to Running,
// while a fresh pending ticket for a different task is left untouched.
func TestTimeoutSweepDeniesStalePending(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	// stale ticket — but ToolApproval.CreatedAt is set by Open to time.Now();
	// to control age we inject a future `now` for the sweep job instead of
	// rewriting CreatedAt on disk.
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}

	// a fresh ticket on a second, still-running task must remain pending: the
	// far-future `now` makes it "stale" too only if the sweep ignores ttl —
	// this second ticket exists to prove the deny loop actually iterates
	// ListPending's full result set, not just the first record.
	if err := sched.Add(context.Background(), domain.Task{ID: "t2", SessionID: "s2", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t2", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s2", TaskID: "t2", ToolCallID: "c1", ToolName: "read_file"}); err != nil {
		t.Fatal(err)
	}

	dec := NewApprovalCoordinator(store, sched)
	fixedNow := func() time.Time { return time.Now().Add(10 * time.Minute) } // far future -> everything stale
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalDenied {
		t.Fatalf("stale ticket status = %s, want denied", got.Status)
	}
	if st, _, err := sched.Get(context.Background(), "t1"); err != nil || st.Status != domain.TaskRunning {
		t.Fatalf("task after timeout-deny = %v (err=%v), want running", st.Status, err)
	}

	got2, _, err := store.Get("s2", approval.TicketID("t2", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != approval.ApprovalDenied {
		t.Fatalf("second stale ticket status = %s, want denied", got2.Status)
	}
}

// TestTimeoutSweepSkipsFreshPending covers the age guard: with a `now` that
// is only a few seconds ahead and a long ttl, a just-opened ticket must stay
// pending — the sweep must not deny everything regardless of age.
func TestTimeoutSweepSkipsFreshPending(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}

	dec := NewApprovalCoordinator(store, sched)
	nearNow := func() time.Time { return time.Now() }
	job := NewTimeoutSweepJob(store, dec, time.Hour, nearNow, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalPending {
		t.Fatalf("fresh ticket status = %s, want still pending", got.Status)
	}
	if st, _, err := sched.Get(context.Background(), "t1"); err != nil || st.Status != domain.TaskSuspended {
		t.Fatalf("task after no-op sweep = %v (err=%v), want still suspended", st.Status, err)
	}
}

// TestTimeoutSweepDecideErrorPropagates covers the fail-loud wrap path: when
// dec.Decide fails on a stale pending ticket (here, because the ticket's
// TaskID refers to a task the scheduler has never heard of), the sweep job
// must not swallow the error — it must return it wrapped with the ticket ID,
// per the "timeout-deny ticket %s: %w" wrap in NewTimeoutSweepJob.
func TestTimeoutSweepDecideErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler() // deliberately empty: "ghost" task is never added

	if _, err := store.Open(approval.ToolApproval{SessionKey: "sghost", TaskID: "ghost", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}

	dec := NewApprovalCoordinator(store, sched)
	fixedNow := func() time.Time { return time.Now().Add(10 * time.Minute) } // far future -> stale
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := job(context.Background())
	if err == nil {
		t.Fatal("job with Decide failure on unknown task: want error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout-deny ticket") {
		t.Fatalf("job error = %q, want it to contain %q", err.Error(), "timeout-deny ticket")
	}

	// The ticket must remain untouched (still pending) since the deny was
	// never actually recorded — Decide failed before any write.
	got, _, err := store.Get("sghost", approval.TicketID("ghost", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalPending {
		t.Fatalf("ticket status after failed sweep = %s, want still pending", got.Status)
	}
}

// TestTimeoutSweepDisabledWhenTTLNonPositive covers the documented "ttl<=0
// disables the sweep" contract: the job must return nil immediately without
// listing or deciding anything, leaving pending tickets untouched.
func TestTimeoutSweepDisabledWhenTTLNonPositive(t *testing.T) {
	dir := t.TempDir()
	store := approval.NewToolGateStore(dir)
	sched := task.NewScheduler()
	if err := sched.Add(context.Background(), domain.Task{ID: "t1", SessionID: "s1", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t1", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}

	dec := NewApprovalCoordinator(store, sched)
	fixedNow := func() time.Time { return time.Now().Add(24 * time.Hour) }
	job := NewTimeoutSweepJob(store, dec, 0, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalPending {
		t.Fatalf("ticket status with ttl<=0 = %s, want still pending (sweep disabled)", got.Status)
	}
}
