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
	_ = s.audit.Append(ctx, domain.AuditEvent{
		ID:          newRequestID(),
		RequestID:   requestIDFromContext(ctx),
		SubjectType: "company",
		SubjectID:   principal.CompanyID,
		Action:      "access_denied.cross_company",
		Hash:        hashResource(resourceType + ":" + resourceID),
		CreatedAt:   time.Now(),
	})
}

func hashResource(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
