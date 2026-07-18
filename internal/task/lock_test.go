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

func TestUnlockReleasesOnlyForOwner(t *testing.T) {
	s := NewLockStore()
	ctx := context.Background()
	if ok, _ := s.TryLock(ctx, "t1", "owner-a", time.Minute); !ok {
		t.Fatal("initial lock failed")
	}
	// wrong owner cannot release
	if ok, err := s.Unlock(ctx, "t1", "owner-b"); err != nil || ok {
		t.Fatalf("Unlock wrong owner: ok=%v err=%v, want false,nil", ok, err)
	}
	// right owner releases
	if ok, err := s.Unlock(ctx, "t1", "owner-a"); err != nil || !ok {
		t.Fatalf("Unlock owner: ok=%v err=%v, want true,nil", ok, err)
	}
	// now re-lockable by anyone
	if ok, _ := s.TryLock(ctx, "t1", "owner-b", time.Minute); !ok {
		t.Fatal("re-lock after unlock failed")
	}
}
