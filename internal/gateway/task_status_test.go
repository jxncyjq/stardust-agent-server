package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit item V22: TaskResult's switch sent every unrecognised status to
// `default: return "", false, nil` — the same answer as "not finished yet".
//
// The doc contract covers pending/running: those legitimately mean "retry". A
// status the client has never heard of (version skew, a renamed field) is not
// that, and treating it as such makes PollOnce retry forever. The user waiting in
// Telegram never gets a reply, and the gateway logs nothing, because runner.go
// only warns when err is non-nil.

func newTestCoreClient(t *testing.T, status string) *HTTPCoreClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"` + status + `","result":"answer"}`))
	}))
	t.Cleanup(server.Close)
	return NewHTTPCoreClient(server.URL, "")
}

func TestTaskResultFailsLoudOnUnknownStatus(t *testing.T) {
	t.Parallel()

	_, done, err := newTestCoreClient(t, "reticulating").TaskResult(context.Background(), "task-1")
	if err == nil {
		t.Fatal("TaskResult(unknown status) error = nil, want an error")
	}
	if done {
		t.Error("done = true, want false")
	}
	if !strings.Contains(err.Error(), "reticulating") {
		t.Errorf("error = %q, want it to name the unknown status", err.Error())
	}
}

func TestTaskResultTreatsInFlightStatusesAsRetry(t *testing.T) {
	t.Parallel()

	// The contract these statuses have: not terminal, no error, poller retries.
	// Turning them into errors would break polling entirely — worse than the bug.
	for _, status := range []string{"pending", "running", "assigned"} {
		text, done, err := newTestCoreClient(t, status).TaskResult(context.Background(), "task-1")
		if err != nil {
			t.Errorf("TaskResult(%q) error = %v, want nil", status, err)
		}
		if done {
			t.Errorf("TaskResult(%q) done = true, want false", status)
		}
		if text != "" {
			t.Errorf("TaskResult(%q) text = %q, want empty", status, text)
		}
	}
}

func TestTaskResultReturnsTerminalStatuses(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"done", "failed", "suspended"} {
		text, done, err := newTestCoreClient(t, status).TaskResult(context.Background(), "task-1")
		if err != nil {
			t.Errorf("TaskResult(%q) error = %v, want nil", status, err)
		}
		if !done {
			t.Errorf("TaskResult(%q) done = false, want true", status)
		}
		if text != "answer" {
			t.Errorf("TaskResult(%q) text = %q, want %q", status, text, "answer")
		}
	}
}

// Audit item V17: postJSON and TaskResult read the response body with
// `data, _ := io.ReadAll(...)` and returned data on the success path regardless.
//
// A connection cut mid-body yields a truncated JSON document that is then handed
// to the caller as if it were complete. EnsureSession/SubmitTask report "decode
// session response: unexpected end of JSON input" — a network failure wearing a
// protocol failure's clothes, which sends the reader looking in the wrong place.
//
// (The fourth ReadAll, in telegram's sendMessage, only builds an error message
// from an already-failed response and is deliberately left alone — same
// reasoning the audit applies to http_maas.go.)

// truncatingServer promises more bytes than it delivers, then hangs up: the
// client's ReadAll fails with unexpected EOF.
func truncatingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "512")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"do`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTaskResultFailsLoudOnTruncatedBody(t *testing.T) {
	t.Parallel()

	_, _, err := NewHTTPCoreClient(truncatingServer(t).URL, "").TaskResult(context.Background(), "task-1")
	if err == nil {
		t.Fatal("TaskResult(truncated body) error = nil, want an error")
	}
	// It must read as a read failure, not as malformed JSON.
	if strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want it to report the read failure rather than a decode failure", err.Error())
	}
}
