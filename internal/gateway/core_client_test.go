package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPCoreClientEnsureSessionAndSubmitTask(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotSessionPath, gotTaskPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/sessions":
			gotSessionPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"session-99"}`))
		case "/v1/tasks":
			gotTaskPath = r.URL.Path
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"task-1"}`))
		default:
			http.Error(w, "no", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client := NewHTTPCoreClient(server.URL, "admintok")
	sid, err := client.EnsureSession(ctx, SessionReq{CompanyID: "c", AgentID: "a", Project: "telegram", Title: "telegram:hash"})
	if err != nil || sid != "session-99" {
		t.Fatalf("EnsureSession = %q, %v, want session-99", sid, err)
	}
	tid, err := client.SubmitTask(ctx, TaskReq{ID: "task-1", Input: "hi", SessionID: "session-99", CompanyID: "c", AgentID: "a"})
	if err != nil || tid != "task-1" {
		t.Fatalf("SubmitTask = %q, %v, want task-1", tid, err)
	}
	if gotAuth != "Bearer admintok" || gotSessionPath != "/v1/sessions" || gotTaskPath != "/v1/tasks" {
		t.Fatalf("auth/paths = %q %q %q", gotAuth, gotSessionPath, gotTaskPath)
	}
}

func TestHTTPCoreClientTaskResultDoneAndPending(t *testing.T) {
	ctx := context.Background()
	var status string // flip between calls
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tasks/t1/result" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"task_id":"t1","status":%q,"result":"answer text"}`, status)
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPCoreClient(server.URL, "tok")

	// Not terminal yet → done=false.
	status = "running"
	text, done, err := client.TaskResult(ctx, "t1")
	if err != nil || done {
		t.Fatalf("TaskResult(running) = %q,%v,%v, want done=false", text, done, err)
	}
	// Terminal → done=true with result text.
	status = "done"
	text, done, err = client.TaskResult(ctx, "t1")
	if err != nil || !done || text != "answer text" {
		t.Fatalf("TaskResult(done) = %q,%v,%v, want answer text/done", text, done, err)
	}
}
