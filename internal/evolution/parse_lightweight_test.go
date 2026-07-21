package evolution

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// Audit item V19: `lightweight, _ := strconv.ParseBool(fields["lightweight"])`
// turned an unparseable or missing value into false.
//
// The field is not optional: NewLearningRuntimeEvent in this same package always
// writes it as `lightweight=%t`. Missing or malformed therefore means the message
// was tampered with or the format drifted — and the cost of guessing is that a
// heavyweight failure signal is processed as a lightweight one, so the evolution
// path branches wrong and an important failure never triggers gene consolidation.
// The returned ok looked only at signal, hiding it completely.

func learningEvent(message string) domain.RuntimeEvent {
	return domain.RuntimeEvent{Type: RuntimeEventLearning, TaskID: "task-1", Message: message}
}

func TestParseLearningRuntimeEventRejectsMalformedLightweight(t *testing.T) {
	t.Parallel()

	_, ok := ParseLearningRuntimeEvent(learningEvent(
		"agent_id=a1 signal=failure reason=tool_error episodic_ref= lightweight=maybe"))
	if ok {
		t.Fatal("ParseLearningRuntimeEvent(lightweight=maybe) ok = true, want false")
	}
}

func TestParseLearningRuntimeEventRejectsMissingLightweight(t *testing.T) {
	t.Parallel()

	_, ok := ParseLearningRuntimeEvent(learningEvent(
		"agent_id=a1 signal=failure reason=tool_error episodic_ref="))
	if ok {
		t.Fatal("ParseLearningRuntimeEvent(no lightweight field) ok = true, want false")
	}
}

// TestParseLearningRuntimeEventAcceptsBothBooleans guards the other direction —
// the values the writer actually emits must keep parsing, in both directions.
func TestParseLearningRuntimeEventAcceptsBothBooleans(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want bool
	}{{"true", true}, {"false", false}} {
		got, ok := ParseLearningRuntimeEvent(learningEvent(
			"agent_id=a1 signal=failure reason=tool_error episodic_ref= lightweight=" + tc.raw))
		if !ok {
			t.Fatalf("ParseLearningRuntimeEvent(lightweight=%s) ok = false, want true", tc.raw)
		}
		if got.IsLightweight != tc.want {
			t.Errorf("IsLightweight = %t, want %t", got.IsLightweight, tc.want)
		}
	}
}

// TestParseLearningRuntimeEventRoundTripsWriterOutput pins writer and reader
// together: whatever NewLearningRuntimeEvent emits must parse.
func TestParseLearningRuntimeEventRoundTripsWriterOutput(t *testing.T) {
	t.Parallel()

	written := NewLearningRuntimeEvent(LearningEvent{
		AgentID: "researcher", TaskID: "task-1", Signal: SignalFailure,
		Reason: "tool_error", IsLightweight: true,
	})
	got, ok := ParseLearningRuntimeEvent(written)
	if !ok {
		t.Fatalf("round trip ok = false, want true (message = %q)", written.Message)
	}
	if !got.IsLightweight || got.AgentID != "researcher" || got.Signal != SignalFailure {
		t.Errorf("round trip = %#v, want the written values back", got)
	}
}
