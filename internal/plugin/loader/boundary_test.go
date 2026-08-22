package loader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/host"
	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/runtime"
)

// boundaryDeps is a Deps func for the constructor tests, which only need one
// that is not nil: New rejects its Config before any of it is used.
func boundaryDeps(name string, cfg json.RawMessage) host.Deps {
	return host.Deps{PluginName: name, Config: cfg}
}

// The bounds every wait in this file is held to. They are generous enough that
// a slow machine does not trip them and short enough that a wedged gate fails
// the test instead of hanging it: nothing here waits on an unbounded channel.
const (
	// boundaryPendingBound is how long a test waits for a backgrounded Apply to
	// become pending. Reaching it FAILS the test.
	boundaryPendingBound = 5 * time.Second
	// boundaryApplyBound is how long a test waits for that Apply to return once
	// the task in flight has ended, and the ApplyWait those tests give the
	// Loader. Reaching it FAILS the test — it is deliberately well under the
	// -timeout the suite runs with, so a wedged gate is reported as a failed
	// assertion here rather than as a killed test binary.
	boundaryApplyBound = 30 * time.Second
	// boundaryPollInterval is the gap between two probes of the gate.
	boundaryPollInterval = 5 * time.Millisecond
)

// TestNewRejectsNilGate and TestNewRejectsNonPositiveApplyWait lock decision 2
// and decision 4 on the loader side: without a gate a convergence would land in
// the middle of a running task, and without a positive wait "how long may an
// apply wait for a boundary" would be answered by accident.
func TestNewRejectsNilGate(t *testing.T) {
	t.Parallel()

	_, err := New(Config{
		Ledger:    lifecycle.NewLedger(),
		Deps:      boundaryDeps,
		Events:    adapter.NewMemoryEventBus(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ApplyWait: time.Second,
	})
	if err == nil {
		t.Fatalf("New() with a nil Gate error = nil, want an error naming the field")
	}
	if !strings.Contains(err.Error(), "Gate") {
		t.Fatalf("New() error = %q, want it to name Config.Gate", err)
	}
}

func TestNewRejectsNonPositiveApplyWait(t *testing.T) {
	t.Parallel()

	for _, wait := range []time.Duration{0, -time.Second} {
		_, err := New(Config{
			Ledger:    lifecycle.NewLedger(),
			Deps:      boundaryDeps,
			Events:    adapter.NewMemoryEventBus(),
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Gate:      runtime.NewTaskGate(),
			ApplyWait: wait,
		})
		if err == nil {
			t.Fatalf("New() with ApplyWait=%s error = nil, want an error naming the field", wait)
		}
		if !strings.Contains(err.Error(), "ApplyWait") {
			t.Fatalf("New() with ApplyWait=%s error = %q, want it to name Config.ApplyWait", wait, err)
		}
	}
}

// TestApplyLandsOnlyAtTaskBoundary is the gate-keeping test of contract 4: a
// task that is already running keeps the capability catalog it started with,
// and the new target state lands only once that task is over.
//
// Every wait is bounded and every bound fails the test. The two ways the
// convergence could escape the boundary — the plugin's tool appearing in the
// registry, or Apply returning at all — are both checked while the task is in
// flight, so a Loader that converged directly fails here rather than passing on
// a timing accident.
func TestApplyLandsOnlyAtTaskBoundary(t *testing.T) {
	h := newHarnessWithApplyWait(t, boundaryApplyBound)
	entry := h.writeEcho("1.0.0")
	dep := manifest.Deployment{Plugins: []manifest.Entry{entry}}

	// A task is in flight, exactly as RunTask holds it.
	end, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	ended := false
	defer func() {
		if !ended {
			end()
		}
	}()

	done := make(chan error, 1)
	go func() { done <- h.loader.Apply(context.Background(), dep, h.root) }()

	h.awaitApplyPending(t, done)

	// The in-flight task's world is unchanged: no tool, no ledger owner, and
	// nothing the Loader reports as loaded.
	if names := h.toolNames(); len(names) != 0 {
		t.Fatalf("registry tools while a task was in flight = %v, want none", names)
	}
	if owners := h.owners(); len(owners) != 0 {
		t.Fatalf("ledger owners while a task was in flight = %v, want none", owners)
	}
	if status := h.loader.Status(); len(status) != 0 {
		t.Fatalf("Status() while a task was in flight = %#v, want empty", status)
	}

	// The boundary: the task ends and the apply may land.
	ended = true
	end()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Apply() error = %v, want nil", err)
		}
	case <-time.After(boundaryApplyBound):
		t.Fatalf("Apply() did not return within %s of the task ending", boundaryApplyBound)
	}

	if names := h.toolNames(); !slices.Contains(names, echoToolName) {
		t.Fatalf("registry tools after the boundary = %v, want %q among them", names, echoToolName)
	}
	if owners := h.owners(); len(owners) != 1 {
		t.Fatalf("ledger owners after the boundary = %v, want exactly one", owners)
	}
	status := h.loader.Status()
	if len(status) != 1 || status[0].State != StateLoaded {
		t.Fatalf("Status() after the boundary = %#v, want one loaded entry", status)
	}
}

