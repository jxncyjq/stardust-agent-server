package approval

import (
	"errors"
	"os"
	"path/filepath"
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
	if _, err := s.Decide("s1", a.TicketID, ApprovalApproved); err != nil {
		t.Fatal(err)
	}
	// Re-read from a fresh store: disk is the source of truth.
	got, ok, err := NewToolGateStore(dir).Get("s1", a.TicketID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != ApprovalApproved {
		t.Fatalf("status = %q, want approved", got.Status)
	}
	// Deciding an already-decided ticket must fail loud.
	if _, err := s.Decide("s1", a.TicketID, ApprovalDenied); err == nil {
		t.Fatal("re-decide: want error, got nil")
	}
}

func TestToolGateStoreDecideUnknownTicketWrapsErrTicketNotFound(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	_, err := s.Decide("s1", "does-not-exist", ApprovalApproved)
	if err == nil {
		t.Fatal("decide unknown ticket: want error, got nil")
	}
	if !errors.Is(err, ErrTicketNotFound) {
		t.Fatalf("decide unknown ticket: err = %v, want wrapping ErrTicketNotFound", err)
	}
}

func TestToolGateStoreListForTaskAndPending(t *testing.T) {
	s := NewToolGateStore(t.TempDir())
	_, _ = s.Open(newRec("t1", "c1", "write_file"))
	a2, _ := s.Open(newRec("t1", "c2", "send_message"))
	_, _ = s.Open(ToolApproval{SessionKey: "s2", TaskID: "t2", ToolCallID: "c9", ToolName: "fetch_url"})
	forT1, err := s.ListForTask("s1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(forT1) != 2 {
		t.Fatalf("ListForTask t1 = %d, want 2", len(forT1))
	}
	if _, err := s.Decide("s1", a2.TicketID, ApprovalApproved); err != nil {
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
	if _, _, err := s.Get("s1", a.TicketID); err == nil {
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
	if _, err := s.ListForTask("s1", "t1"); err == nil {
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

func TestToolGateStoreDecideRejectsInvalidStatus(t *testing.T) {
	dir := t.TempDir()
	s := NewToolGateStore(dir)
	a, _ := s.Open(newRec("t1", "c1", "write_file"))
	if _, err := s.Decide("s1", a.TicketID, ApprovalStatus("bogus")); err == nil {
		t.Fatal("Decide with invalid status: want error, got nil")
	}
	got, ok, err := s.Get("s1", a.TicketID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != ApprovalPending {
		t.Fatalf("status after rejected Decide = %q, want pending", got.Status)
	}
}
