package observability

import (
	"strings"
	"testing"
	"time"
)

func TestTraceRecorderSanitizesSpanAttributes(t *testing.T) {
	t.Parallel()
	recorder := NewTraceRecorder(TraceConfig{MaxSpans: 10})
	recorder.Record(Span{
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Name:      "model.generate",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Millisecond),
		Attributes: map[string]string{
			"component": "runtime",
			"prompt":    "secret prompt",
		},
	})

	snapshot := recorder.Snapshot()
	if len(snapshot.Spans) != 1 {
		t.Fatalf("Snapshot().Spans len = %d, want 1", len(snapshot.Spans))
	}
	got := snapshot.Spans[0].Attributes["prompt"]
	if strings.Contains(got, "secret prompt") {
		t.Fatalf("Snapshot().Spans[0].Attributes[prompt] = %q, want redacted", got)
	}
	if snapshot.Spans[0].Attributes["component"] != "runtime" {
		t.Fatalf("Snapshot().Spans[0].Attributes[component] = %q, want runtime", snapshot.Spans[0].Attributes["component"])
	}
}
