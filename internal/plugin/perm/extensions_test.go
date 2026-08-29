package perm

import "testing"

// The decide extension is the first one that can REFUSE a call, so the parse
// layer has to know it by name before anything downstream can be granted it.

func TestParseExtensionsAcceptsDecide(t *testing.T) {
	parsed, err := ParseExtensions([]string{"decide"})
	if err != nil {
		t.Fatalf("ParseExtensions([decide]) error = %v, want nil", err)
	}
	if !parsed.Decide {
		t.Error("ParseExtensions([decide]).Decide = false, want true")
	}
	if parsed.Observe {
		t.Error("ParseExtensions([decide]).Observe = true; granting one extension must not grant another")
	}
}

func TestExtensionsNamesReportsBothSeams(t *testing.T) {
	names := Extensions{Observe: true, Decide: true}.Names()
	if len(names) != 2 || names[0] != "decide" || names[1] != "observe" {
		t.Errorf("Names() = %v, want [decide observe] (sorted)", names)
	}
}

// TestExtensionsIntersectIsPerSeam: a plugin that asks for both and a
// deployment that grants one must end up with exactly the one. Intersecting
// the two as a single "any extension" flag would turn a partial grant into a
// full one.
func TestExtensionsIntersectIsPerSeam(t *testing.T) {
	declared := Extensions{Observe: true, Decide: true}
	granted := Extensions{Decide: true}

	got := declared.Intersect(granted)
	if got.Observe {
		t.Error("Intersect granted Observe, which the deployment did not grant")
	}
	if !got.Decide {
		t.Error("Intersect dropped Decide, which both sides named")
	}
}

func TestParseExtensionsAcceptsPrompt(t *testing.T) {
	parsed, err := ParseExtensions([]string{"prompt"})
	if err != nil {
		t.Fatalf("ParseExtensions([prompt]) error = %v, want nil", err)
	}
	if !parsed.Prompt || parsed.Observe || parsed.Decide {
		t.Errorf("ParseExtensions([prompt]) = %+v, want only Prompt", parsed)
	}
}

// TestExtensionsNamesReportsAllThreeSeams also guards the sorted, stable order
// the rendered prompt depends on downstream.
func TestExtensionsNamesReportsAllThreeSeams(t *testing.T) {
	names := Extensions{Observe: true, Decide: true, Prompt: true}.Names()
	if len(names) != 3 || names[0] != "decide" || names[1] != "observe" || names[2] != "prompt" {
		t.Errorf("Names() = %v, want [decide observe prompt]", names)
	}
}
