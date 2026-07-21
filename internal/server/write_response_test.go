package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit item V20: writeJSON and writePrometheus discarded their write errors.
//
// When encoding fails — a value carrying a chan/func, or a cycle — the status
// line and headers have already gone out, so the client receives 200 OK with an
// empty body and the server records nothing. In the GUI that reads as "the call
// succeeded but the data is empty", which sends the reader looking at the wrong
// layer entirely. The response cannot be repaired at that point, but it must not
// be silent.

type failingResponseWriter struct {
	header http.Header
	err    error
}

func (f *failingResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingResponseWriter) Write([]byte) (int, error) { return 0, f.err }
func (f *failingResponseWriter) WriteHeader(int)           {}

func TestWriteJSONReportsEncodeFailure(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	// A channel cannot be marshalled; the header is already on the wire by then.
	writeJSONLogging(slog.New(slog.NewTextHandler(&logs, nil)), httptest.NewRecorder(),
		http.StatusOK, map[string]any{"bad": make(chan int)})

	got := logs.String()
	for _, want := range []string{"WARN", "write json response"} {
		if !strings.Contains(got, want) {
			t.Fatalf("logger output = %q, want it to contain %q", got, want)
		}
	}
}

func TestWriteJSONReportsBrokenConnection(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	writeJSONLogging(slog.New(slog.NewTextHandler(&logs, nil)),
		&failingResponseWriter{err: errors.New("connection reset by peer")},
		http.StatusOK, map[string]string{"ok": "yes"})

	if got := logs.String(); !strings.Contains(got, "connection reset by peer") {
		t.Fatalf("logger output = %q, want it to carry the write error", got)
	}
}

func TestWriteJSONQuietOnSuccess(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	writeJSONLogging(slog.New(slog.NewTextHandler(&logs, nil)), rec,
		http.StatusOK, map[string]string{"ok": "yes"})

	if got := logs.String(); got != "" {
		t.Errorf("logger output = %q, want nothing logged on a successful write", got)
	}
	if !strings.Contains(rec.Body.String(), `"ok":"yes"`) {
		t.Errorf("body = %q, want the encoded value", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
