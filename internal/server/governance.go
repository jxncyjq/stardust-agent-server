package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/security"
)

func (s *HTTPServer) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !security.NewRBACPolicy().Allows(principal, security.ActionReadAudit, security.ResourceAudit) {
		s.auditRBACDenied(r, principal, security.ResourceAudit)
		writeError(w, http.StatusForbidden, "audit access denied")
		return
	}
	if s.audit == nil {
		writeJSON(w, http.StatusOK, []domain.AuditEvent{})
		return
	}
	events, err := s.audit.Events()
	if err != nil {
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Error("list audit events failed", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list audit events: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *HTTPServer) handleQualityEvals(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !security.NewRBACPolicy().Allows(principal, security.ActionReadQuality, security.ResourceQuality) {
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
	_ = s.audit.Append(r.Context(), domain.AuditEvent{
		ID:          newRequestID(),
		RequestID:   requestIDFromContext(r.Context()),
		SubjectType: "company",
		SubjectID:   principal.CompanyID,
		Action:      "access_denied.rbac",
		Hash:        hashResource(string(resource)),
		CreatedAt:   time.Now(),
	})
}