// TestApplyTimesOutAndChangesNothing covers the loud-failure path: a task that
// outlasts ApplyWait does not get its plugins swapped out from under it, and
// the caller is told nothing landed. Both halves of "nothing landed" are
// asserted — the ledger and the registry — because a convergence that filed
// nothing but registered a tool would still have broken the running task.
//
// It is bounded by ApplyWait itself (50ms): Apply cannot outlast it.
func TestApplyTimesOutAndChangesNothing(t *testing.T) {
	h := newHarnessWithApplyWait(t, 50*time.Millisecond)
	entry := h.writeEcho("1.0.0")
	dep := manifest.Deployment{Plugins: []manifest.Entry{entry}}

	end, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	defer end()

	err = h.loader.Apply(context.Background(), dep, h.root)
	if err == nil {
		t.Fatalf("Apply() error = nil, want a timeout; a task was in flight for the whole wait")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() error = %v, want one matching context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("Apply() error = %q, want it to report the tasks that were in the way", err)
	}

	if owners := h.owners(); len(owners) != 0 {
		t.Fatalf("ledger owners after a timed-out Apply = %v, want none", owners)
	}
	if names := h.toolNames(); len(names) != 0 {
		t.Fatalf("registry tools after a timed-out Apply = %v, want none", names)
	}
	if status := h.loader.Status(); len(status) != 0 {
		t.Fatalf("Status() after a timed-out Apply = %#v, want empty; nothing was attempted", status)
	}
}

// TestApplyCancelledContextChangesNothing is the same loud failure reached the
// other way: the caller gave up. It also pins that a cancelled apply leaves the
// gate open, so the next task can still start.
func TestApplyCancelledContextChangesNothing(t *testing.T) {
	h := newHarnessWithApplyWait(t, boundaryApplyBound)
	entry := h.writeEcho("1.0.0")
	dep := manifest.Deployment{Plugins: []manifest.Entry{entry}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.loader.Apply(ctx, dep, h.root)
	if err == nil {
		t.Fatalf("Apply() with a cancelled ctx error = nil, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() error = %v, want one matching context.Canceled", err)
	}
	if owners := h.owners(); len(owners) != 0 {
		t.Fatalf("ledger owners after a cancelled Apply = %v, want none", owners)
	}
	if names := h.toolNames(); len(names) != 0 {
		t.Fatalf("registry tools after a cancelled Apply = %v, want none", names)
	}
	// Nothing was even ATTEMPTED: a convergence that ran and merely failed on
	// the cancelled ctx would leave the entry recorded as failed here.
	if status := h.loader.Status(); len(status) != 0 {
		t.Fatalf("Status() after a cancelled Apply = %#v, want empty; fn must not have run", status)
	}

	probe, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() after a cancelled Apply error = %v, want nil; the gate stayed shut", err)
	}
	probe()
}

// TestApplyRejectsEmptyRootBeforeReachingTheGate keeps the cheap rejections
// where they belong: a target state that is not a target state is refused
// outright, without pausing anybody's tasks for it.
func TestApplyRejectsEmptyRootBeforeReachingTheGate(t *testing.T) {
	h := newHarnessWithApplyWait(t, 50*time.Millisecond)
	entry := h.writeEcho("1.0.0")
	dep := manifest.Deployment{Plugins: []manifest.Entry{entry}}

	// A task in flight would make any gated path wait out the 50ms and fail with
	// a timeout instead; the error below is what proves the check ran first.
	end, err := h.gate.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v, want nil", err)
	}
	defer end()

	err = h.loader.Apply(context.Background(), dep, "")
	if err == nil {
		t.Fatalf("Apply() with an empty root error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "root is empty") {
		t.Fatalf("Apply() error = %q, want the empty-root rejection", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Apply() error = %q, want the empty-root rejection before any wait for a boundary", err)
	}
}

// awaitApplyPending waits until the backgrounded Apply has claimed the gate and
// is waiting for the boundary, and fails the test the moment the convergence
// escapes that discipline instead.
//
// The poll is what makes the in-flight assertions meaningful: once Begin
// reports ErrApplyPending, the Apply is demonstrably underway AND demonstrably
// has not converged, so "nothing changed" is a statement about a real window
// rather than about a race the test happened to win.
func (h *harness) awaitApplyPending(t *testing.T, done <-chan error) {
	t.Helper()

	deadline := time.Now().Add(boundaryPendingBound)
	for {
		if names := h.toolNames(); slices.Contains(names, echoToolName) {
			t.Fatalf("plugin tool %q was registered while a task was in flight; "+
				"the convergence did not wait for a task boundary", echoToolName)
		}
		select {
		case err := <-done:
			t.Fatalf("Apply() returned (%v) while a task was still in flight; "+
				"the convergence did not wait for a task boundary", err)
		default:
		}

		probe, err := h.gate.Begin()
		if err != nil {
			if !errors.Is(err, runtime.ErrApplyPending) {
				t.Fatalf("Begin() error = %v, want one matching ErrApplyPending", err)
			}
			return
		}
		probe()

		if time.Now().After(deadline) {
			t.Fatalf("Apply() never claimed the gate within %s; it is not applying at a task boundary",
				boundaryPendingBound)
		}
		time.Sleep(boundaryPollInterval)
	}
}
