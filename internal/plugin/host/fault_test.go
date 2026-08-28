package host

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassifyCallFaultNilErrorIsNotAFault(t *testing.T) {
	category, isFault := ClassifyCallFault(context.Background(), nil)
	if category != "" || isFault {
		t.Errorf("ClassifyCallFault(ctx, nil) = (%q, %t), want (\"\", false)", category, isFault)
	}
}

func TestClassifyCallFaultDeadlineIsTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	category, isFault := ClassifyCallFault(ctx, fmt.Errorf("invoke op=1: %w", context.DeadlineExceeded))
	if category != CategoryTimeout || !isFault {
		t.Errorf("ClassifyCallFault on a deadline = (%q, %t), want (%q, true)", category, isFault, CategoryTimeout)
	}
}

func TestClassifyCallFaultCancellationIsNotThePluginsFault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	category, isFault := ClassifyCallFault(ctx, fmt.Errorf("invoke op=1: %w", context.Canceled))
	if isFault {
		t.Errorf("ClassifyCallFault on a caller cancellation = (%q, true), want isFault=false: "+
			"a caller who walked away has not broken the plugin", category)
	}
}

func TestClassifyCallFaultABISentinel(t *testing.T) {
	err := fmt.Errorf("alloc 8 bytes: %w", ErrGuestABI)
	category, isFault := ClassifyCallFault(context.Background(), err)
	if category != CategoryABI || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryABI)
	}
}

func TestClassifyCallFaultTrapSentinel(t *testing.T) {
	err := fmt.Errorf("invoke op=1: %w", ErrGuestTrap)
	category, isFault := ClassifyCallFault(context.Background(), err)
	if category != CategoryTrap || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryTrap)
	}
}

func TestClassifyCallFaultUnrecognizedCountsAsTrap(t *testing.T) {
	category, isFault := ClassifyCallFault(context.Background(), errors.New("something nobody classified"))
	if category != CategoryTrap || !isFault {
		t.Errorf("ClassifyCallFault on an unclassified error = (%q, %t), want (%q, true): "+
			"an unclassifiable failure is still a failure", category, isFault, CategoryTrap)
	}
}

// TestInvokeBogusResultIsAnABIFault runs the classification against a REAL
// guest failure rather than a hand-built error: the fixture's op 93 returns a
// result pointer far outside linear memory, which is exactly the shape of ABI
// violation the health counter must recognise.
func TestInvokeBogusResultIsAnABIFault(t *testing.T) {
	ctx := context.Background()
	inst := newTestInstance(t, testMemoryPages)

	_, err := inst.Invoke(ctx, opBogusResult, nil)
	if err == nil {
		t.Fatal("Invoke(op=93) = nil error, want an ABI failure: the fixture returns a pointer outside memory")
	}
	category, isFault := ClassifyCallFault(ctx, err)
	if category != CategoryABI || !isFault {
		t.Errorf("ClassifyCallFault(%v) = (%q, %t), want (%q, true)", err, category, isFault, CategoryABI)
	}
}
