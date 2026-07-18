package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/task"
)

// TestSuspendRestartResumeEndToEnd proves the core M1b guarantee: a task that
// suspends (checkpoint on disk) can be resumed by a FRESH runtime built over the
// same store directory (simulating a process restart) and finishes with the
// correct result.
func TestSuspendRestartResumeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	agent := domain.Agent{ID: "agent-1"}
	task := domain.Task{ID: "task-1", SessionID: "sess-1", AgentID: "agent-1", Status: domain.TaskRunning, Input: "go"}
	ctx := context.Background()

	// --- process 1: run until it suspends ---
	store1 := sessionstate.NewStore(dir)
	runner1 := NewRuntime(Config{
		Maas:        &scriptedMaas{},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       echoRegistry(t),
		Checkpoints: store1,
		ToolGate:    &gateOnce{}, // suspends once
	})
	if _, err := runner1.RunTask(ctx, agent, task); !errors.Is(err, ErrSuspended) {
		t.Fatalf("process1 run err = %v, want ErrSuspended", err)
	}

	// --- simulate restart: brand-new store + runtime over the same dir ---
	store2 := sessionstate.NewStore(dir)
	suspended, err := store2.ListSuspended()
	if err != nil {
		t.Fatalf("ListSuspended: %v", err)
	}
	if len(suspended) != 1 || suspended[0].TaskID != "task-1" {
		t.Fatalf("recovered = %#v, want one checkpoint for task-1", suspended)
	}

	// Fresh runtime; its gate never suspends (decision has "arrived"), so the
	// resumed loop runs the pending echo call and completes.
	runner2 := NewRuntime(Config{
		Maas:        &scriptedMaas{},
		Audit:       adapter.NewMemoryAuditLog(),
		Events:      adapter.NewMemoryEventBus(),
		Tools:       echoRegistry(t),
		Checkpoints: store2,
		ToolGate:    nil,
	})
	run, err := runner2.RunTask(ctx, agent, task)
	if err != nil {
		t.Fatalf("process2 resume err = %v, want nil", err)
	}
	if run.Result != "final answer" {
		t.Errorf("resumed result = %q, want %q", run.Result, "final answer")
	}
	if _, ok, _ := store2.Load("sess-1"); ok {
		t.Error("checkpoint not cleaned after resume completion")
	}
}

func TestRecoverSuspendedReRegistersTasks(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        "task-9",
		AgentID:       "default-agent",
		SessionKey:    "sess-9",
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	sched := task.NewScheduler()
	coord := newTestCoordinator(t, sched, 4)
	ctx := context.Background()

	n, err := coord.RecoverSuspended(ctx, store)
	if err != nil {
		t.Fatalf("RecoverSuspended: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered count = %d, want 1", n)
	}
	got, ok, err := sched.Get(ctx, "task-9")
	if err != nil || !ok {
		t.Fatalf("get task-9: ok=%v err=%v", ok, err)
	}
	if got.Status != domain.TaskSuspended {
		t.Errorf("recovered task status = %s, want %s", got.Status, domain.TaskSuspended)
	}
}

func TestRecoverSuspendedFailsLoudOnCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	// Seed a checkpoint with an unsupported schema version so ListSuspended fails loud.
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion + 99,
		TaskID:        "task-bad",
		SessionKey:    "sess-bad",
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	sched := task.NewScheduler()
	coord := newTestCoordinator(t, sched, 4)
	n, err := coord.RecoverSuspended(context.Background(), store)
	if err == nil {
		t.Fatal("RecoverSuspended err = nil, want fail-loud error on unsupported checkpoint schema")
	}
	if n != 0 {
		t.Errorf("recovered = %d, want 0 on error", n)
	}
}

func TestRecoverSuspendedSkipsTaskAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	store := sessionstate.NewStore(dir)
	if err := store.Save(sessionstate.Checkpoint{
		SchemaVersion: sessionstate.CheckpointSchemaVersion,
		TaskID:        "task-dup",
		AgentID:       "default-agent",
		SessionKey:    "sess-dup",
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	sched := task.NewScheduler()
	ctx := context.Background()
	// Pre-register the same task id so RecoverSuspended must skip it (idempotent rescan).
	if err := sched.Add(ctx, domain.Task{ID: "task-dup", AgentID: "default-agent", Status: domain.TaskSuspended}); err != nil {
		t.Fatalf("pre-add: %v", err)
	}
	coord := newTestCoordinator(t, sched, 4)
	n, err := coord.RecoverSuspended(ctx, store)
	if err != nil {
		t.Fatalf("RecoverSuspended err = %v, want nil (skip, not error)", err)
	}
	if n != 0 {
		t.Errorf("recovered = %d, want 0 (task already present, skipped)", n)
	}
}
