package server

import (
	"net/http"

	"github.com/stardust/legion-agent/internal/approval"
)

// ApprovalLister lists persisted Manual-mode approval tickets so a UI can
// reconcile pending approvals it may have missed over the at-most-once SSE
// stream. It is satisfied by *approval.ToolGateStore.
type ApprovalLister interface {
	ListPending() ([]approval.ToolApproval, error)
}

// handleListApprovals serves GET /v1/approvals?status=pending, returning every
// on-disk pending ticket with sensitive/large arguments sanitized. Only the
// "pending" status filter (or none) is supported today; any other value is a
// 400 rather than a silently-ignored filter.
func (s *HTTPServer) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	if s.approvalTickets == nil {
		writeError(w, http.StatusServiceUnavailable, "approval store is unavailable")
		return
	}
	if status := r.URL.Query().Get("status"); status != "" && status != string(approval.ApprovalPending) {
		writeError(w, http.StatusBadRequest, "unsupported status filter; only 'pending' is supported")
		return
	}
	pending, err := s.approvalTickets.ListPending()
	if err != nil {
		s.logger.Error("list pending approvals", "error", err)
		writeError(w, http.StatusInternalServerError, "list pending approvals")
		return
	}
	out := make([]map[string]any, 0, len(pending))
	for _, t := range pending {
		out = append(out, map[string]any{
			"ticket_id":    t.TicketID,
			"task_id":      t.TaskID,
			"session_key":  t.SessionKey,
			"tool_name":    t.ToolName,
			"tool_call_id": t.ToolCallID,
			"arguments":    sanitizeStringMap(t.Arguments),
			"status":       string(t.Status),
			"created_at":   t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
}
