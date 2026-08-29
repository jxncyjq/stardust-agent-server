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

// TestStatusReportsTheServicesAPluginProvidesAndNeeds: when a service chain
// does not come up, the two questions an operator has are "which capability is
// this plugin holding" and "which is it waiting for". A row that omitted them
// would send them to read plugin.json on disk.
func TestStatusReportsTheServicesAPluginProvidesAndNeeds(t *testing.T) {
	h := newHarness(t)
	consumer := h.writeServicePkg("svccons", svcConsumerName, svcConsumerTool, nil, []string{issueTracker})
	provider := h.writeServicePkg("svcprov", svcProviderName, svcProviderTool, []string{issueTracker}, nil)

	h.apply(consumer, provider)

	wantStrings(t, "provider ProvidesServices", h.statusOf(svcProviderName).ProvidesServices, []string{issueTracker})
	wantStrings(t, "consumer RequiresServices", h.statusOf(svcConsumerName).RequiresServices, []string{issueTracker})
}

// TestAServiceConsumerCascadesWhenItsProviderIsSuspended: the provider is
// mounted but suspended (its own tool dependency is missing), so the consumer
// is suspended too — and the diagnosis has to point one hop up the chain
// rather than at a missing installation.
func TestAServiceConsumerCascadesWhenItsProviderIsSuspended(t *testing.T) {
	h := newHarness(t)
	// The provider itself needs a tool nobody has.
	writePackage(t, filepath.Join(h.root, "svcprov"), pkg{
		wasm:             patchIdentity(t, svcProviderName, svcProviderTool),
		name:             svcProviderName,
		version:          "0.1.0",
		tools:            []string{svcProviderTool},
		requires:         []string{"absent_tool"},
		providesServices: []string{issueTracker},
	})
	provider := entryFor(svcProviderName, "svcprov", nil, svcProviderTool)
	consumer := h.writeServicePkg("svccons", svcConsumerName, svcConsumerTool, nil, []string{issueTracker})

	h.apply(provider, consumer)

	h.wantState(svcProviderName, StateSuspended, "absent_tool")
	h.wantState(svcConsumerName, StateSuspended, "service:"+issueTracker)
	wantStrings(t, "registry with the whole chain down", h.toolNames(), nil)
}

// The resolver is what turns "service:issue-tracker/search" into whoever's
// tool is behind it right now. Its answers change as plugins mount, suspend
// and unload — which is why it is asked per call rather than captured when a
// consumer was activated.

// writeServiceProviderPkg writes a provider that also maps capabilities onto
// its own tool.
func (h *harness) writeServiceProviderPkg(dir, name, toolName, service, capability string) manifest.Entry {
	h.t.Helper()

	writePackage(h.t, filepath.Join(h.root, dir), pkg{
		wasm:                patchIdentity(h.t, name, toolName),
		name:                name,
		version:             "0.1.0",
		tools:               []string{toolName},
		providesServices:    []string{service},
		serviceCapabilities: map[string]map[string]string{service: {capability: toolName}},
	})
	return entryFor(name, dir, nil, toolName)
}

func TestResolveServiceAnswersWithTheCurrentProvidersTool(t *testing.T) {
	h := newHarness(t)
	provider := h.writeServiceProviderPkg("svcprov", svcProviderName, svcProviderTool, issueTracker, "search")

	h.apply(provider)

	got, err := h.loader.ResolveService(issueTracker, "search")
	if err != nil {
		t.Fatalf("ResolveService: %v", err)
	}
	if got != svcProviderTool {
		t.Errorf("ResolveService = %q, want %q", got, svcProviderTool)
	}
}

// TestResolveServiceFollowsAProviderSwap is the point of the whole seam: the
// same service name, a different plugin behind it, and nothing on the consumer
// side changed.
func TestResolveServiceFollowsAProviderSwap(t *testing.T) {
	h := newHarness(t)
	first := h.writeServiceProviderPkg("svcprov", svcProviderName, svcProviderTool, issueTracker, "search")
	second := h.writeServiceProviderPkg("svcrival", svcRivalName, svcRivalTool, issueTracker, "search")

	h.apply(first)
	got, err := h.loader.ResolveService(issueTracker, "search")
	if err != nil || got != svcProviderTool {
		t.Fatalf("ResolveService before the swap = (%q, %v), want %q", got, err, svcProviderTool)
	}

	// The deployment drops the first provider and declares the second.
	h.apply(second)

	got, err = h.loader.ResolveService(issueTracker, "search")
	if err != nil {
		t.Fatalf("ResolveService after the swap: %v", err)
	}
	if got != svcRivalTool {
		t.Errorf("ResolveService = %q, want the new provider's tool %q", got, svcRivalTool)
	}
}

func TestResolveServiceRefusesWhatItCannotAnswer(t *testing.T) {
	h := newHarness(t)
	provider := h.writeServiceProviderPkg("svcprov", svcProviderName, svcProviderTool, issueTracker, "search")
	h.apply(provider)

	if _, err := h.loader.ResolveService("calendar", "list"); err == nil {
		t.Error("ResolveService for an unprovided service = nil error, want a refusal")
	}
	_, err := h.loader.ResolveService(issueTracker, "comment")
	if err == nil {
		t.Fatal("ResolveService for an unexposed capability = nil error, want a refusal")
	}
	for _, want := range []string{svcProviderName, "comment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// TestResolveServiceRefusesASuspendedProvider: a suspended plugin's tools are
// withdrawn from the registry, so resolving to one would hand back a name that
// is about to fail as unknown.
func TestResolveServiceRefusesASuspendedProvider(t *testing.T) {
	h := newHarness(t)
	writePackage(t, filepath.Join(h.root, "svcprov"), pkg{
		wasm:                patchIdentity(t, svcProviderName, svcProviderTool),
		name:                svcProviderName,
		version:             "0.1.0",
		tools:               []string{svcProviderTool},
		requires:            []string{"absent_tool"},
		providesServices:    []string{issueTracker},
		serviceCapabilities: map[string]map[string]string{issueTracker: {"search": svcProviderTool}},
	})
	provider := entryFor(svcProviderName, "svcprov", nil, svcProviderTool)

	h.apply(provider)
	h.wantState(svcProviderName, StateSuspended, "absent_tool")

	_, err := h.loader.ResolveService(issueTracker, "search")
	if err == nil {
		t.Fatal("ResolveService with a suspended provider = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Errorf("error = %v, want it to say the provider is suspended", err)
	}
}

// TestAMountedPluginCanResolveServicesThroughItsDeps is the wiring test: the
// Loader implementing host.ServiceResolver proves nothing if no mounted plugin
// is handed it. Without this, a guest's "service:x/y" call_tool would be told
// this deployment has no service resolver — while the loader sitting right
// there knew the answer.
func TestAMountedPluginCanResolveServicesThroughItsDeps(t *testing.T) {
	h := newHarness(t)
	provider := h.writeServiceProviderPkg("svcprov", svcProviderName, svcProviderTool, issueTracker, "search")

	h.apply(provider)

	inst := h.loader.instances[svcProviderName]
	if inst == nil {
		t.Fatalf("no instance for %q", svcProviderName)
	}
	if inst.spec.Deps.Services == nil {
		t.Fatal("the mounted plugin's Deps carries no service resolver; its call_tool could never use a service name")
	}
	got, err := inst.spec.Deps.Services.ResolveService(issueTracker, "search")
	if err != nil {
		t.Fatalf("ResolveService through Deps: %v", err)
	}
	if got != svcProviderTool {
		t.Errorf("ResolveService through Deps = %q, want %q", got, svcProviderTool)
	}
}
