package adapter

import (
	"context"
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
