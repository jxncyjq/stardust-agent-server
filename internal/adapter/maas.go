package adapter

import (
	"context"
	"fmt"
	"sync"

	"github.com/stardust/legion-agent/internal/port"
)

type RecordingMaas struct {
	mu       sync.Mutex
	response string
	calls    []port.InferenceRequest
}

func NewRecordingMaas(response string) *RecordingMaas {
	return &RecordingMaas{response: response}
}

func (m *RecordingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	// The stand-in client validates too: a caller that builds an ambiguous
	// request must fail the same way here as against a real provider, instead of
	// passing in tests and breaking in production.
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("validate inference request: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	return port.InferenceResponse{Text: m.response}, nil
}

func (m *RecordingMaas) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}
