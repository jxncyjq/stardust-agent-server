package host

import (
	"errors"
	"strings"
	"testing"
)

// A "service:<name>/<capability>" target is how a consumer reaches a provider
// without knowing whose tool runs. These tests pin the parsing and the four
// ways resolution can fail — every one of them an error, because the
// alternative (falling back to looking the literal name up as a tool) answers
// a typo in a service reference with an unrelated "tool not found".

type resolverFunc func(service, capability string) (string, error)

func (f resolverFunc) ResolveService(service, capability string) (string, error) {
	return f(service, capability)
}

func TestAPlainToolNamePassesThroughUntouched(t *testing.T) {
	got, err := resolveServiceTarget(Deps{}, "read_file")
	if err != nil {
		t.Fatalf("resolveServiceTarget: %v", err)
	}
	if got != "read_file" {
		t.Errorf("resolved = %q, want the name unchanged", got)
	}
}

func TestAServiceTargetResolvesToTheProvidersTool(t *testing.T) {
	deps := Deps{Services: resolverFunc(func(service, capability string) (string, error) {
		if service != "issue-tracker" || capability != "search" {
			t.Errorf("resolver asked for %q/%q, want issue-tracker/search", service, capability)
		}
		return "jira_search", nil
	})}

	got, err := resolveServiceTarget(deps, "service:issue-tracker/search")
	if err != nil {
		t.Fatalf("resolveServiceTarget: %v", err)
	}
	if got != "jira_search" {
		t.Errorf("resolved = %q, want jira_search", got)
	}
}

func TestAMalformedServiceTargetIsRefused(t *testing.T) {
	deps := Deps{Services: resolverFunc(func(string, string) (string, error) {
		t.Error("the resolver was asked about a malformed target")
		return "", nil
	})}

	for _, target := range []string{
		"service:issue-tracker",  // no capability
		"service:/search",        // no service
		"service:issue-tracker/", // empty capability
	} {
		if _, err := resolveServiceTarget(deps, target); err == nil {
			t.Errorf("resolveServiceTarget(%q) = nil error, want a refusal", target)
		}
	}
}

// TestAServiceTargetWithNoResolverSaysSo: the fallback that must not exist.
// Looking "service:x/y" up as a tool name would answer a service problem with
// "tool not found", which sends the reader looking for the wrong thing.
func TestAServiceTargetWithNoResolverSaysSo(t *testing.T) {
	_, err := resolveServiceTarget(Deps{}, "service:issue-tracker/search")
	if err == nil {
		t.Fatal("resolveServiceTarget with no resolver = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "service resolver") {
		t.Errorf("error = %v, want it to say this deployment has no service resolver", err)
	}
}

func TestAFailedResolutionCarriesTheResolversReason(t *testing.T) {
	deps := Deps{Services: resolverFunc(func(string, string) (string, error) {
		return "", errors.New("no plugin provides service \"issue-tracker\"")
	})}

	_, err := resolveServiceTarget(deps, "service:issue-tracker/search")
	if err == nil {
		t.Fatal("resolveServiceTarget = nil error, want the resolver's refusal")
	}
	if !strings.Contains(err.Error(), "no plugin provides") {
		t.Errorf("error = %v, want the resolver's own reason", err)
	}
}

// TestAResolverThatAnswersNothingIsBrokenWiring: an empty name with no error
// would be dispatched as an unknown tool, blaming the consumer for the host's
// own fault.
func TestAResolverThatAnswersNothingIsBrokenWiring(t *testing.T) {
	deps := Deps{Services: resolverFunc(func(string, string) (string, error) { return "", nil })}

	if _, err := resolveServiceTarget(deps, "service:issue-tracker/search"); err == nil {
		t.Fatal("resolveServiceTarget with an empty answer = nil error, want a refusal")
	}
}
