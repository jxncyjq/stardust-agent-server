package prompt

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/lifecycle"
)

// A plugin's prompt segment is UNTRUSTED TEXT rendered into the system prompt.
// Every test here pins one of the four things that makes that survivable: it
// is fenced, it is bounded, it is ordered, and it goes away with the plugin.

func newTestSegments(t *testing.T) (*Segments, *bytes.Buffer) {
	t.Helper()

	var logged bytes.Buffer
	return NewSegments(slog.New(slog.NewTextHandler(&logged, nil))), &logged
}

func TestRenderFencesEachSegmentWithItsPluginName(t *testing.T) {
	segments, _ := newTestSegments(t)
	segments.Add("legion-jira", "Prefer ticket links over ticket numbers.")

	rendered := segments.Render()

	if !strings.Contains(rendered, `--- plugin "legion-jira" (untrusted, provided by a deployment-installed plugin) ---`) {
		t.Errorf("rendered = %q, want an opening boundary naming the plugin", rendered)
	}
	if !strings.Contains(rendered, `--- end plugin "legion-jira" ---`) {
		t.Errorf("rendered = %q, want a closing boundary", rendered)
	}
	if !strings.Contains(rendered, "Prefer ticket links over ticket numbers.") {
		t.Errorf("rendered = %q, want the plugin's own text", rendered)
	}
	// The marker exists so the model can tell host instructions from plugin
	// ones. Text that arrives BEFORE the opening fence would be indistinguish-
	// able from the host's own words.
	if strings.Index(rendered, "Prefer ticket links") < strings.Index(rendered, "--- plugin") {
		t.Error("the plugin's text appears before its opening boundary")
	}
}

func TestRenderIsEmptyWithNoSegments(t *testing.T) {
	segments, _ := newTestSegments(t)
	if got := segments.Render(); got != "" {
		t.Errorf("Render() = %q, want empty: an empty block would still cost tokens", got)
	}
}

// TestRenderOrdersByPluginNameNotRegistrationOrder is what makes the stable
// prefix stable: the same deployment must render byte-identically on every
// start, and mount order is not something a deployment controls.
func TestRenderOrdersByPluginNameNotRegistrationOrder(t *testing.T) {
	first, _ := newTestSegments(t)
	first.Add("b-plugin", "B text")
	first.Add("a-plugin", "A text")

	second, _ := newTestSegments(t)
	second.Add("a-plugin", "A text")
	second.Add("b-plugin", "B text")

	if first.Render() != second.Render() {
		t.Errorf("render depends on registration order:\n%q\nvs\n%q", first.Render(), second.Render())
	}
	if strings.Index(first.Render(), "a-plugin") > strings.Index(first.Render(), "b-plugin") {
		t.Error("segments are not sorted by plugin name")
	}
}

// TestAnOversizedSegmentIsTruncatedVisiblyAndLogged: silent truncation would
// let an author believe the whole block reached the model. The marker is in
// the text AND the fact is in the log, because those reach two different
// people.
func TestAnOversizedSegmentIsTruncatedVisiblyAndLogged(t *testing.T) {
	segments, logged := newTestSegments(t)
	oversized := strings.Repeat("x", MaxSegmentRunes+500)

	segments.Add("legion-verbose", oversized)
	rendered := segments.Render()

	if len([]rune(rendered)) > MaxSegmentRunes+300 { // + the fences and the marker
		t.Errorf("rendered %d runes, want the segment capped near %d", len([]rune(rendered)), MaxSegmentRunes)
	}
	if !strings.Contains(rendered, "truncated") {
		t.Errorf("rendered = %q…, want a visible truncation marker", rendered[:120])
	}
	if !strings.Contains(logged.String(), "legion-verbose") {
		t.Errorf("log = %q, want a warning naming the plugin whose segment was cut", logged.String())
	}
}

// TestSegmentsBeyondTheTotalBudgetAreRefusedNotSilentlyDropped: the whole
// point of a total cap is that somebody has to be told which plugin lost.
func TestSegmentsBeyondTheTotalBudgetAreRefusedNotSilentlyDropped(t *testing.T) {
	segments, logged := newTestSegments(t)
	body := strings.Repeat("y", MaxSegmentRunes)
	// Four maximum-size segments exceed the total budget of MaxTotalRunes.
	for _, name := range []string{"p1", "p2", "p3", "p4", "p5"} {
		segments.Add(name, body)
	}

	rendered := segments.Render()
	if len([]rune(rendered)) > MaxTotalRunes+1000 { // + fences per admitted segment
		t.Errorf("rendered %d runes, want at most about %d", len([]rune(rendered)), MaxTotalRunes)
	}
	if !strings.Contains(logged.String(), "p5") {
		t.Errorf("log = %q, want a warning naming a plugin whose segment did not fit", logged.String())
	}
	if strings.Contains(rendered, `--- plugin "p5"`) {
		t.Error("a segment past the total budget was rendered anyway")
	}
}

func TestRevokingASegmentRemovesIt(t *testing.T) {
	segments, _ := newTestSegments(t)
	revoke := segments.Add("legion-jira", "Prefer ticket links.")

	if segments.Render() == "" {
		t.Fatal("Render() is empty before revocation")
	}
	revoke()
	revoke() // idempotent, like the other seams' revocations
	if got := segments.Render(); got != "" {
		t.Errorf("Render() = %q after revocation, want empty", got)
	}
}

// TestContributeOwnedRemovesTheSegmentWithTheOwner: a plugin whose tools were
// withdrawn but whose text is still steering the model is a plugin the
// deployment believes it has disabled.
func TestContributeOwnedRemovesTheSegmentWithTheOwner(t *testing.T) {
	segments, _ := newTestSegments(t)
	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:legion-jira")
	ContributeOwned(ledger, owner, segments, "legion-jira", "Prefer ticket links.")

	if segments.Render() == "" {
		t.Fatal("Render() is empty before disposal")
	}
	if err := ledger.DisposeOwner(owner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if got := segments.Render(); got != "" {
		t.Errorf("Render() = %q after disposal, want empty", got)
	}
}

// TestAnEmptySegmentIsNotRegistered: a plugin that was granted the extension
// and returned nothing contributes nothing — rendering an empty fence would
// spend tokens saying a plugin had nothing to say.
func TestAnEmptySegmentIsNotRegistered(t *testing.T) {
	segments, _ := newTestSegments(t)
	segments.Add("legion-quiet", "   \n  ")

	if got := segments.Render(); got != "" {
		t.Errorf("Render() = %q, want empty for a whitespace-only segment", got)
	}
}
