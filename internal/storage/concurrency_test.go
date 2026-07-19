package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// TestSQLiteRepositoryConcurrentWritersNoBusy drives many goroutines writing
// sessions and tasks into one repository at once. Before OpenSQLite set a
// busy_timeout and capped the pool at a single connection, this pattern
// intermittently surfaced SQLITE_BUSY ("database is locked") because
// database/sql's default busy timeout is zero — a locked write errors instead of
// waiting. The test asserts every concurrent write succeeds, that no error is a
// lock-contention error, and that every row lands, confirming contention is
// resolved by block-and-retry rather than swallowed.
func TestSQLiteRepositoryConcurrentWritersNoBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestSQLiteRepository(t)

	const writers = 16
	const perWriter = 25
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*2)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				sessionID := fmt.Sprintf("sess-%d-%d", w, i)
				session := domain.AgentSession{
					ID:        sessionID,
					CompanyID: "company-1",
					AgentID:   fmt.Sprintf("agent-%d", w),
					Title:     "concurrent",
					CreatedAt: base,
					UpdatedAt: base,
				}
				if err := repo.SaveAgentSession(ctx, session); err != nil {
					errs <- fmt.Errorf("save session %q: %w", sessionID, err)
					continue
				}
				task := domain.Task{
					ID:            fmt.Sprintf("task-%d-%d", w, i),
					CompanyID:     "company-1",
					AgentID:       fmt.Sprintf("agent-%d", w),
					SessionID:     sessionID,
					Status:        domain.TaskPending,
					Input:         "concurrent write",
					MaxIterations: 1,
					CreatedAt:     base,
				}
				if err := repo.SaveTask(ctx, task); err != nil {
					errs <- fmt.Errorf("save task for %q: %w", sessionID, err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if isSQLiteBusy(err) {
			t.Errorf("concurrent write hit lock contention: %v", err)
			continue
		}
		t.Errorf("concurrent write failed: %v", err)
	}

	sessions, err := repo.ListAgentSessions(ctx, "", "")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	if len(sessions) != writers*perWriter {
		t.Errorf("persisted %d sessions, want %d", len(sessions), writers*perWriter)
	}
	tasks, err := repo.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks() error = %v, want nil", err)
	}
	if len(tasks) != writers*perWriter {
		t.Errorf("persisted %d tasks, want %d", len(tasks), writers*perWriter)
	}
}

// isSQLiteBusy reports whether err is a SQLite lock-contention error, the exact
// failure the busy_timeout + single-connection pool is meant to eliminate.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}
