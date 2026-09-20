package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/storage"
)

// TestServeTaskResultReadsThePersistedRun 钉住装配那条线：serve 建出来的 HTTP
// 服务必须拿到 task_runs 的 store。
//
// 没有这条测试，把 server.Config 里的 TaskRuns 删掉照样编得过、别的测试照样全绿
// ——端点会静静地退回事件日志，而事件日志里永远没有 interrupted，于是一次被中断
// 的运行在接口上读起来与「跑完了但什么都没产出」一模一样。
//
// 这里让上一次进程留下的 running 记录走完真实的启动扫描：库里预置一条 running，
// serve 起来把它摆成 interrupted，再从端点读回来。事件日志给不出这个答案，所以
// 读到 interrupted 只可能是因为端点真的查了表。
func TestServeTaskResultReadsThePersistedRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "agent.db")
	seed, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	if err := seed.SaveTask(context.Background(), domain.Task{
		ID:        "task-wire-1",
		CompanyID: "company-1",
		Status:    domain.TaskRunning,
		Input:     "上一次进程跑到一半就没了",
		CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SaveTask error = %v, want nil", err)
	}
	if err := seed.StartTaskRun(context.Background(), domain.TaskRun{
		ID:        "run-wire-1",
		TaskID:    "task-wire-1",
		AgentID:   "default-agent",
		Status:    domain.RunStatusRunning,
		StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("StartTaskRun error = %v, want nil", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close(seed) error = %v, want nil", err)
	}

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"},
		"service": {"background_interval": "1h"}
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
	if err := waitForServeListening(addr, done, serveReadyTimeout); err != nil {
		cancel()
		t.Fatalf("waitForServeListening(%q) error = %v, want nil", addr, err)
	}

	resp, err := http.Get("http://" + addr + "/v1/tasks/task-wire-1/result")
	if err != nil {
		cancel()
		t.Fatalf("GET result error = %v, want nil", err)
	}
	var got struct {
		Status    string `json:"status"`
		RunStatus string `json:"run_status"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&got)
	closeErr := resp.Body.Close()
	code := resp.StatusCode
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute(serve) error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute(serve) did not stop")
	}
	if decodeErr != nil {
		t.Fatalf("Decode(result response) error = %v, want nil", decodeErr)
	}
	if closeErr != nil {
		t.Fatalf("Body.Close() error = %v, want nil", closeErr)
	}
	if code != http.StatusOK {
		t.Fatalf("GET result status = %d, want %d", code, http.StatusOK)
	}
	if got.RunStatus != string(domain.RunStatusInterrupted) {
		t.Fatalf("run_status = %q, want %q（端点没查表，或者装配没把 TaskRuns 接进去）",
			got.RunStatus, domain.RunStatusInterrupted)
	}
}
