package lifecycle

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestLedgerDisposesInReverseOrder(t *testing.T) {
	l := NewLedger()
	var order []string
	l.Add("plugin:a", "first", func() error { order = append(order, "first"); return nil })
	l.Add("plugin:a", "second", func() error { order = append(order, "second"); return nil })

	if err := l.DisposeOwner("plugin:a"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("want [second first], got %v", order)
	}
}

func TestLedgerRunsEveryDisposerDespiteFailure(t *testing.T) {
	l := NewLedger()
	ran := 0
	l.Add("plugin:a", "ok-1", func() error { ran++; return nil })
	l.Add("plugin:a", "boom", func() error { ran++; return errors.New("close failed") })
	l.Add("plugin:a", "ok-2", func() error { ran++; return nil })

	err := l.DisposeOwner("plugin:a")
	if err == nil {
		t.Fatal("want joined error, got nil")
	}
	if ran != 3 {
		t.Fatalf("want all 3 disposers to run, ran %d", ran)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("error must name the failing entry and cause, got %q", err)
	}
}

func TestLedgerHandleIsIdempotentAndRemovesEntry(t *testing.T) {
	l := NewLedger()
	calls := 0
	revoke := l.Add("plugin:a", "one", func() error { calls++; return nil })

	if err := revoke(); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := revoke(); err != nil {
		t.Fatalf("second revoke must be a no-op, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want dispose called once, got %d", calls)
	}
	if got := l.Snapshot(); len(got) != 0 {
		t.Fatalf("want empty ledger after revoke, got %v", got)
	}
}

func TestLedgerDisposeOwnerSkipsAlreadyRevoked(t *testing.T) {
	l := NewLedger()
	calls := 0
	revoke := l.Add("plugin:a", "one", func() error { calls++; return nil })
	l.Add("plugin:a", "two", func() error { calls++; return nil })

	if err := revoke(); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := l.DisposeOwner("plugin:a"); err != nil {
		t.Fatalf("DisposeOwner: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 total dispose calls, got %d", calls)
	}
}

func TestLedgerDisposeUnknownOwnerIsNoOp(t *testing.T) {
	l := NewLedger()
	if err := l.DisposeOwner("plugin:missing"); err != nil {
		t.Fatalf("want nil for unknown owner, got %v", err)
	}
}

func TestLedgerSnapshotReportsLabelsPerOwner(t *testing.T) {
	l := NewLedger()
	l.Add("plugin:a", "tool:foo", func() error { return nil })
	l.Add("plugin:a", "tool:bar", func() error { return nil })
	l.Add("plugin:b", "section:x", func() error { return nil })

	snap := l.Snapshot()
	if len(snap["plugin:a"]) != 2 || len(snap["plugin:b"]) != 1 {
		t.Fatalf("unexpected snapshot: %v", snap)
	}
	snap["plugin:a"][0] = "mutated"
	if l.Snapshot()["plugin:a"][0] == "mutated" {
		t.Fatal("Snapshot must return a copy, not internal state")
	}
}

func TestLedgerAddNilDisposePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on nil dispose")
		}
	}()
	NewLedger().Add("plugin:a", "bad", nil)
}

func TestLedgerConcurrentAddAndDispose(t *testing.T) {
	l := NewLedger()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			revoke := l.Add("plugin:a", "n", func() error { return nil })
			_ = revoke()
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = l.DisposeOwner("plugin:a") }()
	wg.Wait()
}
