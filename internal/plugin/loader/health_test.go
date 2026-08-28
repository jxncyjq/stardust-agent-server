package loader

import (
	"testing"

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
