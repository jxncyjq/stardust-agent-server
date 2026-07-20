package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// recordingSubMaas returns a fixed summary and records every prompt it sees, so
// tests can assert what context a delegated child was actually given.
type recordingSubMaas struct {
	mu      sync.Mutex
	summary string
	prompts []string
}

func (m *recordingSubMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompts = append(m.prompts, req.Prompt)
	return port.InferenceResponse{Text: m.summary}, nil
}

func (m *recordingSubMaas) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.prompts...)
}

func TestRunSubTaskReturnsSummaryOnly(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Maas: maas})

	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "parent-1",
		Goal:         "研究 token 计数",
		Context:      "参考 hermes",
	})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}
	// (b) parent gets only the child's final summary.
	if res.Summary != "子任务摘要：完成" {
		t.Fatalf("RunSubTask().Summary = %q, want child final result", res.Summary)
	}
	if res.TaskID != "parent-1:sub-1" {
		t.Fatalf("RunSubTask().TaskID = %q, want parent-1:sub-1", res.TaskID)
	}
	// (a) child ran on its own goal/context, not any parent tool history.
	prompts := maas.recorded()
	if len(prompts) != 1 {
		t.Fatalf("child prompts = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "研究 token 计数") || !strings.Contains(prompts[0], "参考 hermes") {
		t.Fatalf("child prompt = %q, want goal and context", prompts[0])
	}
}

func TestRunSubTaskLeafCannotDelegate(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "ok"}
	// A leaf runtime at depth 1 must refuse to spawn.
	leaf := NewRuntime(Config{Maas: maas, Role: roleLeaf, Depth: 1})
	if _, err := leaf.RunSubTask(context.Background(), SubTaskSpec{Goal: "nope"}); err == nil {
		t.Fatalf("RunSubTask(leaf) error = nil, want delegation-not-permitted")
	}
}

func TestRunSubTaskDepthLimitFailsLoud(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "ok"}
	// Orchestrator already at the spawn depth limit cannot go deeper.
	deep := NewRuntime(Config{Maas: maas, Role: roleOrchestrator, Depth: 2, MaxSpawnDepth: 2})
	if _, err := deep.RunSubTask(context.Background(), SubTaskSpec{Goal: "too deep"}); err == nil {
		t.Fatalf("RunSubTask(at depth limit) error = nil, want depth-exceeded")
	}
}

func TestRunSubTasksBatchStableOrderAndReportsFailure(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "batch summary"}
	parent := NewRuntime(Config{Maas: maas})

	specs := []SubTaskSpec{
		{ParentTaskID: "p", Goal: "a"},
		{ParentTaskID: "p", Goal: ""}, // invalid goal → reported per-entry, not fatal
		{ParentTaskID: "p", Goal: "c"},
	}
	results, err := parent.RunSubTasks(context.Background(), specs)
	if err != nil {
		t.Fatalf("RunSubTasks() error = %v, want nil (per-task errors reported inline)", err)
	}
	if len(results) != 3 {
		t.Fatalf("RunSubTasks() len = %d, want 3", len(results))
	}
	if results[0].Summary != "batch summary" || results[2].Summary != "batch summary" {
		t.Fatalf("RunSubTasks() results[0,2] = %q,%q, want batch summary", results[0].Summary, results[2].Summary)
	}
	if results[1].Err == "" {
		t.Fatalf("RunSubTasks() results[1].Err empty, want reported goal error")
	}
}

func TestRunSubTaskAsyncPublishesCompletion(t *testing.T) {
	t.Parallel()

	maas := &recordingSubMaas{summary: "async done"}
	events := newCollectingEventBus()
	parent := NewRuntime(Config{Maas: maas, Events: events})

	handle, err := parent.RunSubTaskAsync(context.Background(), SubTaskSpec{ParentTaskID: "p", Goal: "bg work"})
	if err != nil {
		t.Fatalf("RunSubTaskAsync() error = %v, want nil", err)
	}
	if handle.TaskID == "" {
		t.Fatalf("RunSubTaskAsync() handle has empty TaskID")
	}
	got := events.waitFor(t, "subtask_completed")
	if got.TaskID != handle.TaskID {
		t.Fatalf("completion event TaskID = %q, want %q", got.TaskID, handle.TaskID)
	}
	if !strings.Contains(got.Message, "async done") {
		t.Fatalf("completion event Message = %q, want child summary", got.Message)
	}
}

func TestParentTaskIDForSubTask(t *testing.T) {
	t.Parallel()
	if parent, ok := ParentTaskIDForSubTask("task-1:run:sub-3"); !ok || parent != "task-1:run" {
		t.Fatalf("ParentTaskIDForSubTask(sub) = %q,%v, want task-1:run,true", parent, ok)
	}
	if parent, ok := ParentTaskIDForSubTask("no-suffix"); ok || parent != "no-suffix" {
		t.Fatalf("ParentTaskIDForSubTask(no suffix) = %q,%v, want no-suffix,false", parent, ok)
	}
}

// collectingEventBus is a thread-safe event sink that lets a test block until an
// event of a given type is published (for the background delegation path).
type collectingEventBus struct {
	mu     sync.Mutex
	cond   *sync.Cond
	events []domain.RuntimeEvent
}

func newCollectingEventBus() *collectingEventBus {
	b := &collectingEventBus{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *collectingEventBus) Publish(ctx context.Context, event domain.RuntimeEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	b.cond.Broadcast()
	return nil
}

func (b *collectingEventBus) Events() ([]domain.RuntimeEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RuntimeEvent(nil), b.events...), nil
}

func (b *collectingEventBus) waitFor(t *testing.T, eventType string) domain.RuntimeEvent {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		for _, event := range b.events {
			if event.Type == eventType {
				return event
			}
		}
		b.cond.Wait()
	}
}
