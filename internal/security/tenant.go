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

func CanAccessCompany(principal Principal, companyID string) bool {
	if principal.CompanyID == "" {
		return true
	}
	return principal.CompanyID == companyID
}
