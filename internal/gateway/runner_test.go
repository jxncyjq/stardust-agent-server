package gateway

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type fakeResult struct {
	text string
	done bool
}

type fakeCore struct {
	ensured   SessionReq
	submitted TaskReq
	sessionID string
	taskID    string
	results   map[string]fakeResult // taskID -> result for TaskResult
}

func (c *fakeCore) EnsureSession(_ context.Context, req SessionReq) (string, error) {
	c.ensured = req
	return c.sessionID, nil
}
func (c *fakeCore) SubmitTask(_ context.Context, req TaskReq) (string, error) {
	c.submitted = req
	return c.taskID, nil
}
func (c *fakeCore) TaskResult(_ context.Context, taskID string) (string, bool, error) {
	r, ok := c.results[taskID]
	if !ok {
		return "", false, nil
	}
	return r.text, r.done, nil
}

type memBinder struct{ m map[string][2]string }

func (b *memBinder) Resolve(_ context.Context, k string) (string, string, bool, error) {
	v, ok := b.m[k]
	return v[0], v[1], ok, nil
}
func (b *memBinder) Bind(_ context.Context, k, sid, raw string) error {
	b.m[k] = [2]string{sid, raw}
	return nil
}

func TestHandleInboundBindsHashedSessionAndTracks(t *testing.T) {
	core := &fakeCore{sessionID: "session-1", taskID: "task-1"}
	binder := &memBinder{m: map[string][2]string{}}
	tracker := NewDeliveryTracker()
	runner := NewGatewayRunner(
		GatewayConfig{Identity: IdentityConfig{AgentID: "im", CompanyID: "co"}},
		core, binder, NewDeliveryRouter(), tracker, nil, slog.Default(),
	)

	msg := InboundMessage{Platform: "telegram", ChatID: "42", UserID: "7", Text: "hi"}
	if err := runner.HandleInbound(context.Background(), msg); err != nil {
		t.Fatalf("HandleInbound() error = %v, want nil", err)
	}
	// Session created with hashed id in the title, never the raw chat id.
	if core.ensured.Title == "" || core.ensured.Title == "telegram:42" {
		t.Fatalf("session title = %q, want hashed (not raw 42)", core.ensured.Title)
	}
	// Task submitted against the session with the message text.
	if core.submitted.SessionID != "session-1" || core.submitted.Input != "hi" {
		t.Fatalf("submitted = %+v, want session-1/hi", core.submitted)
	}
	// Delivery target tracked with the RAW chat id (for outbound).
	target, ok := tracker.Take(core.submitted.ID)
	if !ok || target.ChatID != "42" {
		t.Fatalf("tracked target = %v, %v, want raw chat 42", target, ok)
	}
	// Binding stored under the platform key.
	if _, _, ok, _ := binder.Resolve(context.Background(), "telegram:42"); !ok {
		t.Fatalf("binding not stored for telegram:42")
	}
}

func TestHandleInboundReusesExistingSession(t *testing.T) {
	core := &fakeCore{sessionID: "SHOULD-NOT-BE-USED", taskID: "task-2"}
	binder := &memBinder{m: map[string][2]string{"telegram:42": {"session-existing", "42"}}}
	runner := NewGatewayRunner(
		GatewayConfig{Identity: IdentityConfig{AgentID: "im", CompanyID: "co"}},
		core, binder, NewDeliveryRouter(), NewDeliveryTracker(), nil, slog.Default(),
	)
	if err := runner.HandleInbound(context.Background(), InboundMessage{Platform: "telegram", ChatID: "42", Text: "again"}); err != nil {
		t.Fatalf("HandleInbound() error = %v", err)
	}
	if core.submitted.SessionID != "session-existing" {
		t.Fatalf("submitted session = %q, want reused session-existing", core.submitted.SessionID)
	}
	if core.ensured.AgentID != "" {
		t.Fatalf("EnsureSession called for an already-bound chat")
	}
}

func TestPollOnceDeliversTerminalTasks(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram"}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 4096)
	tracker := NewDeliveryTracker()
	tracker.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})
	tracker.Track("task-2", DeliveryTarget{Platform: "telegram", ChatID: "43"})
	core := &fakeCore{results: map[string]fakeResult{
		"task-1": {text: "answer one", done: true},
		"task-2": {done: false}, // still running
	}}
	runner := NewGatewayRunner(GatewayConfig{}, core, &memBinder{m: map[string][2]string{}}, router, tracker, nil, slog.Default())

	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error = %v, want nil", err)
	}
	// task-1 terminal → delivered and removed; task-2 still pending.
	if len(adapter.sent) != 1 || adapter.sent[0] != "answer one" {
		t.Fatalf("sent = %v, want [answer one]", adapter.sent)
	}
	if pending := tracker.Pending(); len(pending) != 1 || pending[0] != "task-2" {
		t.Fatalf("pending = %v, want [task-2]", pending)
	}
}

func TestPollOnceRetriesThenDropsOnPersistentFailure(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram", err: errors.New("send failed")}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 4096)
	tracker := NewDeliveryTracker()
	tracker.Track("task-1", DeliveryTarget{Platform: "telegram", ChatID: "42"})
	core := &fakeCore{results: map[string]fakeResult{
		"task-1": {text: "answer one", done: true},
	}}
	// BackoffMS: 0 means "not configured" and falls back to the default 500ms
	// spacing (per deliveryBackoff's documented default). The injected clock is
	// advanced past that window between passes so the second attempt isn't
	// held back by its own backoff — mirrors how the ticker-driven pollLoop
	// would reach it on a later tick.
	cfg := GatewayConfig{Delivery: DeliveryConfig{Retries: 2, BackoffMS: 0}}
	runner := NewGatewayRunner(cfg, core, &memBinder{m: map[string][2]string{}}, router, tracker, nil, slog.Default())
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return clock }

	// First pass: Route fails, attempt 1 of 2 — task stays tracked for retry.
	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() #1 error = %v, want nil", err)
	}
	if pending := tracker.Pending(); len(pending) != 1 || pending[0] != "task-1" {
		t.Fatalf("pending after #1 = %v, want [task-1] (retry pending)", pending)
	}
	if _, attempts, _, ok := tracker.Get("task-1"); !ok || attempts != 1 {
		t.Fatalf("Get(task-1) after #1 = attempts %d, ok %v, want 1, true", attempts, ok)
	}

	// Advance the clock past the default backoff window before the second pass.
	clock = clock.Add(time.Second)

	// Second pass: Route fails again, attempt 2 of 2 — retry budget exhausted, task dropped.
	if err := runner.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() #2 error = %v, want nil", err)
	}
	if pending := tracker.Pending(); len(pending) != 0 {
		t.Fatalf("pending after #2 = %v, want empty (task dropped)", pending)
	}
	if len(adapter.sent) != 0 {
		t.Fatalf("sent = %v, want none (Send always errors)", adapter.sent)
	}
}
