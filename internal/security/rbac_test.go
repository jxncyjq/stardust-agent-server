package security

import "testing"

func TestRBACAllowsViewerReadQualityButRejectsAudit(t *testing.T) {
	t.Parallel()
	policy := NewRBACPolicy()
	viewer := Principal{CompanyID: "company-1", Role: "viewer"}
	if !policy.Allows(viewer, ActionReadQuality, ResourceQuality) {
		t.Fatalf("Allows(viewer, read_quality, quality) = false, want true")
	}
	if policy.Allows(viewer, ActionReadAudit, ResourceAudit) {
		t.Fatalf("Allows(viewer, read_audit, audit) = true, want false")
	}
}
