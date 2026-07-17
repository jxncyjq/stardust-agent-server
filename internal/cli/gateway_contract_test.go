package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
)

// TestTaskSubmitEmitsCompletedEvent verifies the core contract the IM gateway
// depends on: a task submitted through the serve stack (POST /v1/tasks)
// executes via the live scheduler + coordinator heartbeat and surfaces a
// task_completed runtime event carrying the model result text, keyed by the
// task id.
//
// It mirrors the existing serve-boot pattern in
// TestServeCommandUsesSQLiteForHTTPTaskState (command_test.go): boot "serve"
// through the real cobra command with a short background_interval so the
// coordinator heartbeat actually runs, POST a task, then poll the
// /v1/runtime-events endpoint (which exposes the shared workflowEvents bus,
// see internal/server/http.go handleRuntimeEvents) until the task_completed
// event for the submitted task id appears.
func TestTaskSubmitEmitsCompletedEvent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const wantResult = "hello from model"
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"service": {"background_interval": "20ms"},
		"runtime": {"demo_response": "`+wantResult+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v, want nil", err)
	}

	var out bytes.Buffer
	root := NewRoot(app.New(), &out)
	root.SetContext(ctx)
	root.SetArgs([]string{"serve", "--config", configPath, "--addr", addr})
	done := make(chan error, 1)
	go func() {
		done <- root.Execute()
	}()

	const taskID = "task-contract-1"
	postURL := "http://" + addr + "/v1/tasks"
	resp, err := waitForPostTask(t, postURL, `{"id":"`+taskID+`","company_id":"company-1","input":"say hi"}`)
	if err != nil {
		cancel()
		t.Fatalf("POST /v1/tasks error = %v, want nil", err)
	}
	if err := resp.Body.Close(); err != nil {
		cancel()
		t.Fatalf("Body.Close() error = %v, want nil", err)
	}
	if resp.StatusCode != http.StatusCreated {
		cancel()
		t.Fatalf("POST /v1/tasks status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	eventsURL := "http://" + addr + "/v1/runtime-events"
	found := false
	var message string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, fetchErr := fetchRuntimeEvents(eventsURL)
		if fetchErr != nil {
			cancel()
			t.Fatalf("GET /v1/runtime-events error = %v, want nil", fetchErr)
		}
		for _, e := range events {
			if e.Type == "task_completed" && e.TaskID == taskID {
				found = true
				message = e.Message
				break
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case execErr := <-done:
		if execErr != nil {
			t.Fatalf("Execute(serve) error = %v, want nil", execErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Execute(serve) did not stop")
	}

	if !found {
		t.Fatalf("no task_completed event for %q", taskID)
	}
	if message != wantResult {
		t.Fatalf("task_completed message = %q, want %q", message, wantResult)
	}
}

// fetchRuntimeEvents GETs the given /v1/runtime-events URL and decodes the
// JSON body into the runtime event list. It wraps both the request and the
// decode failure with %w so a caller polling in a loop reports the exact
// underlying cause rather than a bare "error".
func fetchRuntimeEvents(url string) ([]domain.RuntimeEvent, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	var events []domain.RuntimeEvent
	decodeErr := json.NewDecoder(resp.Body).Decode(&events)
	closeErr := resp.Body.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("decode runtime events from %s: %w", url, decodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close runtime events body from %s: %w", url, closeErr)
	}
	return events, nil
}
