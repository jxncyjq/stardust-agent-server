package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"io"
	"log/slog"

	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/prompt"
)

// testDiscardLogger is the logger these tests hand to code that only uses one
// for warnings; the warnings themselves are asserted in internal/prompt.
func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The prompt segment is the one extension whose effect is not a call: the host
// asks once, at activation, and renders the answer into every prompt until the
// plugin goes away. These tests pin what that asking must guarantee.

func promptSpec(segments *prompt.Segments, extensions perm.Extensions) Spec {
	deps := Deps{PluginName: "legion-jira", Logger: testDiscardLogger()}
	deps.PromptSegments = segments
	return Spec{Name: "legion-jira", Extensions: extensions, Deps: deps}
}

func TestActivationAsksTheGuestOnceForItsPromptSegment(t *testing.T) {
	segments := prompt.NewSegments(testDiscardLogger())
	asked := 0
	guest := guestCallerFunc(func(_ context.Context, op int32, in []byte) ([]byte, error) {
		if op != abi.OpPromptSegment {
			t.Errorf("guest received op %d, want %d", op, abi.OpPromptSegment)
		}
		if len(in) != 0 {
			t.Errorf("request body = %q, want empty: the host is asking, not telling", in)
		}
		asked++
		return []byte(`{"text":"Prefer ticket links over ticket numbers."}`), nil
	})

	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:legion-jira")
	if err := contributePromptSegment(context.Background(), ledger, owner,
		promptSpec(segments, perm.Extensions{Prompt: true}), guest, func(func() error) {}); err != nil {
		t.Fatalf("contributePromptSegment: %v", err)
	}

	if asked != 1 {
		t.Errorf("guest asked %d times, want exactly 1: the answer is a deployment-level fact", asked)
	}
	if !strings.Contains(segments.Render(), "Prefer ticket links over ticket numbers.") {
		t.Errorf("rendered = %q, want the guest's text", segments.Render())
	}
}

// TestAFailedPromptSegmentRefusesTheActivation: a plugin granted the prompt
// extension that cannot produce its text must not mount. Mounting it would
// give the deployment the plugin's tools and silently none of the
// instructions telling the model how to use them.
func TestAFailedPromptSegmentRefusesTheActivation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		err  error
	}{
		{name: "trap", err: errors.New("guest trapped")},
		{name: "empty body", body: nil},
		{name: "unreadable body", body: []byte(`not json`)},
		{name: "unknown field", body: []byte(`{"text":"hi","priority":1}`)},
		{name: "trailing data", body: []byte(`{"text":"hi"}{"text":"again"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segments := prompt.NewSegments(testDiscardLogger())
			guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
				return tc.body, tc.err
			})

			err := contributePromptSegment(context.Background(), lifecycle.NewLedger(),
				lifecycle.Owner("plugin:legion-jira"), promptSpec(segments, perm.Extensions{Prompt: true}),
				guest, func(func() error) {})
			if err == nil {
				t.Fatal("contributePromptSegment = nil error, want the activation refused")
			}
			if segments.Render() != "" {
				t.Errorf("rendered = %q, want nothing from a plugin that failed to answer", segments.Render())
			}
		})
	}
}

// TestAGrantedPromptWithNoStoreIsAWiringFailure: the embedder forgot to hand
// the host a segment store. Mounting anyway would put the plugin's text
// nowhere while reporting success.
func TestAGrantedPromptWithNoStoreIsAWiringFailure(t *testing.T) {
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		t.Error("the guest was asked for a segment with no store to put it in")
		return []byte(`{"text":"hi"}`), nil
	})

	err := contributePromptSegment(context.Background(), lifecycle.NewLedger(),
		lifecycle.Owner("plugin:legion-jira"), promptSpec(nil, perm.Extensions{Prompt: true}),
		guest, func(func() error) {})
	if err == nil {
		t.Fatal("contributePromptSegment with no store = nil error, want a wiring failure")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("error = %v, want it to name the extension that was granted", err)
	}
}

// TestAnEmptyPromptSegmentIsAValidAnswer: a plugin may decide it has nothing
// to say for this deployment's configuration. That is not a failure, and it
// must not mount an empty fence either.
func TestAnEmptyPromptSegmentIsAValidAnswer(t *testing.T) {
	segments := prompt.NewSegments(testDiscardLogger())
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return []byte(`{"text":""}`), nil
	})

	if err := contributePromptSegment(context.Background(), lifecycle.NewLedger(),
		lifecycle.Owner("plugin:legion-jira"), promptSpec(segments, perm.Extensions{Prompt: true}),
		guest, func(func() error) {}); err != nil {
		t.Fatalf("contributePromptSegment: %v", err)
	}
	if got := segments.Render(); got != "" {
		t.Errorf("rendered = %q, want nothing", got)
	}
}

// TestDisposingTheOwnerTakesTheSegmentOutOfThePrompt is the withdrawal half:
// a plugin whose contributions were withdrawn but whose text still steers the
// model is a plugin the deployment believes it has disabled.
func TestDisposingTheOwnerTakesTheSegmentOutOfThePrompt(t *testing.T) {
	segments := prompt.NewSegments(testDiscardLogger())
	guest := guestCallerFunc(func(context.Context, int32, []byte) ([]byte, error) {
		return []byte(`{"text":"Prefer ticket links."}`), nil
	})
	ledger := lifecycle.NewLedger()
	owner := lifecycle.Owner("plugin:legion-jira")

	if err := contributePromptSegment(context.Background(), ledger, owner,
		promptSpec(segments, perm.Extensions{Prompt: true}), guest, func(func() error) {}); err != nil {
		t.Fatalf("contributePromptSegment: %v", err)
	}
	if segments.Render() == "" {
		t.Fatal("nothing was rendered before disposal")
	}
	if err := ledger.DisposeOwner(owner); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if got := segments.Render(); got != "" {
		t.Errorf("rendered = %q after disposal, want empty", got)
	}
}

// TestContributeAllFilesThePromptSegmentOnlyWhenGranted is the wiring test:
// activation's one contribution step has to reach the prompt seam, and only
// when the deployment granted it. Without this, the seam works and nothing
// calls it.
func TestContributeAllFilesThePromptSegmentOnlyWhenGranted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extensions perm.Extensions
		wantText   bool
	}{
		{name: "granted", extensions: perm.Extensions{Prompt: true}, wantText: true},
		{name: "not granted", extensions: perm.Extensions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segments := prompt.NewSegments(testDiscardLogger())
			asked := 0
			guest := guestCallerFunc(func(_ context.Context, op int32, _ []byte) ([]byte, error) {
				if op == abi.OpPromptSegment {
					asked++
				}
				return []byte(`{"text":"Prefer ticket links."}`), nil
			})
			spec := promptSpec(segments, tc.extensions)
			spec.Registry = observingRegistry()

			ledger := lifecycle.NewLedger()
			owner := lifecycle.Owner("plugin:legion-jira")
			if err := contributeAll(context.Background(), ledger, owner, spec, guest, func(func() error) {}); err != nil {
				t.Fatalf("contributeAll: %v", err)
			}
			t.Cleanup(func() {
				if err := ledger.DisposeOwner(owner); err != nil {
					t.Errorf("dispose owner: %v", err)
				}
			})

			if got := segments.Render() != ""; got != tc.wantText {
				t.Errorf("rendered something = %t, want %t", got, tc.wantText)
			}
			if wantAsked := 0; !tc.wantText && asked != wantAsked {
				t.Errorf("guest asked for a segment %d times without the grant, want %d", asked, wantAsked)
			}
		})
	}
}
