package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrWorkingMemoryLimitExceeded = errors.New("working memory limit exceeded")

// MemoryOpKind is the kind of a batched working-memory mutation.
type MemoryOpKind string

const (
	// MemoryOpAdd appends Content as a new entry.
	MemoryOpAdd MemoryOpKind = "add"
	// MemoryOpReplace swaps the entry equal to Match for Content.
	MemoryOpReplace MemoryOpKind = "replace"
	// MemoryOpRemove deletes the entry equal to Match.
	MemoryOpRemove MemoryOpKind = "remove"
)

// MemoryOp is one mutation in an Apply batch. Match selects an existing entry by
// exact content for replace/remove; Content is the new text for add/replace.
type MemoryOp struct {
	Kind    MemoryOpKind
	Match   string
	Content string
}

type WorkingMemory struct {
	mu      sync.Mutex
	limit   int
	entries map[string][]string
}

func NewWorkingMemory(limit int) *WorkingMemory {
	if limit <= 0 {
		limit = 64 * 1024
	}
	return &WorkingMemory{
		limit:   limit,
		entries: make(map[string][]string),
	}
}

func (m *WorkingMemory) Append(ctx context.Context, taskID string, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := strings.Join(m.entries[taskID], "\n")
	size := len(current)
	if size > 0 {
		size++
	}
	size += len(content)
	if size > m.limit {
		return ErrWorkingMemoryLimitExceeded
	}
	m.entries[taskID] = append(m.entries[taskID], content)
	return nil
}

func (m *WorkingMemory) Read(ctx context.Context, taskID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.entries[taskID], "\n"), nil
}

func (m *WorkingMemory) Clear(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, taskID)
}

// Apply runs a batch of add/replace/remove ops against a task's working memory
// atomically: the ops are evaluated on a copy, the total size is checked against
// the limit only after the whole batch is applied, and the change is committed
// only if it fits. A failing op (missing target, empty content, unknown kind) or
// an over-budget final state leaves the stored memory untouched and returns a
// wrapped error — never a partial mutation. Because the budget is checked against
// the final state, a lone add that would overflow can still succeed when batched
// with a remove that frees enough room.
func (m *WorkingMemory) Apply(ctx context.Context, taskID string, ops []MemoryOp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	working := append([]string(nil), m.entries[taskID]...)
	for i, op := range ops {
		switch op.Kind {
		case MemoryOpAdd:
			if op.Content == "" {
				return fmt.Errorf("apply memory op %d (add) for %q: content is required", i, taskID)
			}
			working = append(working, op.Content)
		case MemoryOpReplace:
			if op.Content == "" {
				return fmt.Errorf("apply memory op %d (replace) for %q: content is required", i, taskID)
			}
			idx := indexOfEntry(working, op.Match)
			if idx < 0 {
				return fmt.Errorf("apply memory op %d (replace) for %q: entry %q not found", i, taskID, op.Match)
			}
			working[idx] = op.Content
		case MemoryOpRemove:
			idx := indexOfEntry(working, op.Match)
			if idx < 0 {
				return fmt.Errorf("apply memory op %d (remove) for %q: entry %q not found", i, taskID, op.Match)
			}
			working = append(working[:idx], working[idx+1:]...)
		default:
			return fmt.Errorf("apply memory op %d for %q: unknown kind %q", i, taskID, op.Kind)
		}
	}
	if len(strings.Join(working, "\n")) > m.limit {
		return fmt.Errorf("apply memory ops for %q: %w", taskID, ErrWorkingMemoryLimitExceeded)
	}
	if len(working) == 0 {
		delete(m.entries, taskID)
	} else {
		m.entries[taskID] = working
	}
	return nil
}

// indexOfEntry returns the index of the first entry equal to target, or -1.
func indexOfEntry(entries []string, target string) int {
	for i, entry := range entries {
		if entry == target {
			return i
		}
	}
	return -1
}
