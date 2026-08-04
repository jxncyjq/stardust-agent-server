package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeInterrupter is a minimal TaskInterrupter test double: nilErr controls
// whether Interrupt reports the task as running (nil) or not running (error).
type fakeInterrupter struct {
	err     error
	calledW string
}

func (f *fakeInterrupter) Interrupt(taskID string) error {
	f.calledW = taskID
	return f.err
}

func TestHTTPServerInterruptTaskRunningReturns2xx(t *testing.T) {
	t.Parallel()
	fake := &fakeInterrupter{err: nil}
	srv := NewHTTPServer(Config{TaskInterrupter: fake})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/abc/interrupt", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code/100 != 2 {
		t.Fatalf("POST /v1/tasks/abc/interrupt status = %d, want 2xx body=%s", rec.Code, rec.Body.String())
	}
	if fake.calledW != "abc" {
		t.Fatalf("Interrupt called with %q, want %q", fake.calledW, "abc")
	}
}

func TestHTTPServerInterruptTaskNotRunningReturns404(t *testing.T) {
	t.Parallel()
	fake := &fakeInterrupter{err: fmt.Errorf("task %q is not running; cannot interrupt", "abc")}
	srv := NewHTTPServer(Config{TaskInterrupter: fake})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/abc/interrupt", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /v1/tasks/abc/interrupt status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}
