package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/security"
)

func (s *HTTPServer) requireCompanyAccess(w http.ResponseWriter, r *http.Request, companyID string, resourceType string, resourceID string) bool {
	principal := security.PrincipalFromRequest(r)
	if security.CanAccessCompany(principal, companyID) {
		return true
	}
	s.auditAccessDenied(r.Context(), principal, resourceType, resourceID)
	writeError(w, http.StatusForbidden, "company access denied")
	return false
}

func (s *HTTPServer) auditAccessDenied(ctx context.Context, principal security.Principal, resourceType string, resourceID string) {
	if s.audit == nil {
		return
	}
	const action = "access_denied.cross_company"
	if err := s.audit.Append(ctx, domain.AuditEvent{
		ID:          newRequestID(),
		RequestID:   requestIDFromContext(ctx),
		SubjectType: "company",
		SubjectID:   principal.CompanyID,
		Action:      action,
		Hash:        hashResource(resourceType + ":" + resourceID),
		CreatedAt:   time.Now(),
	}); err != nil {
		// The 403 itself already stands; what must not vanish is the fact that
		// a denied cross-company attempt never reached the audit store. Losing
		// it silently breaks the security forensics chain.
		s.logger.Error("append access-denied audit",
			"error", err,
			"action", action,
			"company_id", principal.CompanyID,
			"resource_type", resourceType,
			"resource_id", resourceID,
			"request_id", requestIDFromContext(ctx),
		)
	}
}

func hashResource(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
