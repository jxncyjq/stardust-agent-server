package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/security"
)

func (s *HTTPServer) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionReadAudit, security.ResourceAudit) {
		s.auditRBACDenied(r, principal, security.ResourceAudit)
		writeError(w, http.StatusForbidden, "audit access denied")
		return
	}
	if s.audit == nil {
		writeJSON(w, http.StatusOK, []domain.AuditEvent{})
		return
	}
	writeJSON(w, http.StatusOK, s.audit.Events())
}

func (s *HTTPServer) handleQualityEvals(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionReadQuality, security.ResourceQuality) {
		s.auditRBACDenied(r, principal, security.ResourceQuality)
		writeError(w, http.StatusForbidden, "quality access denied")
		return
	}
	if s.qualityEvals == nil {
		writeJSON(w, http.StatusOK, []quality.EvalRunRecord{})
		return
	}
	records, err := s.qualityEvals.ListQualityEvalRuns(r.Context(), quality.TrendQuery{
		AgentID:   r.URL.Query().Get("agent_id"),
		TaskID:    r.URL.Query().Get("task_id"),
		Component: r.URL.Query().Get("component"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list quality evals: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (s *HTTPServer) auditRBACDenied(r *http.Request, principal security.Principal, resource security.Resource) {
	if s.audit == nil {
		return
	}
	const action = "access_denied.rbac"
	if err := s.audit.Append(r.Context(), domain.AuditEvent{
		ID:          newRequestID(),
		RequestID:   requestIDFromContext(r.Context()),
		SubjectType: "company",
		SubjectID:   principal.CompanyID,
		Action:      action,
		Hash:        hashResource(string(resource)),
		CreatedAt:   time.Now(),
	}); err != nil {
		// See auditAccessDenied: the 403 is correct either way, but an RBAC
		// denial that never reached the audit store must still leave a trace.
		s.logger.Error("append rbac-denied audit",
			"error", err,
			"action", action,
			"company_id", principal.CompanyID,
			"role", string(principal.Role),
			"resource", string(resource),
			"request_id", requestIDFromContext(r.Context()),
		)
	}
}
