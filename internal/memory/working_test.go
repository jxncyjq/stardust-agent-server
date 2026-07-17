package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWorkingMemoryApplyAtomicBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewWorkingMemory(20)
	big := "0123456789012345" // 16 chars
	if err := m.Append(ctx, "task", big); err != nil {
		t.Fatalf("Append(big) error = %v, want nil", err)
	}

	// A lone add would overflow: 16 + 1 (join) + 10 = 27 > 20.
	err := m.Apply(ctx, "task", []MemoryOp{{Kind: MemoryOpAdd, Content: "0123456789"}})
	if !errors.Is(err, ErrWorkingMemoryLimitExceeded) {
		t.Fatalf("Apply(lone add) error = %v, want ErrWorkingMemoryLimitExceeded", err)
	}
	// Atomic: the failed batch left memory untouched.
	if got, _ := m.Read(ctx, "task"); got != big {
		t.Fatalf("Read after failed Apply = %q, want unchanged %q", got, big)
	}

	// Same add succeeds when batched with a remove that frees room.
	if err := m.Apply(ctx, "task", []MemoryOp{
		{Kind: MemoryOpRemove, Match: big},
		{Kind: MemoryOpAdd, Content: "0123456789"},
	}); err != nil {
		t.Fatalf("Apply(remove+add) error = %v, want nil", err)
	}
	if got, _ := m.Read(ctx, "task"); got != "0123456789" {
		t.Fatalf("Read after remove+add = %q, want \"0123456789\"", got)
	}
}

func TestWorkingMemoryApplyFailsLoudOnBadOps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewWorkingMemory(64)
	if err := m.Apply(ctx, "task", []MemoryOp{{Kind: MemoryOpRemove, Match: "ghost"}}); err == nil {
		t.Fatalf("Apply(remove missing) error = nil, want non-nil")
	}
	if err := m.Apply(ctx, "task", []MemoryOp{{Kind: "explode", Content: "x"}}); err == nil {
		t.Fatalf("Apply(unknown kind) error = nil, want non-nil")
	}
	if got, _ := m.Read(ctx, "task"); got != "" {
		t.Fatalf("Read after failed ops = %q, want empty", got)
	}
}

func TestWorkingMemoryAppendsAndReadsPerTask(t *testing.T) {
	t.Parallel()

	memory := NewWorkingMemory(64)
	if err := memory.Append(context.Background(), "task-1", "first"); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", "first", err)
	}
	if err := memory.Append(context.Background(), "task-1", "second"); err != nil {
		t.Fatalf("Append(%q) error = %v, want nil", "second", err)
	}

	got, err := memory.Read(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Read(%q) error = %v, want nil", "task-1", err)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("Read(%q) = %q, want both entries", "task-1", got)
	}
}

func TestWorkingMemoryRejectsOverLimitAppend(t *testing.T) {
	t.Parallel()

	memory := NewWorkingMemory(4)
	err := memory.Append(context.Background(), "task-1", "12345")
	if !errors.Is(err, ErrWorkingMemoryLimitExceeded) {
		t.Fatalf("Append() error = %v, want ErrWorkingMemoryLimitExceeded", err)
	}
}

func TestWorkingMemoryClearTask(t *testing.T) {
	t.Parallel()

	memory := NewWorkingMemory(64)
	if err := memory.Append(context.Background(), "task-1", "scratch"); err != nil {
		t.Fatalf("Append() error = %v, want nil", err)
	}
	memory.Clear("task-1")

	got, err := memory.Read(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Read(%q) error = %v, want nil", "task-1", err)
	}
	if got != "" {
		t.Errorf("Read(%q) = %q, want empty", "task-1", got)
	}
}
