package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// streamingMaas implements both Generate and GenerateStream so the runtime's
// type assertion picks the streaming path.
type streamingMaas struct {
	deltas []string
	resp   port.InferenceResponse
}

func (m *streamingMaas) Generate(context.Context, port.InferenceRequest) (port.InferenceResponse, error) {
	return m.resp, nil
}
func (m *streamingMaas) GenerateStream(_ context.Context, _ port.InferenceRequest, onDelta func(string)) (port.InferenceResponse, error) {
	for _, d := range m.deltas {
		if onDelta != nil {
			onDelta(d)
		}
	}
	return m.resp, nil
}

// recordingBus captures published events.
type recordingBus struct {
	mu     sync.Mutex
	events []domain.RuntimeEvent
}

func (b *recordingBus) Publish(_ context.Context, e domain.RuntimeEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return nil
}
func (b *recordingBus) Events() ([]domain.RuntimeEvent, error) { return nil, nil }

func TestRuntimeStreamsTokenEventsWhenClientSupportsStreaming(t *testing.T) {
	bus := &recordingBus{}
	maas := &streamingMaas{deltas: []string{"He", "llo"}, resp: port.InferenceResponse{Text: "Hello"}}
	rt := NewRuntime(Config{Gate: NewTaskGate(), Maas: maas, Events: bus, Tools: unchangingReadRegistry(t)})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}

	var tokens []string
	for _, e := range bus.events {
		if e.Type == "token" {
			if e.TaskID != "t1" {
				t.Errorf("token event TaskID = %q, want t1", e.TaskID)
			}
			tokens = append(tokens, e.Message)
		}
	}
	if len(tokens) != 2 || tokens[0] != "He" || tokens[1] != "llo" {
		t.Fatalf("token events = %v, want [He llo]", tokens)
	}
}

// A client that does NOT implement MaasStreamingClient (e.g. MOA/summary clients,
// test stubs) must go through the synchronous path and emit no token events.
func TestRuntimeEmitsNoTokenEventsForNonStreamingClient(t *testing.T) {
	bus := &recordingBus{}
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{Gate: NewTaskGate(), Maas: maas, Events: bus, Tools: unchangingReadRegistry(t)})
	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}
	for _, e := range bus.events {
		if e.Type == "token" {
			t.Fatalf("non-streaming client emitted a token event: %+v", e)
		}
	}
}
