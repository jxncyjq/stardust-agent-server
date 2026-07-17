package task

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLockStoreTryLockAllowsOnlyOneOwner(t *testing.T) {
	t.Parallel()

	store := NewLockStore()
	ctx := context.Background()
	const attempts = 16

	var wg sync.WaitGroup
	results := make(chan bool, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := store.TryLock(ctx, "task-1", "agent-"+string(rune('a'+i)), time.Minute)
			if err != nil {
				t.Errorf("TryLock(%q) error = %v, want nil", "task-1", err)
				return
			}
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)

	var locked int
	for ok := range results {
		if ok {
			locked++
		}
	}
	if locked != 1 {
		t.Errorf("successful TryLock calls = %d, want 1", locked)
	}
}
