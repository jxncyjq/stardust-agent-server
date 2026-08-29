package host

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/tool"
)

// A granted extension is a seam the host will actually call the guest at. If
// the guest does not implement it, the grant is a lie the deployment tells
// itself: the plugin is consulted on every tool call, answers "unsupported
// op" every time, and nothing anywhere says so. The guest's own manifest is
// the only place that mismatch is visible, so activation cross-checks it —
// exactly as it already does for tool names.

func extensionSpec(extensions perm.Extensions) Spec {
	return Spec{
		Name:       "legion-observer",
		Extensions: extensions,
		Tools:      []tool.Descriptor{{Name: "observed_tool"}},
	}
}

func extensionManifest(extensions []string) Manifest {
	return Manifest{
		Name:       "legion-observer",
		Version:    "1.0.0",
		Provides:   []string{"observed_tool"},
		Extensions: extensions,
	}
}

func TestCrossCheckRefusesAGrantedExtensionTheGuestDoesNotImplement(t *testing.T) {
	err := crossCheck(extensionSpec(perm.Extensions{Observe: true}), extensionManifest(nil))
	if err == nil {
		t.Fatal("crossCheck with a granted-but-unimplemented extension = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "observe") {
		t.Errorf("error = %v, want it to name the extension", err)
	}
}

func TestCrossCheckAcceptsAGrantedExtensionTheGuestImplements(t *testing.T) {
	if err := crossCheck(extensionSpec(perm.Extensions{Observe: true}),
		extensionManifest([]string{"observe"})); err != nil {
		t.Errorf("crossCheck = %v, want nil", err)
	}
}

// TestCrossCheckAcceptsAGuestThatImplementsMoreThanItWasGranted: a plugin may
// ship an observer and be deployed without the grant. It is simply never
// called — that is what an ungranted extension MEANS — and refusing to mount
// it would make "declares an observer" an all-or-nothing property of the
// binary rather than a decision the deployment makes.
func TestCrossCheckAcceptsAGuestThatImplementsMoreThanItWasGranted(t *testing.T) {
	if err := crossCheck(extensionSpec(perm.Extensions{}),
		extensionManifest([]string{"observe"})); err != nil {
		t.Errorf("crossCheck = %v, want nil", err)
	}
}
