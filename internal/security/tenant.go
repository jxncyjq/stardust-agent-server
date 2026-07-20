package security

import "net/http"

type Principal struct {
	CompanyID string
	SubjectID string
	Role      string
}

func PrincipalFromRequest(r *http.Request) Principal {
	return Principal{
		CompanyID: r.Header.Get("X-Company-ID"),
		SubjectID: r.Header.Get("X-Subject-ID"),
		Role:      r.Header.Get("X-Role"),
	}
}

// CanAccessCompany reports whether principal may reach a resource owned by
// companyID. An empty Principal.CompanyID matches every company only while
// Policy.RequireIdentity is false; with the switch on it is rejected like a
// mismatched tenant.
func (p Policy) CanAccessCompany(principal Principal, companyID string) bool {
	if principal.CompanyID == "" {
		return !p.RequireIdentity
	}
	return principal.CompanyID == companyID
}
