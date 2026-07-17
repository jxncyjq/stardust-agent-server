package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/server"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/task"
	"github.com/stardust/legion-agent/internal/workflow"
)

func TestMinimalConfigGoldenLoads(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(context.Background(), config.Options{Path: "testdata/config-minimal.json"})
	if err != nil {
		t.Fatalf("Load(config-minimal.json) error = %v, want nil", err)
	}
	if cfg.Storage.Driver != "memory" {
		t.Fatalf("Load(config-minimal.json).Storage.Driver = %q, want memory", cfg.Storage.Driver)
	}
	if cfg.Server.RequestIDHeader != "X-Request-ID" || !cfg.Server.PublicHealthEnabled {
		t.Fatalf("Load(config-minimal.json).Server = %#v, want public X-Request-ID health", cfg.Server)
	}
	if cfg.Service.BackgroundInterval != "1s" {
		t.Fatalf("Load(config-minimal.json).Service.BackgroundInterval = %q, want 1s", cfg.Service.BackgroundInterval)
	}
}

func TestWorkflowMinimalGoldenRunsToWaitingEvent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/workflow-minimal.json")
	if err != nil {
		t.Fatalf("ReadFile(workflow-minimal.json) error = %v, want nil", err)
	}
	var def workflow.Definition
	if err := json.Unmarshal(data, &def); err != nil {
		t.Fatalf("Unmarshal(workflow-minimal.json) error = %v, want nil", err)
	}

	result, err := workflow.NewEngine(workflow.Config{
		Scheduler: task.NewScheduler(),
		Events:    adapter.NewMemoryEventBus(),
	}).Execute(context.Background(), def)
	if err != nil {
		t.Fatalf("Execute(compat workflow) error = %v, want nil", err)
	}
	if result.WorkflowID != "compat-workflow" || result.Status != workflow.StatusWaitingEvent {
		t.Fatalf("Execute(compat workflow) = %#v, want waiting event", result)
	}
}

func TestHTTPAPILiteGoldenFields(t *testing.T) {
	t.Parallel()
	golden := loadHTTPGolden(t)
	scheduler := task.NewScheduler()
	repo, err := storage.OpenSQLite(context.Background(), filepathSafeTempDB(t))
	if err != nil {
		t.Fatalf("OpenSQLite(temp) error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	srv := server.NewHTTPServer(server.Config{
		Tasks:               scheduler,
		Workflows:           repo,
		PublicHealthEnabled: true,
		RequestIDHeader:     "X-Request-ID",
		Metrics:             observability.NewMetricsRecorder(nil),
	})

	create := httptest.NewRecorder()
	srv.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewBufferString(`{
		"id": "compat-task",
		"company_id": "company-1",
		"agent_id": "agent-1",
		"input": "compat input"
	}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d body=%s", create.Code, http.StatusCreated, create.Body.String())
	}
	var taskResp map[string]any
	if err := json.NewDecoder(create.Body).Decode(&taskResp); err != nil {
		t.Fatalf("Decode(task response) error = %v, want nil", err)
	}
	requireFields(t, "task response", taskResp, golden.TaskResponseRequiredFields)

	if err := repo.SaveWorkflowState(context.Background(), workflow.Definition{
		ID: "compat-waiting",
		Root: workflow.Node{
			ID:        "wait-node",
			Kind:      workflow.NodeWaitEvent,
			EventType: "compat.ready",
		},
	}, workflow.Result{
		WorkflowID: "compat-waiting",
		Status:     workflow.StatusWaitingEvent,
		Nodes:      []workflow.NodeResult{{NodeID: "wait-node", Status: workflow.StatusWaitingEvent}},
	}); err != nil {
		t.Fatalf("SaveWorkflowState(compat-waiting) error = %v, want nil", err)
	}
	waiting := httptest.NewRecorder()
	srv.ServeHTTP(waiting, httptest.NewRequest(http.MethodGet, "/v1/workflows/waiting", nil))
	if waiting.Code != http.StatusOK {
		t.Fatalf("GET /v1/workflows/waiting status = %d, want %d body=%s", waiting.Code, http.StatusOK, waiting.Body.String())
	}
	var workflowResp []map[string]any
	if err := json.NewDecoder(waiting.Body).Decode(&workflowResp); err != nil {
		t.Fatalf("Decode(waiting workflows) error = %v, want nil", err)
	}
	if len(workflowResp) == 0 {
		t.Fatalf("GET /v1/workflows/waiting returned no workflows, want at least one")
	}
	requireFields(t, "waiting workflow response", workflowResp[0], golden.WaitingWorkflowRequiredFields)

	metrics := httptest.NewRecorder()
	srv.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d body=%s", metrics.Code, http.StatusOK, metrics.Body.String())
	}
	var metricsResp map[string]any
	if err := json.NewDecoder(metrics.Body).Decode(&metricsResp); err != nil {
		t.Fatalf("Decode(metrics) error = %v, want nil", err)
	}
	requireFields(t, "metrics response", metricsResp, golden.MetricsRequiredFields)
}

type httpGolden struct {
	TaskResponseRequiredFields    []string `json:"task_response_required_fields"`
	WaitingWorkflowRequiredFields []string `json:"waiting_workflow_required_fields"`
	MetricsRequiredFields         []string `json:"metrics_required_fields"`
}

func loadHTTPGolden(t *testing.T) httpGolden {
	t.Helper()
	data, err := os.ReadFile("testdata/http-openapi-lite.json")
	if err != nil {
		t.Fatalf("ReadFile(http-openapi-lite.json) error = %v, want nil", err)
	}
	var golden httpGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("Unmarshal(http-openapi-lite.json) error = %v, want nil", err)
	}
	return golden
}

func requireFields(t *testing.T, name string, got map[string]any, fields []string) {
	t.Helper()
	for _, field := range fields {
		if _, ok := got[field]; !ok {
			t.Fatalf("%s missing field %q: %#v", name, field, got)
		}
	}
}

func filepathSafeTempDB(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/compat.db"
}
