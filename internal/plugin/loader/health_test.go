package loader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"

	"github.com/stardust/legion-agent/internal/plugin/host"
)

// The tests below pin the fault-counting semantics of the runtime-health work
// (plans/2026-08-28-plugin-runtime-health.md). They drive the counter directly
// rather than through a failing guest: what needs pinning here is the
// arithmetic and its three exclusions, and a fixture that traps on demand would
// prove the same thing more slowly and less precisely.
//
// The threshold is set on the loader the harness already built. It is the
// deployment's policy, not the harness's, and every one of these tests needs a
// different one.

func TestRecordFaultUnloadsAtTheThreshold(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 3
	h.apply(h.writeEcho("1.0.0", "echo_tool"))

	for i := 1; i < 3; i++ {
		if unload := h.loader.recordFault(echoPluginName, host.CategoryTrap); unload {
			t.Fatalf("fault %d of 3 asked for an unload; want it to wait for the threshold", i)
		}
	}
	if unload := h.loader.recordFault(echoPluginName, host.CategoryTrap); !unload {
		t.Error("the 3rd consecutive fault did not ask for an unload, want it to")
	}
}

func TestRecordFaultResetsOnSuccess(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 2
	h.apply(h.writeEcho("1.0.0", "echo_tool"))

	h.loader.recordFault(echoPluginName, host.CategoryTimeout)
	h.loader.recordSuccess(echoPluginName)
	if unload := h.loader.recordFault(echoPluginName, host.CategoryTimeout); unload {
		t.Error("a fault after a success asked for an unload; the counter must have reset")
	}
}

func TestRecordFaultIgnoresDenials(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 1
	h.apply(h.writeEcho("1.0.0", "echo_tool"))

	if unload := h.loader.recordFault(echoPluginName, host.CategoryDenied); unload {
		t.Error("a denial asked for an unload; a denial means the plugin overstepped, not that it is broken")
	}
}

// TestRecordFaultIgnoresAPluginThatIsNoLongerMounted covers the case a
// convergence creates: a call that was in flight when its plugin was replaced
// reports its failure afterwards. Counting it would charge the new mount for
// the old one's failure.
func TestRecordFaultIgnoresAPluginThatIsNoLongerMounted(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 1

	if unload := h.loader.recordFault("never-mounted", host.CategoryTrap); unload {
		t.Error("a fault for an unmounted plugin asked for an unload, want it dropped")
	}
}

// TestUnhealthyPluginIsUnloadedAndSaysWhy is the payoff of the counting above:
// crossing the threshold must actually unmount the plugin AND leave an
// explanation an operator can act on. An unload nobody can explain is worse
// than no unload — the tool simply vanishes from the model's list.
func TestUnhealthyPluginIsUnloadedAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 1
	h.apply(h.writeEcho("1.0.0", "echo_tool"))

	h.loader.unloadUnhealthy(context.Background(), echoPluginName, host.CategoryTrap, "echo_tool",
		"invoke op=1: guest trap")

	statuses := h.loader.Status()
	if len(statuses) != 1 {
		t.Fatalf("Status() = %d rows, want 1", len(statuses))
	}
	if statuses[0].State != StateFailed {
		t.Errorf("state = %q, want %q", statuses[0].State, StateFailed)
	}
	if !strings.Contains(statuses[0].LastError, "health") ||
		!strings.Contains(statuses[0].LastError, host.CategoryTrap) {
		t.Errorf("LastError = %q, want it to name the health policy and the category", statuses[0].LastError)
	}

	var unloaded []domain.RuntimeEvent
	for _, event := range h.eventsOfType(RuntimeEventUnloaded) {
		if strings.Contains(event.Message, "reason=health") {
			unloaded = append(unloaded, event)
		}
	}
	if len(unloaded) != 1 {
		t.Errorf("plugin/unloaded events with reason=health = %d, want 1: an automatic unload must be visible",
			len(unloaded))
	}
}

// TestUnhealthyUnloadDoesNotBringThePluginBack pins the design decision not to
// retry: a plugin that trapped its way to the threshold will usually trap
// again, and an automatic reload would turn one visible unload into an
// invisible loop. Getting it back is `agent plugins reload`.
func TestUnhealthyUnloadDoesNotBringThePluginBack(t *testing.T) {
	h := newHarness(t)
	h.loader.maxConsecutiveFaults = 1
	h.apply(h.writeEcho("1.0.0", "echo_tool"))

	h.loader.unloadUnhealthy(context.Background(), echoPluginName, host.CategoryTrap, "echo_tool", "boom")

	if names := h.toolNames(); len(names) != 0 {
		t.Errorf("tools still registered after a health unload: %v; there is no automatic retry by design", names)
	}
}

// TestUnloadPublishesLeakedWhenDrainDoesNotConverge covers the design doc's
// fifth event (§8), the only one that had never been emitted: an unload whose
// wait for in-flight calls ran out leaves guest work running inside a runtime
// nobody owns any more, and until now that fact appeared nowhere.
//
// The drain failure is injected rather than provoked: making a real fixture
// hang a call past the drain deadline would test wazero's scheduling, not this
// branch. What this pins is that the Loader recognises host.ErrDrainIncomplete
// and reports the COUNT — the number is the whole point of the event.
func TestUnloadPublishesLeakedWhenDrainDoesNotConverge(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0", "echo_tool"))
	inst := h.loader.mounted(echoPluginName)

	leaked := fmt.Errorf("drain instance pool of plugin %q (waited 5s): %w",
		echoPluginName, host.ErrDrainIncomplete)
	h.loader.reportDrainLeak(context.Background(), inst, leaked)

	events := h.eventsOfType(RuntimeEventUnloadLeaked)
	if len(events) != 1 {
		t.Fatalf("plugin/unload_leaked events = %d, want 1: a drain that left calls behind must say so",
			len(events))
	}
	if !strings.Contains(events[0].Message, "inflight=") {
		t.Errorf("event %q does not report how many calls were left behind", events[0].Message)
	}
	if !strings.Contains(events[0].Message, echoPluginName) {
		t.Errorf("event %q does not name the plugin", events[0].Message)
	}
}

// TestUnloadPublishesNoLeakOnAnOrdinaryFailure is the other half: a disposal
// that failed for some other reason already travels out as an error, and
// reporting it as a leak would tell an operator to go looking for guest work
// that is not there.
func TestUnloadPublishesNoLeakOnAnOrdinaryFailure(t *testing.T) {
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0", "echo_tool"))
	inst := h.loader.mounted(echoPluginName)

	h.loader.reportDrainLeak(context.Background(), inst, errors.New("close wasm runtime: already closed"))

	if events := h.eventsOfType(RuntimeEventUnloadLeaked); len(events) != 0 {
		t.Errorf("plugin/unload_leaked events for an unrelated disposal failure = %d, want 0", len(events))
	}
}
