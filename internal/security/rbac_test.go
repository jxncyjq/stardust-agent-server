package security

import "testing"

func TestRBACAllowsViewerReadQualityButRejectsAudit(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(false)
	viewer := Principal{CompanyID: "company-1", Role: "viewer"}
	if !policy.Allows(viewer, ActionReadQuality, ResourceQuality) {
		t.Fatalf("Allows(viewer, read_quality, quality) = false, want true")
	}
	if policy.Allows(viewer, ActionReadAudit, ResourceAudit) {
		t.Fatalf("Allows(viewer, read_audit, audit) = true, want false")
	}
}

// TestRBACWritePluginIsAdminOnly pins ActionWritePlugin's deliberately
// narrow grant: unlike ActionReadPlugin (which operator carries alongside
// its other read actions), the write action that authorizes or revokes a
// plugin's capabilities is restricted to admin -- neither operator nor
// viewer may call POST /v1/plugins/{name}/grant or .../deny.
func TestRBACWritePluginIsAdminOnly(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(false)

	admin := Principal{CompanyID: "company-1", Role: "admin"}
	if !policy.Allows(admin, ActionWritePlugin, ResourcePlugin) {
		t.Fatalf("Allows(admin, write_plugin, plugin) = false, want true")
	}
	operator := Principal{CompanyID: "company-1", Role: "operator"}
	if policy.Allows(operator, ActionWritePlugin, ResourcePlugin) {
		t.Fatalf("Allows(operator, write_plugin, plugin) = true, want false")
	}
	viewer := Principal{CompanyID: "company-1", Role: "viewer"}
	if policy.Allows(viewer, ActionWritePlugin, ResourcePlugin) {
		t.Fatalf("Allows(viewer, write_plugin, plugin) = true, want false")
	}
}
