package task

import (
	"context"
	"sync"
	"time"
)

type LockStore struct {
	mu    sync.Mutex
	locks map[string]Lock
}

type Lock struct {
	TaskID    string
	OwnerID   string
	ExpiresAt time.Time
}

func NewLockStore() *LockStore {
	return &LockStore{locks: make(map[string]Lock)}
}

func (s *LockStore) TryLock(ctx context.Context, taskID string, ownerID string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, ok := s.locks[taskID]; ok && lock.ExpiresAt.After(now) {
		return false, nil
	}
	s.locks[taskID] = Lock{
		TaskID:    taskID,
		OwnerID:   ownerID,
		ExpiresAt: now.Add(ttl),
	}
	return true, nil
}

func (s *LockStore) ReapExpired(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var reaped int
	for taskID, lock := range s.locks {
		if lock.ExpiresAt.After(now) {
			continue
		}
		delete(s.locks, taskID)
		reaped++
	}
	return reaped, nil
}
