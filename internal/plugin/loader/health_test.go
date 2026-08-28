package loader

import (
	"context"
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
