package observability

import (
	"maps"
	"strconv"
	"sync"
	"time"
)

type MetricsSnapshot struct {
	StartedAt    time.Time        `json:"started_at"`
	Tasks        map[string]int64 `json:"tasks"`
	HTTPStatus   map[string]int64 `json:"http_status"`
	ModelCalls   map[string]int64 `json:"model_calls"`
	Approvals    map[string]int64 `json:"approvals"`
	WorkflowRuns map[string]int64 `json:"workflow_runs"`
}

type MetricsRecorder struct {
	mu           sync.Mutex
	startedAt    time.Time
	tasks        map[string]int64
	httpStatus   map[string]int64
	modelCalls   map[string]int64
	approvals    map[string]int64
	workflowRuns map[string]int64
}

func NewMetricsRecorder(now func() time.Time) *MetricsRecorder {
	if now == nil {
		now = time.Now
	}
	return &MetricsRecorder{
		startedAt:    now(),
		tasks:        make(map[string]int64),
		httpStatus:   make(map[string]int64),
		modelCalls:   make(map[string]int64),
		approvals:    make(map[string]int64),
		workflowRuns: make(map[string]int64),
	}
}

func (m *MetricsRecorder) IncTaskStatus(status string) {
	m.inc(m.tasks, status)
}

func (m *MetricsRecorder) IncHTTPStatus(status int) {
	m.inc(m.httpStatus, strconv.Itoa(status))
}

func (m *MetricsRecorder) IncModelCall(status string) {
	m.inc(m.modelCalls, status)
}

func (m *MetricsRecorder) IncApproval(status string) {
	m.inc(m.approvals, status)
}

func (m *MetricsRecorder) IncWorkflowRun(status string) {
	m.inc(m.workflowRuns, status)
}

func (m *MetricsRecorder) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MetricsSnapshot{
		StartedAt:    m.startedAt,
		Tasks:        cloneCounterMap(m.tasks),
		HTTPStatus:   cloneCounterMap(m.httpStatus),
		ModelCalls:   cloneCounterMap(m.modelCalls),
		Approvals:    cloneCounterMap(m.approvals),
		WorkflowRuns: cloneCounterMap(m.workflowRuns),
	}
}

func (m *MetricsRecorder) inc(counters map[string]int64, key string) {
	if key == "" {
		key = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	counters[key]++
}

func cloneCounterMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	maps.Copy(out, in)
	return out
}
