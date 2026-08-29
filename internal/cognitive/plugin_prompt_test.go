package cognitive

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/prompt"
)

// A plugin's prompt segment is a DEPLOYMENT-level fact: it is the same for
// every task this process runs, so it belongs in the cache-stable prefix. Get
// that wrong and every task pays for a cache miss it did not need.

func promptRequest() Request {
	return Request{
		Agent: domain.Agent{ID: "a1", Role: "developer"},
		Task:  domain.Task{ID: "t1", Input: "do the thing"},
		Tools: []string{"read_file"},
	}
}

func TestPluginSegmentsLandInsideTheStablePrefix(t *testing.T) {
	segments := prompt.NewSegments(nil)
	segments.Add("legion-jira", "Prefer ticket links over ticket numbers.")
	core := NewCore(NoopCompressor{}).WithPluginSegments(segments)

	built, err := core.BuildContext(context.Background(), promptRequest())
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	if !strings.Contains(built.Prompt, "Prefer ticket links over ticket numbers.") {
		t.Fatalf("prompt = %q, want the plugin's segment in it", built.Prompt)
	}
	// StablePrefixLen is measured in runes, so the prefix is sliced in runes.
	stable := string([]rune(built.Prompt)[:built.StablePrefixLen])
	if !strings.Contains(stable, "Prefer ticket links over ticket numbers.") {
		t.Errorf("the plugin segment is outside the stable prefix; stable part = %q", stable)
	}
	if !strings.Contains(stable, `--- plugin "legion-jira"`) {
		t.Errorf("the boundary marker is outside the stable prefix; stable part = %q", stable)
	}
}

// TestPluginSegmentsAreReportedAsTheirOwnBlock: prompt growth has to be
// attributable. A deployment whose prompt grew by 2 KB should be able to see
// that a plugin did it.
func TestPluginSegmentsAreReportedAsTheirOwnBlock(t *testing.T) {
	segments := prompt.NewSegments(nil)
	segments.Add("legion-jira", "Prefer ticket links.")
	core := NewCore(NoopCompressor{}).WithPluginSegments(segments)

	built, err := core.BuildContext(context.Background(), promptRequest())
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	var found bool
	for _, block := range built.Blocks {
		if block.Name == "plugin_prompt" {
			found = true
			if block.Chars == 0 {
				t.Error("plugin_prompt block reports 0 chars")
			}
		}
	}
	if !found {
		t.Errorf("blocks = %+v, want one named plugin_prompt", built.Blocks)
	}
}

// TestTwoBuildsProduceTheSameStablePrefix is the property the whole placement
// exists for: same deployment, different task, byte-identical prefix.
func TestTwoBuildsProduceTheSameStablePrefix(t *testing.T) {
	segments := prompt.NewSegments(nil)
	segments.Add("b-plugin", "B text")
	segments.Add("a-plugin", "A text")
	core := NewCore(NoopCompressor{}).WithPluginSegments(segments)

	first, err := core.BuildContext(context.Background(), promptRequest())
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	other := promptRequest()
	other.Task = domain.Task{ID: "t2", Input: "a different task"}
	second, err := core.BuildContext(context.Background(), other)
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	firstStable := string([]rune(first.Prompt)[:first.StablePrefixLen])
	secondStable := string([]rune(second.Prompt)[:second.StablePrefixLen])
	if firstStable != secondStable {
		t.Errorf("stable prefixes differ between tasks:\n%q\nvs\n%q", firstStable, secondStable)
	}
}

// TestNoPluginSegmentsAddNothing: a deployment with no plugin prompt must not
// pay a single character for the feature.
func TestNoPluginSegmentsAddNothing(t *testing.T) {
	withStore := NewCore(NoopCompressor{}).WithPluginSegments(prompt.NewSegments(nil))
	withoutStore := NewCore(NoopCompressor{})

	first, err := withStore.BuildContext(context.Background(), promptRequest())
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	second, err := withoutStore.BuildContext(context.Background(), promptRequest())
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if first.Prompt != second.Prompt {
		t.Errorf("an empty segment store changed the prompt:\n%q\nvs\n%q", first.Prompt, second.Prompt)
	}
}
