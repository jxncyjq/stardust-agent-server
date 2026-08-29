package loader

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
)

// Named services let a plugin depend on a CAPABILITY instead of on somebody's
// specific tool name. The lifecycle is the one this package already runs: no
// provider means the consumer is SUSPENDED (not unloaded), and a provider
// arriving later resumes it.
//
// Bounds (fork-bomb regime): every test here mounts real wasm instances and
// applies a written-out, fixed number of times. Nothing loops until a
// condition, and no test uses the feature under test as its termination
// condition.

const (
	svcProviderName = "svc-prov"
	svcProviderTool = "svc_prov_tool"

	svcConsumerName = "svc-cons"
	svcConsumerTool = "svc_cons_tool"

	svcRivalName = "svc-rival"
	svcRivalTool = "svc_rival_tool"

	issueTracker = "issue-tracker"
)

// writeServicePkg writes a plugin that provides/requires SERVICES rather than
// tool names.
func (h *harness) writeServicePkg(dir, name, toolName string, provides, requires []string) manifest.Entry {
	h.t.Helper()

	writePackage(h.t, filepath.Join(h.root, dir), pkg{
		wasm:             patchIdentity(h.t, name, toolName),
		name:             name,
		version:          "0.1.0",
		tools:            []string{toolName},
		providesServices: provides,
		requiresServices: requires,
	})
	return entryFor(name, dir, nil, toolName)
}

// applyErr runs one convergence and RETURNS whatever it reports, for the
// tests whose subject is the refusal itself.
func (h *harness) applyErr(entries ...manifest.Entry) error {
	h.t.Helper()

	return h.loader.Apply(context.Background(), manifest.Deployment{Plugins: entries}, h.root)
}

// TestAConsumerIsSuspendedWhileNobodyProvidesItsService is the base case: the
// consumer is mounted and its tools are NOT in the registry, because the
// capability it depends on has no provider.
func TestAConsumerIsSuspendedWhileNobodyProvidesItsService(t *testing.T) {
	h := newHarness(t)
	consumer := h.writeServicePkg("svccons", svcConsumerName, svcConsumerTool, nil, []string{issueTracker})

	h.apply(consumer)

	h.wantState(svcConsumerName, StateSuspended, "service:"+issueTracker)
	wantStrings(t, "registry while the service is unprovided", h.toolNames(), nil)
}

// TestAConsumerResumesWhenAProviderArrives: the provider does not have to be
// the plugin the consumer was written against — that is the whole point of
// binding to a capability name.
func TestAConsumerResumesWhenAProviderArrives(t *testing.T) {
	h := newHarness(t)
	consumer := h.writeServicePkg("svccons", svcConsumerName, svcConsumerTool, nil, []string{issueTracker})
	provider := h.writeServicePkg("svcprov", svcProviderName, svcProviderTool, []string{issueTracker}, nil)

	h.apply(consumer)
	h.wantState(svcConsumerName, StateSuspended, "service:"+issueTracker)

	h.apply(consumer, provider)

	h.wantState(svcConsumerName, StateLoaded)
	h.wantState(svcProviderName, StateLoaded)
	wantStrings(t, "registry once the service is provided", h.toolNames(),
		[]string{svcConsumerTool, svcProviderTool})
}

// TestASecondProviderOfTheSameServiceFailsToActivate is the decision the spec
// records: first come, first served, exactly one holder. A second claimant
// neither steps aside silently (which would leave "who provides this?"
// unanswerable while both report loaded) nor takes over silently (which is the
// risk that ruled out install-time displacement).
func TestASecondProviderOfTheSameServiceFailsToActivate(t *testing.T) {
	h := newHarness(t)
	first := h.writeServicePkg("svcprov", svcProviderName, svcProviderTool, []string{issueTracker}, nil)
	rival := h.writeServicePkg("svcrival", svcRivalName, svcRivalTool, []string{issueTracker}, nil)

	// A conflicting claim is reported by Apply AND recorded on the row: the
	// deployment as a whole did not fully converge, and the operator needs
	// both halves.
	if err := h.applyErr(first, rival); err == nil {
		t.Fatal("Apply with two claimants = nil error, want the conflict reported")
	}

	h.wantState(svcProviderName, StateLoaded)
	row := h.statusOf(svcRivalName)
	if row.State != StateFailed {
		t.Fatalf("rival state = %q, want %q (LastError %q)", row.State, StateFailed, row.LastError)
	}
	for _, want := range []string{issueTracker, svcProviderName} {
		if !strings.Contains(row.LastError, want) {
			t.Errorf("rival LastError = %q, want it to name %q", row.LastError, want)
		}
	}
	// The holder keeps its contributions; a conflict must not cost the plugin
	// that was there first anything.
	wantStrings(t, "registry after the conflict", h.toolNames(), []string{svcProviderTool})
}

// TestTheServiceHolderIsDecidedByDeclarationOrder: "first" must mean the order
// the deployment declared, not the order mounting happened to finish in —
// otherwise the same deployment could hand the service to different plugins on
// two starts of the same machine.
func TestTheServiceHolderIsDecidedByDeclarationOrder(t *testing.T) {
	h := newHarness(t)
	first := h.writeServicePkg("svcprov", svcProviderName, svcProviderTool, []string{issueTracker}, nil)
	rival := h.writeServicePkg("svcrival", svcRivalName, svcRivalTool, []string{issueTracker}, nil)

	// Declared in the other order: the rival is now the one that gets it.
	if err := h.applyErr(rival, first); err == nil {
		t.Fatal("Apply with two claimants = nil error, want the conflict reported")
	}

	h.wantState(svcRivalName, StateLoaded)
	if row := h.statusOf(svcProviderName); row.State != StateFailed {
		t.Fatalf("second-declared provider state = %q, want %q", row.State, StateFailed)
	}
}

// TestAServiceCycleIsReported: two plugins that need each other's service form
// an activation order that does not exist. It is operator data (bad
// manifests), so it is an error naming the plugins, not a panic.
func TestAServiceCycleIsReported(t *testing.T) {
	h := newHarness(t)
	left := h.writeServicePkg("svcleft", svcProviderName, svcProviderTool,
		[]string{issueTracker}, []string{"calendar"})
	right := h.writeServicePkg("svcright", svcConsumerName, svcConsumerTool,
		[]string{"calendar"}, []string{issueTracker})

	err := h.applyErr(left, right)
	if err == nil {
		t.Fatal("Apply with a service cycle = nil error, want a refusal")
	}
	for _, want := range []string{svcProviderName, svcConsumerName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}
