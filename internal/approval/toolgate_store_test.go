package approval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/sessionstate"
)

func newRec(task, call, tool string) ToolApproval {
	return ToolApproval{SessionKey: "s1", TaskID: task, ToolCallID: call, ToolName: tool}
}

func writeFileHelper(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestToolGateStoreOpenIsIdempotent(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	a, err := s.Open(newRec("t1", "c1", "write_file"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != ApprovalPending {
		t.Fatalf("status = %q, want pending", a.Status)
	}
	b, err := s.Open(newRec("t1", "c1", "write_file"))
	if err != nil {
		t.Fatal(err)
	}
	if b.TicketID != a.TicketID {
		t.Fatalf("second Open minted new ticket %q != %q", b.TicketID, a.TicketID)
	}
}

func TestToolGateStoreDecidePersists(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	if _, err := s.Decide("s1", a.TicketID, ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	// Re-read from a fresh store: disk is the source of truth.
	got, ok, err := NewToolGateStore(dir).Get("s1", a.TicketID, "")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != ApprovalApproved {
		t.Fatalf("status = %q, want approved", got.Status)
	}
	// Deciding an already-decided ticket must fail loud.
	if _, err := s.Decide("s1", a.TicketID, ApprovalDenied, ""); err == nil {
		t.Fatal("re-decide: want error, got nil")
	}
}

func TestToolGateStoreDecideUnknownTicketWrapsErrTicketNotFound(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	_, err := s.Decide("s1", "does-not-exist", ApprovalApproved, "")
	if err == nil {
		t.Fatal("decide unknown ticket: want error, got nil")
	}
	if !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("decide unknown ticket: err = %v, want wrapping ErrTicketNotFound", err)
	}
}

// TestToolGateStoreDecideAlreadyDecidedWrapsErrTicketAlreadyDecided asserts the
// "already decided" rejection wraps the ErrTicketAlreadyDecided sentinel, so a
// caller can errors.Is-match it (the HTTP layer maps it to 409; the timeout
// sweep tolerates it as a benign race) rather than only reading the message.
func TestToolGateStoreDecideAlreadyDecidedWrapsErrTicketAlreadyDecided(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	if _, err := s.Decide("s1", a.TicketID, ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Decide("s1", a.TicketID, ApprovalDenied, "")
	if err == nil {
		t.Fatal("re-decide decided ticket: want error, got nil")
	}
	if !errors.Is(err, ErrTicketAlreadyDecided) {
		t.Fatalf("re-decide: err = %v, want wrapping ErrTicketAlreadyDecided", err)
	}
}

func TestToolGateStoreListForTaskAndPending(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	_, _ = s.Open(newRec("t1", "c1", "write_file"))
	a2, _ := s.Open(newRec("t1", "c2", "send_message"))
	_, _ = s.Open(ToolApproval{SessionKey: "s2", TaskID: "t2", ToolCallID: "c9", ToolName: "fetch_url"})
	forT1, err := s.ListForTask("s1", "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(forT1) != 2 {
		t.Fatalf("ListForTask t1 = %d, want 2", len(forT1))
	}
	if _, err := s.Decide("s1", a2.TicketID, ApprovalApproved, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	// c1(s1) + c9(s2) still pending; a2 decided.
	if len(pending) != 2 {
		t.Fatalf("ListPending = %d, want 2", len(pending))
	}
}

func TestToolGateStoreCorruptJSONFailsLoud(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	path := filepath.Join(sessionstate.SessionDir(dir, "s1"), "approvals", a.TicketID+".json")
	if err := writeFileHelper(path, "{ not json"); err != nil { // helper: os.WriteFile
		t.Fatal(err)
	}
	if _, _, err := s.Get("s1", a.TicketID, ""); err == nil {
		t.Fatal("Get on corrupt JSON: want fail-loud error, got nil")
	}
}

func TestToolGateStoreListForTaskCorruptFileFailsLoud(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	path := filepath.Join(sessionstate.SessionDir(dir, "s1"), "approvals", a.TicketID+".json")
	if err := writeFileHelper(path, "{ not json"); err != nil { // helper: os.WriteFile
		t.Fatal(err)
	}
	if _, err := s.ListForTask("s1", "t1", ""); err == nil {
		t.Fatal("ListForTask on corrupt JSON: want fail-loud error, got nil")
	}
}

func TestToolGateStoreListPendingCorruptFileFailsLoud(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	path := filepath.Join(sessionstate.SessionDir(dir, "s1"), "approvals", a.TicketID+".json")
	if err := writeFileHelper(path, "{ not json"); err != nil { // helper: os.WriteFile
		t.Fatal(err)
	}
	if _, err := s.ListPending(); err == nil {
		t.Fatal("ListPending on corrupt JSON: want fail-loud error, got nil")
	}
}

// TestToolGateStoreConcurrentAccessNoRace exercises ToolGateStore's mutex
// directly, in-package: several tickets are opened up front in one session,
// then N goroutines concurrently mix Decide (each on its own distinct
// ticket, so no goroutine can observe another's "already decided" race),
// Get, ListForTask, and ListPending against that same session. It must
// complete without any goroutine returning an unexpected error, and must be
// race-clean under `go test -race` — regressions in s.mu's coverage of
// reads (Get/ListForTask/ListPending) racing writes (Decide) should surface
// here, not only via manualgate's decider test.
func TestToolGateStoreConcurrentAccessNoRace(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)

	const n = 8
	tickets := make([]ToolApproval, n)
	for i := range n {
		rec, err := s.Open(newRec("t1", fmt.Sprintf("c%d", i), "write_file"))
		if err != nil {
			t.Fatalf("Open ticket %d: %v", i, err)
		}
		tickets[i] = rec
	}

	var wg sync.WaitGroup
	errCh := make(chan error, n*4)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Each goroutine decides its own distinct ticket — no two
			// goroutines target the same ticket, so a re-decide error here
			// would always be unexpected.
			if _, err := s.Decide("s1", tickets[i].TicketID, ApprovalApproved, ""); err != nil {
				errCh <- fmt.Errorf("goroutine %d Decide: %w", i, err)
			}

			if _, _, err := s.Get("s1", tickets[i].TicketID, ""); err != nil {
				errCh <- fmt.Errorf("goroutine %d Get: %w", i, err)
			}

			if _, err := s.ListForTask("s1", "t1", ""); err != nil {
				errCh <- fmt.Errorf("goroutine %d ListForTask: %w", i, err)
			}

			if _, err := s.ListPending(); err != nil {
				errCh <- fmt.Errorf("goroutine %d ListPending: %w", i, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}

	// Sanity: every ticket must have landed Approved — no lost writes.
	forT1, err := s.ListForTask("s1", "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(forT1) != n {
		t.Fatalf("ListForTask t1 after concurrent access = %d, want %d", len(forT1), n)
	}
	for _, rec := range forT1 {
		if rec.Status != ApprovalApproved {
			t.Fatalf("ticket %s status = %q, want approved", rec.TicketID, rec.Status)
		}
	}
}

func TestToolGateStoreDecideRejectsInvalidStatus(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	if _, err := s.Decide("s1", a.TicketID, ApprovalStatus("bogus"), ""); err == nil {
		t.Fatal("Decide with invalid status: want error, got nil")
	}
	got, ok, err := s.Get("s1", a.TicketID, "")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != ApprovalPending {
		t.Fatalf("status after rejected Decide = %q, want pending", got.Status)
	}
}

// TestToolGateStoreTicketUnderWorkingDir covers the working_dir-aware base
// resolution (design §4.0, mirroring sessionstate.Store's checkpoint
// resolution): a ticket opened with a non-empty WorkingDir must be filed
// under <WorkingDir>/.stardust rather than the workspace root, and Get must
// resolve the same base to find it again.
func TestToolGateStoreTicketUnderWorkingDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	workingDir := t.TempDir()
	s := NewToolGateStore(workspaceRoot)
	rec, err := s.Open(ToolApproval{
		SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file",
		WorkingDir: workingDir,
	})
	if err != nil {
		t.Fatalf("Open error = %v, want nil", err)
	}
	want := filepath.Join(workingDir, ".stardust", "session", "s1", "approvals", rec.TicketID+".json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("ticket not at %q: %v", want, err)
	}
	got, ok, err := s.Get("s1", rec.TicketID, workingDir) // Get 签名加 workingDir
	if err != nil || !ok {
		t.Fatalf("Get = _, %v, %v; want found", ok, err)
	}
	if got.ToolName != "write_file" {
		t.Fatalf("Get ToolName = %q, want write_file", got.ToolName)
	}
}

// TestListPendingInScansGivenBase covers ListPendingIn's explicit-base
// contract: it must scan exactly the base sessionstate.SessionBase resolves
// to for a given (workspaceRoot, workingDir) pair, not the store's
// workspaceRoot (that is what the parameterless ListPending covers).
func TestListPendingInScansGivenBase(t *testing.T) {
	workspaceRoot := t.TempDir()
	workingDir := t.TempDir()
	s := NewToolGateStore(workspaceRoot)
	_, _ = s.Open(ToolApproval{SessionKey: "s1", TaskID: "t1", ToolCallID: "c1", ToolName: "write_file", WorkingDir: workingDir})
	got, err := s.ListPendingIn(sessionstate.SessionBase(workspaceRoot, workingDir))
	if err != nil {
		t.Fatalf("ListPendingIn error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListPendingIn = %d tickets, want 1", len(got))
	}
}
