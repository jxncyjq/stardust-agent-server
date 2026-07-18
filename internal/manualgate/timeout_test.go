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
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/task"
)

// singleBaseFn returns a basesFn that always enumerates exactly one base —
// the fixture helper used by every single-base sweep test in this file, so
// each keeps testing NewTimeoutSweepJob's per-base ListPendingIn/deny loop
// without needing to know about multi-base enumeration.
func singleBaseFn(base string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) {
		return []string{base}, nil
	}
}

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
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)), singleBaseFn(dir))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalDenied {
		t.Fatalf("stale ticket status = %s, want denied", got.Status)
	}
	if st, _, err := sched.Get(context.Background(), "t1"); err != nil || st.Status != domain.TaskRunning {
		t.Fatalf("task after timeout-deny = %v (err=%v), want running", st.Status, err)
	}

	got2, _, err := store.Get("s2", approval.TicketID("t2", "c1"), "")
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
	job := NewTimeoutSweepJob(store, dec, time.Hour, nearNow, slog.New(slog.NewTextHandler(io.Discard, nil)), singleBaseFn(dir))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"), "")
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
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)), singleBaseFn(dir))

	err := job(context.Background())
	if err == nil {
		t.Fatal("job with Decide failure on unknown task: want error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout-deny ticket") {
		t.Fatalf("job error = %q, want it to contain %q", err.Error(), "timeout-deny ticket")
	}

	// The ticket must remain untouched (still pending) since the deny was
	// never actually recorded — Decide failed before any write.
	got, _, err := store.Get("sghost", approval.TicketID("ghost", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalPending {
		t.Fatalf("ticket status after failed sweep = %s, want still pending", got.Status)
	}
}

// TestTimeoutSweepToleratesConcurrentDecision covers the benign race: a ticket
// the sweep captured as pending is decided by someone else (a human, or another
// pass) in the window between ListPending and the sweep's own Decide. The now
// hook fires exactly in that window — the loop's age check calls it before
// Decide — so injecting the competing approval there reproduces the race
// deterministically. The sweep must treat the resulting ErrTicketAlreadyDecided
// as the intended outcome and return nil (the winning decision stands, the task
// still resumes), not bubble it up as a background-scheduler Error.
func TestTimeoutSweepToleratesConcurrentDecision(t *testing.T) {
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
	// Fire once, in the loop's age check, i.e. exactly in the list→decide window:
	// inject a competing human approval so the sweep's own deny then loses the
	// race and hits "already decided".
	var raced bool
	racingNow := func() time.Time {
		if !raced {
			raced = true
			if _, err := dec.Decide(context.Background(), "t1", approval.TicketID("t1", "c1"), approval.ApprovalApproved); err != nil {
				t.Fatalf("inject competing decision: %v", err)
			}
		}
		return time.Now().Add(10 * time.Minute) // far future -> stale, so the sweep tries to deny
	}
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, racingNow, slog.New(slog.NewTextHandler(io.Discard, nil)), singleBaseFn(dir))
	if err := job(context.Background()); err != nil {
		t.Fatalf("sweep racing a concurrent decision: want nil (benign already-decided), got %v", err)
	}

	// The competing approval stands — the sweep must not have overwritten it to
	// denied.
	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalApproved {
		t.Fatalf("ticket status = %s, want approved (competing decision preserved)", got.Status)
	}
	// And the winning decision resumed the task, exactly as intended.
	if st, _, err := sched.Get(context.Background(), "t1"); err != nil || st.Status != domain.TaskRunning {
		t.Fatalf("task after tolerated race = %v (err=%v), want running", st.Status, err)
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
	job := NewTimeoutSweepJob(store, dec, 0, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)), singleBaseFn(dir))
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _, err := store.Get("s1", approval.TicketID("t1", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != approval.ApprovalPending {
		t.Fatalf("ticket status with ttl<=0 = %s, want still pending (sweep disabled)", got.Status)
	}
}

// TestTimeoutSweepScansAllBases is the M3a Task 5 regression: a pending ticket
// filed under a working_dir-bound session base (not the workspace root) must
// still be swept and denied when basesFn enumerates that base too. Before this
// change the sweep only ever scanned a single hard-coded base
// (store.ListPending() == ListPendingIn(workspaceRoot)), so a ticket under
// SessionBase(workspaceRoot, workingDir) would be silently skipped forever.
func TestTimeoutSweepScansAllBases(t *testing.T) {
	workspaceRoot := t.TempDir()
	workingDir := t.TempDir()
	store := approval.NewToolGateStore(workspaceRoot)
	sched := task.NewScheduler()

	if err := sched.Add(context.Background(), domain.Task{ID: "t-root", SessionID: "s-root", Status: domain.TaskRunning}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t-root", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s-root", TaskID: "t-root", ToolCallID: "c1", ToolName: "write_file"}); err != nil {
		t.Fatal(err)
	}

	if err := sched.Add(context.Background(), domain.Task{ID: "t-wd", SessionID: "s-wd", Status: domain.TaskRunning, WorkingDir: workingDir}); err != nil {
		t.Fatal(err)
	}
	if err := sched.Transition(context.Background(), "t-wd", domain.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(approval.ToolApproval{SessionKey: "s-wd", TaskID: "t-wd", ToolCallID: "c1", ToolName: "write_file", WorkingDir: workingDir}); err != nil {
		t.Fatal(err)
	}

	dec := NewApprovalCoordinator(store, sched)
	fixedNow := func() time.Time { return time.Now().Add(10 * time.Minute) } // far future -> everything stale
	wdBase := sessionstate.SessionBase(workspaceRoot, workingDir)
	basesFn := func(context.Context) ([]string, error) {
		return []string{workspaceRoot, wdBase}, nil
	}
	job := NewTimeoutSweepJob(store, dec, 5*time.Minute, fixedNow, slog.New(slog.NewTextHandler(io.Discard, nil)), basesFn)
	if err := job(context.Background()); err != nil {
		t.Fatal(err)
	}

	gotRoot, _, err := store.Get("s-root", approval.TicketID("t-root", "c1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot.Status != approval.ApprovalDenied {
		t.Fatalf("root-base ticket status = %s, want denied", gotRoot.Status)
	}

	gotWD, _, err := store.Get("s-wd", approval.TicketID("t-wd", "c1"), workingDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotWD.Status != approval.ApprovalDenied {
		t.Fatalf("working_dir-base ticket status = %s, want denied (must not be skipped by single-base scan)", gotWD.Status)
	}
}
