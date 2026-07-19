package observability

import (
	"maps"
	"strings"
	"sync"
	"time"
)

type TraceConfig struct {
	MaxSpans int
}

type Span struct {
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
	Name       string            `json:"name"`
	StartedAt  time.Time         `json:"started_at"`
	EndedAt    time.Time         `json:"ended_at"`
	Attributes map[string]string `json:"attributes"`
}

type TraceSnapshot struct {
	Spans []Span `json:"spans"`
}

type TraceRecorder struct {
	mu       sync.Mutex
	maxSpans int
	spans    []Span
}

func NewTraceRecorder(cfg TraceConfig) *TraceRecorder {
	maxSpans := cfg.MaxSpans
	if maxSpans <= 0 {
		maxSpans = 100
	}
	return &TraceRecorder{maxSpans: maxSpans}
}

func (r *TraceRecorder) Record(span Span) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	span.Attributes = sanitizeTraceAttributes(span.Attributes)
	r.spans = append(r.spans, span)
	if len(r.spans) > r.maxSpans {
		r.spans = r.spans[len(r.spans)-r.maxSpans:]
	}
}

func (r *TraceRecorder) Snapshot() TraceSnapshot {
	if r == nil {
		return TraceSnapshot{Spans: []Span{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	spans := make([]Span, len(r.spans))
	for i, span := range r.spans {
		spans[i] = span
		spans[i].Attributes = cloneStringMap(span.Attributes)
	}
	return TraceSnapshot{Spans: spans}
}

func sanitizeTraceAttributes(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		if isSensitiveTraceAttribute(key) {
			out[key] = redact(value)
			continue
		}
		out[key] = value
	}
	return out
}

func isSensitiveTraceAttribute(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range []string{"api_key", "apikey", "authorization", "credential", "input", "prompt", "secret", "token"} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
