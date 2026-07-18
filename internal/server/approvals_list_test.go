package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/approval"
)

type fakeApprovalLister struct {
	pending []approval.ToolApproval
	err     error
}

func (f fakeApprovalLister) ListPending() ([]approval.ToolApproval, error) {
	return f.pending, f.err
}

func TestListApprovalsReturnsPendingSanitized(t *testing.T) {
	lister := fakeApprovalLister{pending: []approval.ToolApproval{{
		TicketID: "ticket-1", TaskID: "task-1", SessionKey: "s1", ToolName: "write_file",
		ToolCallID: "call-1", Status: approval.ApprovalPending,
		Arguments: map[string]string{"path": "/tmp/x", "api_key": "SECRET"},
	}}}
	srv := NewHTTPServer(Config{AdminToken: "token", ApprovalTickets: lister})
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals?status=pending", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal error = %v, body=%s", err, rec.Body.String())
	}
	if len(resp.Approvals) != 1 {
		t.Fatalf("approvals len = %d, want 1", len(resp.Approvals))
	}
	got := resp.Approvals[0]
	if got["ticket_id"] != "ticket-1" || got["tool_name"] != "write_file" {
		t.Fatalf("approval = %#v, want ticket-1/write_file", got)
	}
	args, ok := got["arguments"].(map[string]any)
	if !ok || args["path"] != "/tmp/x" {
		t.Fatalf("arguments = %#v, want sanitized map with path", got["arguments"])
	}
	if _, leaked := args["api_key"]; leaked {
		t.Fatalf("arguments leaked sensitive api_key: %#v", args)
	}
}

func TestListApprovalsRejectsUnsupportedStatus(t *testing.T) {
	srv := NewHTTPServer(Config{AdminToken: "token", ApprovalTickets: fakeApprovalLister{}})
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals?status=approved", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unsupported status filter", rec.Code)
	}
}

// TestListApprovalsListPendingErrorReturns500 covers T5 Minor#1: the
// fakeApprovalLister.err field was defined but never exercised by a test, so
// the fail-loud branch in handleListApprovals (ListPending error -> 500,
// approvals_list.go:29-32) had no assertion. CLAUDE.md testing standards
// require fail-loud branches to be asserted, not just happy paths.
func TestListApprovalsListPendingErrorReturns500(t *testing.T) {
	srv := NewHTTPServer(Config{AdminToken: "token", ApprovalTickets: fakeApprovalLister{err: errors.New("boom")}})
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when ListPending errors; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListApprovalsUnavailableWithoutStore(t *testing.T) {
	srv := NewHTTPServer(Config{AdminToken: "token"})
	req := httptest.NewRequest(http.MethodGet, "/v1/approvals", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when approval store unwired", rec.Code)
	}
}
