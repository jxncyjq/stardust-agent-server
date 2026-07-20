package security

import "testing"

// TestPolicyAllowsIdentityMatrix pins the two-mode contract: with
// RequireIdentity off an empty role keeps the legacy single-tenant behaviour
// (full access), with it on an empty role is rejected like any unknown role.
func TestPolicyAllowsIdentityMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		requireIdentity bool
		role            string
		action          Action
		resource        Resource
		want            bool
	}{
		{name: "optional identity empty role reads audit", requireIdentity: false, role: "", action: ActionReadAudit, resource: ResourceAudit, want: true},
		{name: "optional identity empty role reads quality", requireIdentity: false, role: "", action: ActionReadQuality, resource: ResourceQuality, want: true},
		{name: "optional identity admin reads audit", requireIdentity: false, role: "admin", action: ActionReadAudit, resource: ResourceAudit, want: true},
		{name: "optional identity viewer rejected on audit", requireIdentity: false, role: "viewer", action: ActionReadAudit, resource: ResourceAudit, want: false},
		{name: "optional identity unknown role rejected", requireIdentity: false, role: "intruder", action: ActionReadQuality, resource: ResourceQuality, want: false},

		{name: "required identity empty role rejected on audit", requireIdentity: true, role: "", action: ActionReadAudit, resource: ResourceAudit, want: false},
		{name: "required identity empty role rejected on quality", requireIdentity: true, role: "", action: ActionReadQuality, resource: ResourceQuality, want: false},
		{name: "required identity admin reads audit", requireIdentity: true, role: "admin", action: ActionReadAudit, resource: ResourceAudit, want: true},
		{name: "required identity operator reads audit", requireIdentity: true, role: "operator", action: ActionReadAudit, resource: ResourceAudit, want: true},
		{name: "required identity viewer reads quality", requireIdentity: true, role: "viewer", action: ActionReadQuality, resource: ResourceQuality, want: true},
		{name: "required identity viewer rejected on audit", requireIdentity: true, role: "viewer", action: ActionReadAudit, resource: ResourceAudit, want: false},
		{name: "required identity unknown role rejected", requireIdentity: true, role: "intruder", action: ActionReadQuality, resource: ResourceQuality, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := NewPolicy(tt.requireIdentity)
			principal := Principal{CompanyID: "company-1", Role: tt.role}
			if got := policy.Allows(principal, tt.action, tt.resource); got != tt.want {
				t.Fatalf("NewPolicy(%t).Allows(role=%q, %q, %q) = %t, want %t",
					tt.requireIdentity, tt.role, tt.action, tt.resource, got, tt.want)
			}
		})
	}
}

// TestPolicyCanAccessCompanyIdentityMatrix pins the tenant half of the same
// contract: an absent X-Company-ID is a wildcard only while identity is
// optional.
func TestPolicyCanAccessCompanyIdentityMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		requireIdentity   bool
		principalCompany  string
		resourceCompanyID string
		want              bool
	}{
		{name: "optional identity empty company passes", requireIdentity: false, principalCompany: "", resourceCompanyID: "company-a", want: true},
		{name: "optional identity matching company passes", requireIdentity: false, principalCompany: "company-a", resourceCompanyID: "company-a", want: true},
		{name: "optional identity mismatched company rejected", requireIdentity: false, principalCompany: "company-b", resourceCompanyID: "company-a", want: false},

		{name: "required identity empty company rejected", requireIdentity: true, principalCompany: "", resourceCompanyID: "company-a", want: false},
		{name: "required identity matching company passes", requireIdentity: true, principalCompany: "company-a", resourceCompanyID: "company-a", want: true},
		{name: "required identity mismatched company rejected", requireIdentity: true, principalCompany: "company-b", resourceCompanyID: "company-a", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := NewPolicy(tt.requireIdentity)
			principal := Principal{CompanyID: tt.principalCompany, Role: "admin"}
			if got := policy.CanAccessCompany(principal, tt.resourceCompanyID); got != tt.want {
				t.Fatalf("NewPolicy(%t).CanAccessCompany(principal=%q, %q) = %t, want %t",
					tt.requireIdentity, tt.principalCompany, tt.resourceCompanyID, got, tt.want)
			}
		})
	}
}
