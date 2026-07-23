package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/server"
	"github.com/stardust/legion-agent/internal/sessioncache"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/taskledger"
	"github.com/stardust/legion-agent/internal/testsupport"
	"github.com/stardust/legion-agent/internal/tool"
)

func TestRootAcceptsGoRunSeparator(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := Execute(app.New(), &out, []string{"--", "run", "--demo", "--plain"}); err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if got := out.String(); got == "" {
		t.Errorf("Execute() output = empty, want demo result")
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{"version", "--plain"})
	if err != nil {
		t.Fatalf("Execute(version --plain) error = %v, want nil", err)
	}
	got := out.String()
	for _, want := range []string{"version=dev", "commit=unknown", "build_time=unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Execute(version --plain) output = %q, want containing %q", got, want)
		}
	}
}

func TestRootIncludesTUICommand(t *testing.T) {
	t.Parallel()

	root := NewRoot(app.New(), &bytes.Buffer{})
	cmd, _, err := root.Find([]string{"tui"})
	if err != nil {
		t.Fatalf("Root.Find(tui) error = %v, want nil", err)
	}
	if cmd == nil || cmd.Use != "tui" {
		t.Fatalf("Root.Find(tui) = %#v, want tui command", cmd)
	}
}

func TestTUIDisplayConfigUsesSelectedProfileModel(t *testing.T) {
	t.Parallel()

	display := tuiDisplayConfig(config.MaasConfig{
		DefaultProfile: "planner",
		Profiles: map[string]config.MaasProfile{
			"planner": {Model: "planner-model"},
			"review":  {Model: "review-model"},
		},
	}, "", "")
	if display.AgentName != "planner" {
		t.Fatalf("tuiDisplayConfig().AgentName = %q, want planner", display.AgentName)
	}
	if display.ModelName != "planner-model" {
		t.Fatalf("tuiDisplayConfig().ModelName = %q, want planner-model", display.ModelName)
	}

	display = tuiDisplayConfig(config.MaasConfig{
		DefaultProfile: "planner",
		Profiles: map[string]config.MaasProfile{
			"planner": {Model: "planner-model"},
			"review":  {Model: "review-model"},
		},
	}, "review", "")
	if display.AgentName != "review" || display.ModelName != "review-model" {
		t.Fatalf("tuiDisplayConfig(review) = %#v, want review/review-model", display)
	}
}

func TestDefaultLoggerWritesToLogFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "logs", "agent.log")

	logger, err := newFileLogger(logPath)
	if err != nil {
		t.Fatalf("newFileLogger() error = %v, want nil", err)
	}
	logger.Info("file logger smoke", "task_id", "task-log-file")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(logs/agent.log) error = %v, want nil", err)
	}
	if !bytes.Contains(data, []byte("file logger smoke")) || !bytes.Contains(data, []byte("task-log-file")) {
		t.Fatalf("logs/agent.log = %s, want structured log entry", data)
	}
}

func TestNewCommandTaskIDUsesPrefixAndUniqueSuffix(t *testing.T) {
	t.Parallel()

	first := newCommandTaskID("tui-task")
	second := newCommandTaskID("tui-task")
	if !strings.HasPrefix(first, "tui-task-") {
		t.Fatalf("newCommandTaskID() = %q, want tui-task prefix", first)
	}
	if !strings.HasPrefix(second, "tui-task-") {
		t.Fatalf("newCommandTaskID() = %q, want tui-task prefix", second)
	}
	if first == second {
		t.Fatalf("newCommandTaskID() returned duplicate ID %q", first)
	}
}

func TestBuildRunContextPrefixLoadsAllConfiguredContextFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCLIFile(t, dir, "AGENTS.md", "agents rules")
	writeCLIFile(t, dir, "configs/persona/SOUL.md", "soul identity")
	writeCLIFile(t, dir, "configs/persona/TOOLS.md", "tool policy")
	writeCLIFile(t, dir, "configs/persona/USER.md", "user preference")
	writeCLIFile(t, dir, "configs/persona/MEMORY.md", "agent memory")
	cfg := config.Config{
		ContextFiles: config.ContextFilesConfig{
			Enabled:      true,
			Root:         dir,
			AgentsPath:   "AGENTS.md",
			SoulPath:     "configs/persona/SOUL.md",
			ToolsPath:    "configs/persona/TOOLS.md",
			UserPath:     "configs/persona/USER.md",
			MemoryPath:   "configs/persona/MEMORY.md",
			MaxFileChars: 20000,
		},
	}

	prefix, err := buildRunContextPrefix(context.Background(), cfg, false, "")
	if err != nil {
		t.Fatalf("buildRunContextPrefix() error = %v, want nil", err)
	}
	for _, want := range []string{"agents rules", "soul identity", "tool policy", "user preference", "agent memory"} {
		if !strings.Contains(prefix, want) {
			t.Fatalf("buildRunContextPrefix() missing %q:\n%s", want, prefix)
		}
	}
}

// waitForPostTask waits for the in-process serve to start listening, then posts
// the task once.
//
// It used to retry the POST 100 times at 10ms — a 1s ceiling that a slow CI
// startup crossed, surfacing as "connection refused" from the request itself
// (runs 29938476692, 29986168680). Readiness is now a dial probe with a real
// budget, and serveDone (the channel `go root.Execute()` reports on; nil is
// allowed) turns a serve that died during startup into that error rather than a
// timeout. Waiting on the port instead of retrying the POST also means the task
// is submitted exactly once, so a retry can never create a second task.
func waitForPostTask(t *testing.T, url string, body string, serveDone <-chan error) (*http.Response, error) {
	t.Helper()
	parsed, err := neturl.Parse(url)
	if err != nil {
		return nil, fmt.Errorf("parse task URL %q: %w", url, err)
	}
	if err := waitForServeListening(parsed.Host, serveDone, serveReadyTimeout); err != nil {
		return nil, err
	}
	return http.Post(url, "application/json", strings.NewReader(body))
}

func TestRunCommandUsesHTTPMaasForPrompt(t *testing.T) {
	t.Parallel()
	var gotPrompt string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		gotPrompt = testsupport.RequestText(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "real result"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--prompt", "Summarize Legion",
		"--maas-url", server.URL,
		"--maas-api-key", "secret-token",
		"--no-context-files",
	})
	if err != nil {
		t.Fatalf("Execute(run --prompt --maas-url) error = %v, want nil", err)
	}
	if gotPrompt != "Summarize Legion" {
		t.Fatalf("HTTP MaaS prompt = %q, want %q", gotPrompt, "Summarize Legion")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("HTTP MaaS authorization = %q, want bearer token", gotAuth)
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte(`result="real result"`)) {
		t.Fatalf("Execute(run --prompt --maas-url) output = %q, want real result", got)
	}
}

func TestRunCommandLoadsHTTPMaasFromConfigFile(t *testing.T) {
	t.Parallel()
	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		gotPrompt = testsupport.RequestText(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "configured result"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"maas":{"base_url":"`+server.URL+`"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--prompt", "Use config",
		"--no-context-files",
	})
	if err != nil {
		t.Fatalf("Execute(run --config --prompt) error = %v, want nil", err)
	}
	if gotPrompt != "Use config" {
		t.Fatalf("HTTP MaaS prompt = %q, want %q", gotPrompt, "Use config")
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte(`result="configured result"`)) {
		t.Fatalf("Execute(run --config --prompt) output = %q, want configured result", got)
	}
}

func TestRunCommandLoadsContextFilesFromConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeCLIFile(t, dir, "AGENTS.md", "project rule from agents")
	writeCLIFile(t, dir, "configs/persona/SOUL.md", "soul identity from file")
	writeCLIFile(t, dir, "configs/persona/TOOLS.md", "tool policy from file")
	writeCLIFile(t, dir, "configs/persona/USER.md", "user preference from file")
	writeCLIFile(t, dir, "configs/persona/MEMORY.md", "agent memory from file")

	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		gotPrompt = testsupport.RequestText(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "context ok"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(dir, "agent.json")
	body := `{
		"maas": {"base_url": "` + server.URL + `"},
		"context_files": {
			"enabled": true,
			"root": "` + filepath.ToSlash(dir) + `",
			"agents_path": "AGENTS.md",
			"soul_path": "configs/persona/SOUL.md",
			"tools_path": "configs/persona/TOOLS.md",
			"user_path": "configs/persona/USER.md",
			"memory_path": "configs/persona/MEMORY.md",
			"max_file_chars": 20000
		}
	}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--prompt", "ship context",
	})
	if err != nil {
		t.Fatalf("Execute(run context files) error = %v, want nil", err)
	}
	for _, want := range []string{
		"project rule from agents",
		"soul identity from file",
		"tool policy from file",
		"user preference from file",
		"agent memory from file",
		"ship context",
	} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("MaaS prompt missing %q:\n%q", want, gotPrompt)
		}
	}
}

func TestRunCommandUsesMaasProfile(t *testing.T) {
	t.Parallel()
	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		gotPrompt = testsupport.RequestText(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "profile result"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"maas": {
			"default_profile": "fast",
			"profiles": {
				"review": {"base_url": "`+server.URL+`", "api_key": "profile-key"}
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--maas-profile", "review",
		"--prompt", "Use profile",
		"--no-context-files",
	})
	if err != nil {
		t.Fatalf("Execute(run --maas-profile) error = %v, want nil", err)
	}
	if gotPrompt != "Use profile" {
		t.Fatalf("HTTP MaaS profile prompt = %q, want %q", gotPrompt, "Use profile")
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte(`result="profile result"`)) {
		t.Fatalf("Execute(run --maas-profile) output = %q, want profile result", got)
	}
}

func TestRunCommandUsesDefaultMaasProfileBeforeTopLevelBaseURL(t *testing.T) {
	t.Parallel()
	var gotPrompt string
	topLevelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("top-level MaaS server called, want default profile server")
	}))
	t.Cleanup(topLevelServer.Close)
	profileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		gotPrompt = testsupport.RequestText(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "profile default result"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(profileServer.Close)
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"maas": {
			"base_url": "`+topLevelServer.URL+`",
			"default_profile": "dev",
			"profiles": {
				"dev": {"base_url": "`+profileServer.URL+`", "api_key": "profile-key"}
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--prompt", "Use default profile",
		"--no-context-files",
	})
	if err != nil {
		t.Fatalf("Execute(run default profile) error = %v, want nil", err)
	}
	if gotPrompt != "Use default profile" {
		t.Fatalf("default profile prompt = %q, want Use default profile", gotPrompt)
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte(`result="profile default result"`)) {
		t.Fatalf("Execute(run default profile) output = %q, want profile result", got)
	}
}

func TestParseTUIAgentPrompt(t *testing.T) {
	t.Parallel()

	parsed := parseTUIAgentPrompt("@researcher 调研一下当前实现")
	if !parsed.Mentioned || parsed.AgentID != "researcher" || parsed.Prompt != "调研一下当前实现" {
		t.Fatalf("parseTUIAgentPrompt(@researcher) = %#v, want researcher mention", parsed)
	}
	bound := parseTUIAgentPrompt("@writer --task TASK-20260523-001 整理成说明")
	if !bound.Mentioned || bound.AgentID != "writer" || bound.TaskID != "TASK-20260523-001" || bound.Prompt != "整理成说明" {
		t.Fatalf("parseTUIAgentPrompt(@writer --task) = %#v, want task-bound writer mention", bound)
	}
	inbox := parseTUIAgentPrompt("@writer --inbox 根据最新消息整理")
	if !inbox.Mentioned || inbox.AgentID != "writer" || !inbox.IncludeInbox || inbox.Prompt != "根据最新消息整理" {
		t.Fatalf("parseTUIAgentPrompt(@writer --inbox) = %#v, want inbox-bound writer mention", inbox)
	}
	plain := parseTUIAgentPrompt("普通问题")
	if plain.Mentioned || plain.AgentID != "" || plain.Prompt != "普通问题" {
		t.Fatalf("parseTUIAgentPrompt(plain) = %#v, want plain prompt", plain)
	}
}

func TestRunTUITaskRoutesMentionToConfiguredAgent(t *testing.T) {
	t.Parallel()

	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		if len(req.Messages) > 0 {
			gotPrompt = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "research result"}},
			},
		}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	writeCLIFile(t, root, "AGENTS.md", "project rules")
	writeCLIFile(t, root, "researcher/SOUL.md", "researcher soul")
	cfg := config.Config{
		Maas: config.MaasConfig{
			Profiles: map[string]config.MaasProfile{
				"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
			},
		},
		Runtime: config.RuntimeConfig{MaxToolRounds: 1},
	}
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"researcher": {
			ID:          "researcher",
			Role:        "researcher",
			MaasProfile: "review",
			ContextFiles: config.ContextFilesConfig{
				Enabled:      true,
				Root:         root,
				AgentsPath:   "AGENTS.md",
				SoulPath:     "researcher/SOUL.md",
				MaxFileChars: 20000,
			},
		},
	})

	result, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:   cfg,
		Registry: registry,
		Prompt:   "@researcher 调研一下当前实现",
	})
	if err != nil {
		t.Fatalf("runTUITask(@researcher) error = %v, want nil", err)
	}
	if result.Result != "research result" {
		t.Fatalf("runTUITask(@researcher).Result = %q, want research result", result.Result)
	}
	for _, want := range []string{"researcher soul", "调研一下当前实现"} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("MaaS prompt missing %q:\n%q", want, gotPrompt)
		}
	}
}

func TestRunTUITaskBindsMentionedAgentToTaskLedgerTask(t *testing.T) {
	t.Parallel()

	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		if len(req.Messages) > 0 {
			gotPrompt = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "整理后的任务说明"}},
			},
		}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	ledger, err := taskledger.New(taskledger.Config{
		WorkspaceRoot:   root,
		AllowedAgentIDs: []string{"cli-agent", "researcher", "writer"},
	})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	if _, err := ledger.Append(context.Background(), taskledger.Event{
		TaskID:       "TASK-20260523-001",
		Type:         taskledger.EventTaskCreated,
		ActorAgentID: "cli-agent",
		Title:        "调研缓存实现",
		Status:       "planned",
		Summary:      "确认 cache 包的数据结构与淘汰策略",
	}); err != nil {
		t.Fatalf("Ledger.Append(task.created) error = %v, want nil", err)
	}
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"researcher": {
			ID:           "researcher",
			Role:         "researcher",
			MaasProfile:  "review",
			ContextFiles: config.ContextFilesConfig{Root: root},
		},
	})
	cfg := config.Config{
		Maas: config.MaasConfig{
			Profiles: map[string]config.MaasProfile{
				"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
			},
		},
		Runtime: config.RuntimeConfig{MaxToolRounds: 1},
		ContextFiles: config.ContextFilesConfig{
			Root: root,
		},
	}

	result, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:     cfg,
		Registry:   registry,
		Prompt:     "@researcher --task TASK-20260523-001 请继续调研",
		TaskLedger: ledger,
	})
	if err != nil {
		t.Fatalf("runTUITask(@researcher --task) error = %v, want nil", err)
	}
	if result.Result != "整理后的任务说明" {
		t.Fatalf("runTUITask(@researcher --task).Result = %q, want model response", result.Result)
	}
	for _, want := range []string{"TaskLedger task context:", "TASK-20260523-001", "调研缓存实现", "确认 cache 包的数据结构与淘汰策略", "请继续调研"} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("MaaS prompt missing %q:\n%s", want, gotPrompt)
		}
	}
	projection, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Ledger.Snapshot() error = %v, want nil", err)
	}
	task := projection.Tasks["TASK-20260523-001"]
	if len(task.Messages) != 1 || task.Messages[0].Type != taskledger.EventResultAppended {
		t.Fatalf("TaskLedger messages = %#v, want one result.appended", task.Messages)
	}
	if task.Messages[0].From != "researcher" || task.Messages[0].Summary != "整理后的任务说明" {
		t.Fatalf("TaskLedger result message = %#v, want researcher result", task.Messages[0])
	}
}

func TestRunTUITaskInjectsMentionedAgentInboxAndMarksRead(t *testing.T) {
	t.Parallel()

	var gotPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		if len(req.Messages) > 0 {
			gotPrompt = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "writer summary"}},
			},
		}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)

	repo := openCLITestSQLiteRepository(t)
	if err := repo.SaveAgentMessage(context.Background(), domain.AgentMessage{
		ID:          "msg-research-1",
		FromAgentID: "researcher",
		ToAgentID:   "writer",
		Type:        domain.AgentMessageTypeResult,
		Status:      domain.AgentMessageUnread,
		Summary:     "缓存实现位于 internal/cache，结论已整理",
		CreatedAt:   time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveAgentMessage() error = %v, want nil", err)
	}
	root := t.TempDir()
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"writer": {
			ID:           "writer",
			Role:         "writer",
			MaasProfile:  "review",
			ContextFiles: config.ContextFilesConfig{Root: root},
		},
	})
	cfg := config.Config{
		Maas: config.MaasConfig{
			Profiles: map[string]config.MaasProfile{
				"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
			},
		},
		Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		ContextFiles: config.ContextFilesConfig{Root: root},
	}

	result, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:       cfg,
		Registry:     registry,
		Prompt:       "@writer --inbox 根据最新消息整理成说明",
		MessageStore: repo,
	})
	if err != nil {
		t.Fatalf("runTUITask(@writer --inbox) error = %v, want nil", err)
	}
	if result.Result != "writer summary" {
		t.Fatalf("runTUITask(@writer --inbox).Result = %q, want writer summary", result.Result)
	}
	for _, want := range []string{"AgentMessage inbox context:", "msg-research-1", "researcher -> writer", "缓存实现位于 internal/cache", "根据最新消息整理成说明"} {
		if !strings.Contains(gotPrompt, want) {
			t.Fatalf("MaaS prompt missing %q:\n%s", want, gotPrompt)
		}
	}
	messages, err := repo.ListAgentMessages(context.Background(), domain.AgentMessageQuery{ToAgentID: "writer"})
	if err != nil {
		t.Fatalf("ListAgentMessages() error = %v, want nil", err)
	}
	if len(messages) != 1 || messages[0].Status != domain.AgentMessageRead || messages[0].ReadAt.IsZero() {
		t.Fatalf("message after @writer --inbox = %#v, want read with read_at", messages)
	}
}

func TestRunTUITaskKeepsMentionedAgentInboxUnreadWhenRunFails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model unavailable", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	repo := openCLITestSQLiteRepository(t)
	if err := repo.SaveAgentMessage(context.Background(), domain.AgentMessage{
		ID:          "msg-retry-1",
		FromAgentID: "researcher",
		ToAgentID:   "writer",
		Type:        domain.AgentMessageTypeMessage,
		Status:      domain.AgentMessageUnread,
		Summary:     "失败后需要保留未读",
		CreatedAt:   time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveAgentMessage() error = %v, want nil", err)
	}
	root := t.TempDir()
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"writer": {
			ID:           "writer",
			Role:         "writer",
			MaasProfile:  "review",
			ContextFiles: config.ContextFilesConfig{Root: root},
		},
	})
	cfg := config.Config{
		Maas: config.MaasConfig{Profiles: map[string]config.MaasProfile{
			"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
		}},
		Runtime:      config.RuntimeConfig{MaxToolRounds: 1},
		ContextFiles: config.ContextFilesConfig{Root: root},
	}

	_, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:       cfg,
		Registry:     registry,
		Prompt:       "@writer --inbox 根据最新消息整理成说明",
		MessageStore: repo,
	})
	if err == nil {
		t.Fatalf("runTUITask(@writer --inbox failed model) error = nil, want error")
	}
	messages, listErr := repo.ListAgentMessages(context.Background(), domain.AgentMessageQuery{ToAgentID: "writer"})
	if listErr != nil {
		t.Fatalf("ListAgentMessages() error = %v, want nil", listErr)
	}
	if len(messages) != 1 || messages[0].Status != domain.AgentMessageUnread || !messages[0].ReadAt.IsZero() {
		t.Fatalf("message after failed @writer --inbox = %#v, want still unread", messages)
	}
}

func TestRunTUITaskReturnsErrorForUnknownMentionedAgent(t *testing.T) {
	t.Parallel()

	_, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:   config.Config{},
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{}),
		Prompt:   "@unknown 做事",
	})
	if err == nil {
		t.Fatalf("runTUITask(@unknown) error = nil, want error")
	}
	if !strings.Contains(err.Error(), `agent "unknown" not configured`) {
		t.Fatalf("runTUITask(@unknown) error = %v, want not configured", err)
	}
}

// TestRunMentionedTUIAgentTaskAppliesSessionModeAndWorkingDir closes a
// cross-entry-point gap found in review of Task 4: runMentionedTUIAgentTask
// is a second TUI task execution path (reached whenever the prompt starts
// with "@agent"), and it built its own app.RunTaskOptions literal without
// the Mode/WorkingDir fields Task 4 added to the default runTUITask path.
// That let an @mention task bypass both the session's working_dir sandbox
// and Plan mode's read-only tool subset. This test drives the exact same
// scenarios as TestRunTUITaskAppliesSessionModeAndWorkingDir (session
// working_dir sandboxes the tool root; session Plan mode blocks
// write_file), but through the @mention path. Unlike the default path,
// runMentionedTUIAgentTask always builds its MaaS client from config via
// maasFactoryFromConfig (never accepts cfg.DefaultMaas directly), so the
// tool-call round trip is driven through a real HTTP-backed MaaS profile
// (mirroring the openai-chat-shaped mock servers used by the other
// @mention tests above) instead of an in-process fake like
// toolProbingMaas.
func TestRunMentionedTUIAgentTaskAppliesSessionModeAndWorkingDir(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:     repo,
		Enabled:   true,
		CompanyID: "cli-company",
		AgentID:   "cli-agent",
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	workingDir := t.TempDir()
	if err := session.SetWorkingDir(ctx, workingDir); err != nil {
		t.Fatalf("SetWorkingDir(%q) error = %v, want nil", workingDir, err)
	}
	writeCLIFile(t, workingDir, "inside.txt", "inside-content")

	// The agent's own ContextFiles.Root is a different directory than the
	// session working_dir, so a tool call reaching workingDir only succeeds
	// once RunTaskOptions.WorkingDir (sourced from the session, taking
	// priority over ToolRoot per app.RunTaskOptions.WorkingDir's contract)
	// is actually wired through.
	agentContextRoot := t.TempDir()
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"researcher": {
			ID:          "researcher",
			Role:        "researcher",
			MaasProfile: "review",
			ContextFiles: config.ContextFilesConfig{
				Root: agentContextRoot,
			},
		},
	})

	// The two subtests intentionally run sequentially (no t.Parallel()):
	// they share the single session/store above, mirroring
	// TestRunTUITaskAppliesSessionModeAndWorkingDir's sequencing (the
	// second subtest depends on SetMode(plan) applied only after the first
	// subtest observes auto mode).
	t.Run("session working_dir sandboxes the mentioned agent's tool root", func(t *testing.T) {
		requestCount := 0
		var lastPrompt string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(request body) error = %v, want nil", err)
			}
			// Flatten every turn: with multi-turn requests the tool output arrives
			// as its own tool message instead of being appended to the first one.
			var flattened strings.Builder
			for _, msg := range req.Messages {
				flattened.WriteString(msg.Content)
				flattened.WriteString("\n")
			}
			lastPrompt = flattened.String()
			w.Header().Set("Content-Type", "application/json")
			if requestCount == 1 {
				argsJSON, err := json.Marshal(map[string]string{"path": filepath.Join(workingDir, "inside.txt")})
				if err != nil {
					t.Fatalf("Marshal(read_file args) error = %v, want nil", err)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{"tool_calls": []map[string]any{
							{"id": "call-1", "type": "function", "function": map[string]any{
								"name":      "read_file",
								"arguments": string(argsJSON),
							}},
						}}},
					},
				}); err != nil {
					t.Fatalf("Encode(tool_calls response) error = %v, want nil", err)
				}
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"content": "mention-done"}},
				},
			}); err != nil {
				t.Fatalf("Encode(final response) error = %v, want nil", err)
			}
		}))
		t.Cleanup(server.Close)

		cfg := config.Config{
			Maas: config.MaasConfig{
				Profiles: map[string]config.MaasProfile{
					"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
				},
			},
			Runtime: config.RuntimeConfig{MaxToolRounds: 2},
		}
		result, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
			Config:   cfg,
			Registry: registry,
			Prompt:   "@researcher read the session working dir file",
			Session:  session,
		})
		if err != nil {
			t.Fatalf("runTUITask(@researcher, session working_dir) error = %v, want nil", err)
		}
		if result.Result != "mention-done" {
			t.Fatalf("runTUITask(@researcher, session working_dir).Result = %q, want %q", result.Result, "mention-done")
		}
		if !strings.Contains(lastPrompt, "inside-content") {
			t.Fatalf("runTUITask(@researcher, session working_dir) prompt = %q, want tool success reading inside.txt", lastPrompt)
		}
	})

	t.Run("session Plan mode blocks the mentioned agent's write_file", func(t *testing.T) {
		if err := session.SetMode(ctx, domain.ModePlan); err != nil {
			t.Fatalf("SetMode(plan) error = %v, want nil", err)
		}
		target := filepath.Join(workingDir, "should-not-exist.txt")

		requestCount := 0
		var lastPrompt string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("Decode(request body) error = %v, want nil", err)
			}
			// Flatten every turn: with multi-turn requests the tool output arrives
			// as its own tool message instead of being appended to the first one.
			var flattened strings.Builder
			for _, msg := range req.Messages {
				flattened.WriteString(msg.Content)
				flattened.WriteString("\n")
			}
			lastPrompt = flattened.String()
			w.Header().Set("Content-Type", "application/json")
			if requestCount == 1 {
				argsJSON, err := json.Marshal(map[string]string{"path": target, "content": "package foo\n"})
				if err != nil {
					t.Fatalf("Marshal(write_file args) error = %v, want nil", err)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{
						{"message": map[string]any{"tool_calls": []map[string]any{
							{"id": "call-1", "type": "function", "function": map[string]any{
								"name":      "write_file",
								"arguments": string(argsJSON),
							}},
						}}},
					},
				}); err != nil {
					t.Fatalf("Encode(tool_calls response) error = %v, want nil", err)
				}
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"content": "写不了"}},
				},
			}); err != nil {
				t.Fatalf("Encode(final response) error = %v, want nil", err)
			}
		}))
		t.Cleanup(server.Close)

		cfg := config.Config{
			Maas: config.MaasConfig{
				Profiles: map[string]config.MaasProfile{
					"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
				},
			},
			Runtime: config.RuntimeConfig{MaxToolRounds: 2},
		}
		result, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
			Config:   cfg,
			Registry: registry,
			Prompt:   "@researcher 写一个文件",
			Session:  session,
		})
		if err != nil {
			t.Fatalf("runTUITask(@researcher, session plan mode) error = %v, want nil", err)
		}
		if result.Result != "写不了" {
			t.Fatalf("runTUITask(@researcher, session plan mode).Result = %q, want final answer text", result.Result)
		}
		if !strings.Contains(lastPrompt, "failed: "+tool.ErrToolNotFound.Error()) {
			t.Fatalf("runTUITask(@researcher, session plan mode) prompt = %q, want write_file rejected as tool not found", lastPrompt)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("runTUITask(@researcher, session plan mode) created %q on disk, want Plan mode to block the write", target)
		}
	})
}

func TestRunTUITaskInjectsAndPersistsSessionTurns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        repo,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
		RecentTurns:  4,
		MaxTurnChars: 6000,
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleUser, "task-old", "cli-agent", "dev", "你是什么模型"); err != nil {
		t.Fatalf("record previous user turn error = %v, want nil", err)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleAssistant, "task-old", "cli-agent", "dev", "我是 Legion Agent"); err != nil {
		t.Fatalf("record previous assistant turn error = %v, want nil", err)
	}
	maas := &cliCaptureMaas{response: "第三点是上下文连续性"}
	result, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
		Config: config.Config{
			Runtime: config.RuntimeConfig{MaxToolRounds: 1},
			Session: config.SessionConfig{Enabled: true, DefaultRecentTurns: 4, MaxTurnChars: 6000},
		},
		Prompt:      "展开刚才的第三点",
		DefaultMaas: maas,
		Session:     session,
	})
	if err != nil {
		t.Fatalf("runTUITask(session) error = %v, want nil", err)
	}
	if result.Result != "第三点是上下文连续性" {
		t.Fatalf("runTUITask(session).Result = %q, want model response", result.Result)
	}
	for _, want := range []string{"Recent conversation:", "你是什么模型", "我是 Legion Agent", "展开刚才的第三点"} {
		if !strings.Contains(maas.prompt, want) {
			t.Fatalf("MaaS prompt missing %q:\n%s", want, maas.prompt)
		}
	}
	turns, err := repo.ListConversationTurns(ctx, session.CurrentSessionID(), 0)
	if err != nil {
		t.Fatalf("ListConversationTurns() error = %v, want nil", err)
	}
	if len(turns) != 4 {
		t.Fatalf("ListConversationTurns() len = %d, want 4 turns: %#v", len(turns), turns)
	}
	if turns[2].Role != domain.ConversationRoleUser || turns[2].Content != "展开刚才的第三点" {
		t.Fatalf("new user turn = %#v, want current prompt", turns[2])
	}
	if turns[3].Role != domain.ConversationRoleAssistant || turns[3].Content != "第三点是上下文连续性" {
		t.Fatalf("new assistant turn = %#v, want current response", turns[3])
	}
}

// TestRunTUITaskAppliesSessionModeAndWorkingDir guards Task 4's runTUITask
// wiring: the default (non-mentioned-agent) task path must read
// SessionManager.CurrentMode/CurrentWorkingDir and forward them into
// app.RunTaskOptions, so the session's bound working_dir becomes the tool
// sandbox root and the session's Plan mode restricts the model to read-only
// tools — end to end, driven through runTUITask exactly as the TUI calls it,
// with real tool dispatch (toolProbingMaas, defined above for
// TestDefaultTaskRunnerSandboxesToolsToTaskWorkingDir) instead of a mocked
// tool layer.
func TestRunTUITaskAppliesSessionModeAndWorkingDir(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:     repo,
		Enabled:   true,
		CompanyID: "cli-company",
		AgentID:   "cli-agent",
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	workingDir := t.TempDir()
	if err := session.SetWorkingDir(ctx, workingDir); err != nil {
		t.Fatalf("SetWorkingDir(%q) error = %v, want nil", workingDir, err)
	}
	writeCLIFile(t, workingDir, "inside.txt", "inside-content")

	// The two subtests intentionally run sequentially (no t.Parallel()): they
	// share the single session/store above, and the second subtest depends on
	// SetMode(plan) applied only after the first subtest observes auto mode.
	t.Run("session working_dir sandboxes the tool root", func(t *testing.T) {
		maas := &toolProbingMaas{path: filepath.Join(workingDir, "inside.txt")}
		result, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
			Config:      config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 2}},
			Prompt:      "read the session working dir file",
			DefaultMaas: maas,
			Session:     session,
		})
		if err != nil {
			t.Fatalf("runTUITask(session working_dir) error = %v, want nil", err)
		}
		if result.Result != "done" {
			t.Fatalf("runTUITask(session working_dir).Result = %q, want %q", result.Result, "done")
		}
		if !strings.Contains(maas.lastPrompt, "inside-content") {
			t.Fatalf("runTUITask(session working_dir) prompt = %q, want tool success reading inside.txt", maas.lastPrompt)
		}
	})

	t.Run("session Plan mode blocks write_file", func(t *testing.T) {
		if err := session.SetMode(ctx, domain.ModePlan); err != nil {
			t.Fatalf("SetMode(plan) error = %v, want nil", err)
		}
		target := filepath.Join(workingDir, "should-not-exist.txt")
		maas := &appPlanWriteFileMaas{path: target}
		result, err := runTUITask(ctx, app.New(), tuiTaskRunConfig{
			Config:      config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 2}},
			Prompt:      "写一个文件",
			DefaultMaas: maas,
			Session:     session,
		})
		if err != nil {
			t.Fatalf("runTUITask(session plan mode) error = %v, want nil", err)
		}
		if result.Result != "写不了" {
			t.Fatalf("runTUITask(session plan mode).Result = %q, want final answer text", result.Result)
		}
		if !strings.Contains(maas.lastPrompt, "failed: "+tool.ErrToolNotFound.Error()) {
			t.Fatalf("runTUITask(session plan mode) prompt = %q, want write_file rejected as tool not found", maas.lastPrompt)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("runTUITask(session plan mode) created %q on disk, want Plan mode to block the write", target)
		}
	})
}

// appPlanWriteFileMaas emits a single write_file tool call on the first
// inference, then returns a final text answer, capturing the last prompt so
// a test can assert whether the write reached the tool registry (see
// toolProbingMaas above for the same pattern applied to read_file).
type appPlanWriteFileMaas struct {
	path       string
	rounds     int
	lastPrompt string
}

func (m *appPlanWriteFileMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.rounds++
	m.lastPrompt = testsupport.RequestText(req)
	if m.rounds == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "plan-write-1",
			Name:      "write_file",
			Arguments: map[string]string{"path": m.path, "content": "package foo\n"},
		}}}, nil
	}
	return port.InferenceResponse{Text: "写不了"}, nil
}

func TestTUISessionControllerCachesRecentTurnsAndInvalidatesAfterRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	store := &countingConversationStore{delegate: repo}
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        store,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
		RecentTurns:  4,
		MaxTurnChars: 6000,
		Cache:        sessioncache.NewMemoryCache(8),
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleUser, "task-1", "cli-agent", "dev", "first"); err != nil {
		t.Fatalf("recordTurn(first) error = %v, want nil", err)
	}

	if _, err := session.RecentTurns(ctx); err != nil {
		t.Fatalf("RecentTurns(first) error = %v, want nil", err)
	}
	if _, err := session.RecentTurns(ctx); err != nil {
		t.Fatalf("RecentTurns(second) error = %v, want nil", err)
	}
	if store.listConversationTurnsCalls != 1 {
		t.Fatalf("ListConversationTurns calls = %d, want 1 after cache hit", store.listConversationTurnsCalls)
	}
	if err := session.recordTurn(ctx, domain.ConversationRoleAssistant, "task-1", "cli-agent", "dev", "second"); err != nil {
		t.Fatalf("recordTurn(second) error = %v, want nil", err)
	}
	if _, err := session.RecentTurns(ctx); err != nil {
		t.Fatalf("RecentTurns(after record) error = %v, want nil", err)
	}
	if store.listConversationTurnsCalls != 2 {
		t.Fatalf("ListConversationTurns calls = %d, want 2 after invalidation", store.listConversationTurnsCalls)
	}
}

func TestTUISessionControllerSetAndGetMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        repo,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	if got := session.CurrentMode(); got != domain.ModeAuto {
		t.Fatalf("CurrentMode() (fresh session) = %q, want %q", got, domain.ModeAuto)
	}

	if err := session.SetMode(ctx, "manual"); err != nil {
		t.Fatalf("SetMode(manual) error = %v, want nil", err)
	}
	if got := session.CurrentMode(); got != "manual" {
		t.Fatalf("CurrentMode() = %q, want %q", got, "manual")
	}

	// Persistence: re-fetch the session from the store directly.
	sessions, err := repo.ListAgentSessions(ctx, "cli-company", "cli-agent")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	var found bool
	for _, s := range sessions {
		if s.ID == session.CurrentSessionID() {
			found = true
			if s.Mode != "manual" {
				t.Fatalf("persisted AgentSession.Mode = %q, want %q", s.Mode, "manual")
			}
		}
	}
	if !found {
		t.Fatalf("session %q not found in store", session.CurrentSessionID())
	}

	if err := session.SetMode(ctx, "bogus"); err == nil {
		t.Fatalf("SetMode(bogus) error = nil, want error")
	}
	if got := session.CurrentMode(); got != "manual" {
		t.Fatalf("CurrentMode() after rejected SetMode = %q, want unchanged %q", got, "manual")
	}
}

func TestTUISessionControllerSetAndGetWorkingDir(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        repo,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}
	if got := session.CurrentWorkingDir(); got != "" {
		t.Fatalf("CurrentWorkingDir() (fresh session) = %q, want empty", got)
	}

	dir := t.TempDir()
	if err := session.SetWorkingDir(ctx, dir); err != nil {
		t.Fatalf("SetWorkingDir(%q) error = %v, want nil", dir, err)
	}
	if got := session.CurrentWorkingDir(); got != dir {
		t.Fatalf("CurrentWorkingDir() = %q, want %q", got, dir)
	}

	sessions, err := repo.ListAgentSessions(ctx, "cli-company", "cli-agent")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	var found bool
	for _, s := range sessions {
		if s.ID == session.CurrentSessionID() {
			found = true
			if s.WorkingDir != dir {
				t.Fatalf("persisted AgentSession.WorkingDir = %q, want %q", s.WorkingDir, dir)
			}
		}
	}
	if !found {
		t.Fatalf("session %q not found in store", session.CurrentSessionID())
	}

	notDir := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", notDir, err)
	}
	if err := session.SetWorkingDir(ctx, notDir); err == nil {
		t.Fatalf("SetWorkingDir(%q) error = nil, want error (not a directory)", notDir)
	}
	if got := session.CurrentWorkingDir(); got != dir {
		t.Fatalf("CurrentWorkingDir() after rejected SetWorkingDir = %q, want unchanged %q", got, dir)
	}
}

// TestTUISessionControllerWorkingDirSetOnce verifies the TUI session
// controller enforces the same set-once-then-immutable WorkingDir semantics
// as the HTTP server's handlePatchSession (server/http.go): once a session's
// working_dir is non-empty, changing it to a different directory must be
// rejected, because on-disk session state (checkpoints, approval tickets) is
// filed under whatever working_dir was in effect at write time, and
// repointing it would silently strand that state.
func TestTUISessionControllerWorkingDirSetOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openCLITestSQLiteRepository(t)
	session := newTUISessionController(tuiSessionControllerConfig{
		Store:        repo,
		Enabled:      true,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
	})
	if _, err := session.NewSession(ctx); err != nil {
		t.Fatalf("NewSession() error = %v, want nil", err)
	}

	dirA := t.TempDir()
	dirB := t.TempDir()

	// First set: working_dir is currently empty, so this must succeed.
	if err := session.SetWorkingDir(ctx, dirA); err != nil {
		t.Fatalf("SetWorkingDir(%q) (first set) error = %v, want nil", dirA, err)
	}
	if got := session.CurrentWorkingDir(); got != dirA {
		t.Fatalf("CurrentWorkingDir() = %q, want %q", got, dirA)
	}

	// Changing to a different directory once set must be rejected.
	if err := session.SetWorkingDir(ctx, dirB); err == nil {
		t.Fatalf("SetWorkingDir(%q) (change after set) error = nil, want error", dirB)
	}
	if got := session.CurrentWorkingDir(); got != dirA {
		t.Fatalf("CurrentWorkingDir() after rejected change = %q, want unchanged %q", got, dirA)
	}

	// Persisted state must remain dirA, not dirB.
	sessions, err := repo.ListAgentSessions(ctx, "cli-company", "cli-agent")
	if err != nil {
		t.Fatalf("ListAgentSessions() error = %v, want nil", err)
	}
	var found bool
	for _, s := range sessions {
		if s.ID == session.CurrentSessionID() {
			found = true
			if s.WorkingDir != dirA {
				t.Fatalf("persisted AgentSession.WorkingDir = %q, want unchanged %q", s.WorkingDir, dirA)
			}
		}
	}
	if !found {
		t.Fatalf("session %q not found in store", session.CurrentSessionID())
	}

	// Re-setting the same value is a no-op and must succeed.
	if err := session.SetWorkingDir(ctx, dirA); err != nil {
		t.Fatalf("SetWorkingDir(%q) (same value, no-op) error = %v, want nil", dirA, err)
	}
	if got := session.CurrentWorkingDir(); got != dirA {
		t.Fatalf("CurrentWorkingDir() after no-op set = %q, want %q", got, dirA)
	}
}

// TestTUISessionControllerSetModeWhenDisabledFailsLoud verifies that when the
// session feature is disabled (cfg.Session.Enabled == false), SetMode and
// SetWorkingDir fail loudly with an error instead of silently no-op'ing.
// Previously both returned nil when disabled, so a user typing "/mode
// manual" or "/cwd <dir>" saw no error and no state change -- their explicit
// intent was silently swallowed (violates the fail-loud rule: CLAUDE.md
// section 0). CurrentMode/CurrentWorkingDir (the read paths) are unaffected
// by this fix: returning defaults when disabled remains legitimate, since
// there is no session to read from.
func TestTUISessionControllerSetModeWhenDisabledFailsLoud(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session := newTUISessionController(tuiSessionControllerConfig{
		Enabled:      false,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
	})

	if err := session.SetMode(ctx, "manual"); err == nil {
		t.Fatalf("SetMode(manual) on disabled session error = nil, want error")
	}
	if got := session.CurrentMode(); got != domain.ModeAuto {
		t.Fatalf("CurrentMode() after rejected SetMode = %q, want unchanged %q", got, domain.ModeAuto)
	}
}

// TestTUISessionControllerSetWorkingDirWhenDisabledFailsLoud is the
// SetWorkingDir counterpart to TestTUISessionControllerSetModeWhenDisabledFailsLoud.
func TestTUISessionControllerSetWorkingDirWhenDisabledFailsLoud(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session := newTUISessionController(tuiSessionControllerConfig{
		Enabled:      false,
		CompanyID:    "cli-company",
		AgentID:      "cli-agent",
		ModelProfile: "dev",
	})

	dir := t.TempDir()
	if err := session.SetWorkingDir(ctx, dir); err == nil {
		t.Fatalf("SetWorkingDir(%q) on disabled session error = nil, want error", dir)
	}
	if got := session.CurrentWorkingDir(); got != "" {
		t.Fatalf("CurrentWorkingDir() after rejected SetWorkingDir = %q, want unchanged empty", got)
	}
}

func TestLoadServeAgentRegistryReturnsMissingChildError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"agents": {"researcher": "agents/missing.json"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}
	cfg, err := config.Load(context.Background(), config.Options{Path: configPath})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", configPath, err)
	}

	_, err = loadServeAgentRegistry(context.Background(), cfg, configPath)
	if err == nil {
		t.Fatalf("loadServeAgentRegistry() error = nil, want missing agent config")
	}
	if !strings.Contains(err.Error(), "agent config not found") {
		t.Fatalf("loadServeAgentRegistry() error = %v, want agent config not found", err)
	}
}

func TestRunCommandMaasURLOverridesProfile(t *testing.T) {
	t.Parallel()
	profileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("profile server called, want explicit --maas-url override")
	}))
	t.Cleanup(profileServer.Close)
	overrideCalled := false
	overrideServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overrideCalled = true
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "override result"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(overrideServer.Close)
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"maas": {
			"default_profile": "review",
			"profiles": {
				"review": {"base_url": "`+profileServer.URL+`", "api_key": "profile-key"}
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--maas-profile", "review",
		"--maas-url", overrideServer.URL,
		"--prompt", "Use override",
		"--no-context-files",
	})
	if err != nil {
		t.Fatalf("Execute(run --maas-url --maas-profile) error = %v, want nil", err)
	}
	if !overrideCalled {
		t.Fatalf("override MaaS server called = false, want true")
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte(`result="override result"`)) {
		t.Fatalf("Execute(run --maas-url --maas-profile) output = %q, want override result", got)
	}
}

func TestRunCommandPersistsToSQLiteWhenConfigured(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{
		"run",
		"--plain",
		"--config", configPath,
		"--prompt", "Persist from CLI",
	})
	if err != nil {
		t.Fatalf("Execute(run --config sqlite --prompt) error = %v, want nil", err)
	}
	repo, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	audits, err := repo.ListAuditEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) == 0 {
		t.Fatalf("ListAuditEvents() len = 0, want persisted audit events")
	}
	task, ok, err := repo.GetTask(context.Background(), audits[0].SubjectID)
	if err != nil {
		t.Fatalf("GetTask(%q) error = %v, want nil", audits[0].SubjectID, err)
	}
	if !ok || task.Input != "Persist from CLI" {
		t.Fatalf("GetTask(%q) = %#v, %t, want persisted CLI task", audits[0].SubjectID, task, ok)
	}
	events, err := repo.ListRuntimeEvents(context.Background())
	if err != nil {
		t.Fatalf("ListRuntimeEvents() error = %v, want nil", err)
	}
	if len(events) == 0 {
		t.Fatalf("ListRuntimeEvents() len = 0, want persisted events")
	}
}

func TestRunCommandUsesUniqueTaskIDsForPersistentRuns(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	for _, prompt := range []string{"First persistent CLI run", "Second persistent CLI run"} {
		var out bytes.Buffer
		err := Execute(app.New(), &out, []string{
			"run",
			"--plain",
			"--config", configPath,
			"--prompt", prompt,
		})
		if err != nil {
			t.Fatalf("Execute(run --config sqlite --prompt %q) error = %v, want nil", prompt, err)
		}
	}

	repo, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	audits, err := repo.ListAuditEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	taskIDs := make(map[string]struct{})
	for _, event := range audits {
		if event.Action == "model_inference_completed" {
			taskIDs[event.SubjectID] = struct{}{}
		}
	}
	if len(taskIDs) != 2 {
		t.Fatalf("model audit task IDs = %#v, want 2 unique task IDs", taskIDs)
	}
}

func TestServeCommandUsesSQLiteForHTTPTaskState(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
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
	postURL := "http://" + addr + "/v1/tasks"
	resp, err := waitForPostTask(t, postURL, `{"id":"task-api-1","company_id":"company-1","input":"persist api"}`, done)
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
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute(serve sqlite) error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute(serve sqlite) did not stop")
	}
	repo, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	task, ok, err := repo.GetTask(context.Background(), "task-api-1")
	if err != nil {
		t.Fatalf("GetTask(task-api-1) error = %v, want nil", err)
	}
	if !ok || task.Input != "persist api" {
		t.Fatalf("GetTask(task-api-1) = %#v, %t, want persisted API task", task, ok)
	}
}

// TestServeCommandPersistsTerminalTaskStatus pins the durable half of task
// state: a task that actually ran must not still read "pending" in SQLite once
// serve is gone. Before the scheduler wrote transitions through, every row in
// the tasks table kept the status it was created with forever, so a restarted
// serve answered GET /v1/tasks/{id} with "pending" for a task that had long
// since finished -- a stored value that looks healthy and is wrong.
func TestServeCommandPersistsTerminalTaskStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configPath := filepath.Join(t.TempDir(), "agent.json")
	// background_interval must be short enough that the coordinator heartbeat
	// dispatches the posted task inside the test window (see the comment in
	// TestServeCommandStreamsLifecycleEventsOverSSE); no maas config means the
	// demo recording client runs the task offline and deterministically.
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"},
		"service": {"background_interval": "20ms"}
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
	go func() { done <- root.Execute() }()

	resp, err := waitForPostTask(t, "http://"+addr+"/v1/tasks",
		`{"id":"persist-status-1","company_id":"c1","input":"finish me"}`, done)
	if err != nil {
		cancel()
		t.Fatalf("POST /v1/tasks error = %v, want nil", err)
	}
	status := resp.StatusCode
	if err := resp.Body.Close(); err != nil {
		cancel()
		t.Fatalf("Body.Close() error = %v, want nil", err)
	}
	// waitForPostTask retries transport errors only, so a 5xx from SQLite under
	// this package's parallel load would otherwise surface ten seconds later as
	// "the task never ran" and read like a persistence failure.
	if status != http.StatusCreated {
		cancel()
		t.Fatalf("POST /v1/tasks status = %d, want %d", status, http.StatusCreated)
	}

	// Wait for the live scheduler to report a terminal state before shutting
	// serve down, so the assertion below is about persistence and not about
	// the task never having run.
	liveStatus := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && liveStatus != string(domain.TaskDone) && liveStatus != string(domain.TaskFailed) {
		getResp, getErr := http.Get("http://" + addr + "/v1/tasks/persist-status-1")
		if getErr != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var payload struct {
			Status string `json:"status"`
		}
		decErr := json.NewDecoder(getResp.Body).Decode(&payload)
		if closeErr := getResp.Body.Close(); closeErr != nil {
			t.Fatalf("Body.Close() error = %v, want nil", closeErr)
		}
		if decErr == nil {
			liveStatus = payload.Status
		}
		if liveStatus == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	cancel()
	select {
	case execErr := <-done:
		if execErr != nil {
			t.Fatalf("Execute(serve) error = %v, want nil", execErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute(serve) did not stop")
	}
	if liveStatus != string(domain.TaskDone) && liveStatus != string(domain.TaskFailed) {
		t.Fatalf("live task status = %q, want a terminal status; the task never ran so persistence cannot be judged", liveStatus)
	}

	repo, err := storage.OpenSQLite(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	persisted, ok, err := repo.GetTask(context.Background(), "persist-status-1")
	if err != nil {
		t.Fatalf("GetTask(persist-status-1) error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("GetTask(persist-status-1) ok = false, want true")
	}
	if string(persisted.Status) != liveStatus {
		t.Errorf("persisted status = %q, want %q (the status the task actually reached)", persisted.Status, liveStatus)
	}
}

func TestServeCommandEventsEndpointNotUnavailable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"service": {"background_interval": "1h"}}`), 0o600); err != nil {
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
	go func() { done <- root.Execute() }()

	// Poll until the server is listening, then open the SSE stream.
	streamCtx, streamCancel := context.WithTimeout(ctx, 3*time.Second)
	defer streamCancel()
	var status int
	var contentType string
	for range 100 {
		req, reqErr := http.NewRequestWithContext(streamCtx, http.MethodGet, "http://"+addr+"/v1/events", nil)
		if reqErr != nil {
			t.Fatalf("NewRequest(/v1/events) error = %v, want nil", reqErr)
		}
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		status = resp.StatusCode
		contentType = resp.Header.Get("Content-Type")
		_ = resp.Body.Close()
		break
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
	if status == http.StatusServiceUnavailable {
		t.Fatal("GET /v1/events status = 503, want SSE wired (503 death-code regression)")
	}
	if status != http.StatusOK {
		t.Fatalf("GET /v1/events status = %d, want 200", status)
	}
	if !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("GET /v1/events Content-Type = %q, want text/event-stream", contentType)
	}
}

func TestServeCommandStreamsLifecycleEventsOverSSE(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configPath := filepath.Join(t.TempDir(), "agent.json")
	// A short background_interval (matching TestTaskSubmitEmitsCompletedEvent
	// in gateway_contract_test.go) is required so the coordinator heartbeat
	// actually runs and dispatches the posted task within the test window;
	// the service.BackgroundScheduler ticker (internal/task/background_scheduler.go)
	// fires no immediate tick, so background_interval: "1h" would never
	// process the task inside this test's 5s deadline.
	if err := os.WriteFile(configPath, []byte(`{"service": {"background_interval": "20ms"}}`), 0o600); err != nil {
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
	go func() { done <- root.Execute() }()

	// Drive one task to completion (demo maas), so task_started/task_completed
	// are buffered on the platform bus.
	resp, err := waitForPostTask(t, "http://"+addr+"/v1/tasks",
		`{"id":"sse-task-1","company_id":"c1","input":"hello sse"}`, done)
	if err != nil {
		cancel()
		t.Fatalf("POST /v1/tasks error = %v, want nil", err)
	}
	if err := resp.Body.Close(); err != nil {
		cancel()
		t.Fatalf("Body.Close() error = %v, want nil", err)
	}

	// Subscribe AFTER the task ran; buffered events are replayed to new
	// subscribers, so task_completed is deterministically available.
	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	found := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !found {
		req, reqErr := http.NewRequestWithContext(streamCtx, http.MethodGet,
			"http://"+addr+"/v1/events?type=task_completed", nil)
		if reqErr != nil {
			t.Fatalf("NewRequest error = %v, want nil", reqErr)
		}
		sseResp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		buf := make([]byte, 4096)
		n, _ := sseResp.Body.Read(buf) // one read is enough; replay is immediate
		_ = sseResp.Body.Close()
		if n > 0 && strings.Contains(string(buf[:n]), "event: task_completed") {
			found = true
		}
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
		t.Fatal("GET /v1/events never streamed task_completed, want lifecycle event bridged to SSE")
	}
}

// TestServeCommandSandboxesTaskToolsToSessionWorkingDir is the M3a Task 9
// integration gate. It was originally written as a full network e2e (spin up
// `serve`, POST /v1/sessions and /v1/tasks over real HTTP, wait for the
// background scheduler to dispatch and complete the task). That version
// reliably passed standalone but proved flaky under the same load this
// package's own parallel test suite creates: repeated full-package runs
// intermittently hit a transient 500 from session/task creation (SQLite
// under concurrent access from this test's own goroutines plus every other
// parallel *_test.go server in the package — internal/storage/sqlite.go
// opens no busy_timeout, a pre-existing gap unrelated to working_dir) and, in
// one run, a task not completing inside a 15s deadline. Per the brief's
// explicit "若 serve 级 e2e 过重/易 flaky,退化为 runtime 层集成测试" escape
// hatch, this test now drives the real HTTP handlers synchronously (no
// listener, no coordinator/background-scheduler timing, no goroutines) and
// then feeds the resulting HTTP+SQLite-round-tripped domain.Task straight
// into the exact TaskRunner the coordinator would have dispatched to
// (defaultTaskRunner, Task 7), closing the loop deterministically:
//
//  1. POST /v1/sessions with working_dir (server.NewHTTPServer's real
//     handleCreateSession, Task 6) -> session.WorkingDir persisted.
//  2. POST /v1/tasks with session_id (real handleCreateTask, Task 6) ->
//     task.WorkingDir inherited from the session and validated to exist.
//  3. That real, HTTP-created-and-SQLite-persisted domain.Task is handed to
//     defaultTaskRunner.RunTask (the same TaskRunner coordinator.go dispatches
//     a task with no agent_id to) with a scripted MaaS issuing a read_file
//     call — proving the sandbox root really is the session's working_dir:
//     a file inside it is readable, and a `../` escape from it is rejected
//     with port.ErrPathOutsideWorkspace (Task 7).
//
// TestCreateTaskInheritsSessionWorkingDir (internal/server/http_test.go, Task
// 6) already covers step 1-2 in isolation, and
// TestDefaultTaskRunnerSandboxesToolsToTaskWorkingDir (this package, Task 7)
// already covers step 3 with a hand-built task. What only this test proves is
// that the two halves compose: the exact task object that flowed through the
// real HTTP + storage layer is the one the sandbox enforces against.
//
// It exercises read_file to check the read-side sandbox boundary. The serve and
// per-agent paths now build tool.NewFileReadWriteWorkspaceRegistry, so they also
// carry write_file (sandboxed to the same root, still Sensitive); that write
// capability is covered by TestDefaultTaskRunnerCanWriteFile here and by
// TestResolverGivesWorkerWriteFile in package runtime.
func TestServeCommandSandboxesTaskToolsToSessionWorkingDir(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	writeCLIFile(t, workingDir, "inside.txt", "inside-content")
	// A real file at the `../` escape target proves the rejection is an actual
	// sandbox-boundary check, not merely "the file happens not to exist".
	writeCLIFile(t, filepath.Dir(workingDir), "outside.txt", "outside-content")

	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	srv := server.NewHTTPServer(server.Config{Sessions: repo, Tasks: repo})

	sessionRec := httptest.NewRecorder()
	sessionReq := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(
		`{"company_id":"c1","agent_id":"a1","working_dir":"`+filepath.ToSlash(workingDir)+`"}`))
	srv.ServeHTTP(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/sessions status = %d, want %d body=%s", sessionRec.Code, http.StatusCreated, sessionRec.Body.String())
	}
	var session domain.AgentSession
	if err := json.Unmarshal(sessionRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("Decode(session) error = %v, want nil, body=%s", err, sessionRec.Body.String())
	}
	if session.WorkingDir != filepath.ToSlash(workingDir) {
		t.Fatalf("session.WorkingDir = %q, want %q", session.WorkingDir, filepath.ToSlash(workingDir))
	}

	createTask := func(taskID string) domain.Task {
		t.Helper()
		body := `{"id":"` + taskID + `","company_id":"c1","agent_id":"a1","session_id":"` + session.ID + `","input":"read a file"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /v1/tasks(%s) status = %d, want %d body=%s", taskID, rec.Code, http.StatusCreated, rec.Body.String())
		}
		var task domain.Task
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("Decode(task %s) error = %v, want nil, body=%s", taskID, err, rec.Body.String())
		}
		if task.WorkingDir != filepath.ToSlash(workingDir) {
			t.Fatalf("task(%s).WorkingDir = %q, want inherited %q", taskID, task.WorkingDir, filepath.ToSlash(workingDir))
		}
		return task
	}

	insideTask := createTask("wd-e2e-inside")
	escapeTask := createTask("wd-e2e-escape")

	newRunner := func() *defaultTaskRunner {
		return &defaultTaskRunner{
			runtimeCfg: agentruntime.Config{Events: adapter.NewMemoryEventBus()},
			// contextRoot is never consulted here: both tasks above carry a
			// non-empty WorkingDir (inherited from the session), and
			// defaultTaskRunner.RunTask prioritizes task.WorkingDir over it.
			contextRoot: t.TempDir(),
			audit:       adapter.NewMemoryAuditLog(),
			webOptions:  tool.WebToolOptions{},
		}
	}

	t.Run("read_file inside the HTTP-inherited working_dir succeeds", func(t *testing.T) {
		maas := &toolProbingMaas{path: filepath.Join(workingDir, "inside.txt")}
		runner := newRunner()
		runner.runtimeCfg.Maas = maas
		if _, err := runner.RunTask(context.Background(), domain.Agent{}, insideTask); err != nil {
			t.Fatalf("RunTask(inside) error = %v, want nil", err)
		}
		if !strings.Contains(maas.lastPrompt, "inside-content") {
			t.Fatalf("RunTask(inside) prompt = %q, want tool success reading inside.txt", maas.lastPrompt)
		}
	})

	t.Run("`../` escape from the HTTP-inherited working_dir is rejected", func(t *testing.T) {
		maas := &toolProbingMaas{path: filepath.Join(workingDir, "..", "outside.txt")}
		runner := newRunner()
		runner.runtimeCfg.Maas = maas
		if _, err := runner.RunTask(context.Background(), domain.Agent{}, escapeTask); err != nil {
			t.Fatalf("RunTask(escape) error = %v, want nil", err)
		}
		if !strings.Contains(maas.lastPrompt, "failed: "+port.ErrPathOutsideWorkspace.Error()) {
			t.Fatalf("RunTask(escape) prompt = %q, want tool call rejected as outside workspace", maas.lastPrompt)
		}
	})
}

func TestServeCommandStartsAndStopsWithContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	root := NewRoot(app.New(), &out)
	root.SetContext(ctx)
	root.SetArgs([]string{"serve"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(serve canceled context) error = %v, want nil", err)
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("agent service stopped")) {
		t.Fatalf("Execute(serve canceled context) output = %q, want stopped message", got)
	}
}

func TestServeCommandRejectsInvalidServerAddress(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := NewRoot(app.New(), &out)
	root.SetArgs([]string{"serve", "--addr", "bad address"})
	if err := root.Execute(); err == nil {
		t.Fatalf("Execute(serve --addr bad address) error = nil, want error")
	}
}

func TestBackupAndRestoreCommandsUseSQLiteConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")
	backupPath := filepath.Join(dir, "agent.db.bak")
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}
	repo, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	if err := repo.SaveTask(ctx, storageTestTask("cli-backup-task", "before backup")); err != nil {
		t.Fatalf("SaveTask(cli-backup-task) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(source repo) error = %v, want nil", err)
	}

	var out bytes.Buffer
	if err := Execute(app.New(), &out, []string{"backup", "--config", configPath, "--out", backupPath}); err != nil {
		t.Fatalf("Execute(backup) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "backup=") || !strings.Contains(out.String(), "checksum=") {
		t.Fatalf("Execute(backup) output = %q, want backup and checksum", out.String())
	}
	repo, err = storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) after backup error = %v, want nil", dbPath, err)
	}
	if err := repo.SaveTask(ctx, storageTestTask("cli-backup-task", "after backup")); err != nil {
		t.Fatalf("SaveTask(modified cli-backup-task) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(modified repo) error = %v, want nil", err)
	}

	out.Reset()
	if err := Execute(app.New(), &out, []string{"restore", "--config", configPath, "--in", backupPath}); err != nil {
		t.Fatalf("Execute(restore) error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "restored=") || !strings.Contains(out.String(), "pre_restore=") {
		t.Fatalf("Execute(restore) output = %q, want restored and pre_restore", out.String())
	}
	repo, err = storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) after restore error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close(restored repo) error = %v, want nil", err)
		}
	})
	task, ok, err := repo.GetTask(ctx, "cli-backup-task")
	if err != nil {
		t.Fatalf("GetTask(cli-backup-task) error = %v, want nil", err)
	}
	if !ok || task.Input != "before backup" {
		t.Fatalf("GetTask(cli-backup-task) = %#v, %t, want restored task before backup", task, ok)
	}
}

func TestDataRetentionCommandUsesSQLiteConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}
	repo, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	now := time.Now()
	if err := repo.AppendQualityEvalRun(ctx, quality.EvalRunRecord{
		ID:        "eval-old",
		AgentID:   "agent-1",
		TaskID:    "task-old",
		Component: "planner",
		Status:    quality.EvalComponentDegraded,
		Score:     0.2,
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("AppendQualityEvalRun(eval-old) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(source repo) error = %v, want nil", err)
	}

	var out bytes.Buffer
	err = Execute(app.New(), &out, []string{
		"data",
		"retention",
		"--config", configPath,
		"--quality-days", "7",
		"--apply",
	})
	if err != nil {
		t.Fatalf("Execute(data retention) error = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "quality_history_deleted=1") || !strings.Contains(got, "dry_run=false") {
		t.Fatalf("Execute(data retention) output = %q, want applied quality deletion", got)
	}
	repo, err = storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) after retention error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close(retained repo) error = %v, want nil", err)
		}
	})
	records, err := repo.ListQualityEvalRuns(ctx, quality.TrendQuery{})
	if err != nil {
		t.Fatalf("ListQualityEvalRuns() error = %v, want nil", err)
	}
	if len(records) != 0 {
		t.Fatalf("ListQualityEvalRuns() len = %d, want 0", len(records))
	}
	audits, err := repo.ListAuditEvents(ctx)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v, want nil", err)
	}
	if len(audits) != 1 || audits[0].Action != "storage.retention.apply" {
		t.Fatalf("ListAuditEvents() = %#v, want retention audit", audits)
	}
}

func TestDataExportCommandWritesSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")
	exportPath := filepath.Join(dir, "agent-export.json")
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"storage": {"driver": "sqlite", "path": "`+filepath.ToSlash(dbPath)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}
	repo, err := storage.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	if err := repo.AppendRuntimeEvent(ctx, domain.RuntimeEvent{
		Type:      "task.completed",
		TaskID:    "task-1",
		Message:   "done",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AppendRuntimeEvent(task.completed) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(source repo) error = %v, want nil", err)
	}

	var out bytes.Buffer
	err = Execute(app.New(), &out, []string{
		"data",
		"export",
		"--config", configPath,
		"--out", exportPath,
	})
	if err != nil {
		t.Fatalf("Execute(data export) error = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "export=") || !strings.Contains(got, "runtime_events=1") {
		t.Fatalf("Execute(data export) output = %q, want export summary", got)
	}
	contents, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v, want nil", exportPath, err)
	}
	if !bytes.Contains(contents, []byte(`"runtime_events"`)) || !bytes.Contains(contents, []byte(`"task.completed"`)) {
		t.Fatalf("ReadFile(%q) = %s, want runtime event snapshot", exportPath, contents)
	}
}

func TestSkillSyncCommand(t *testing.T) {
	t.Parallel()
	content := `---
id: go-testing
name: Go Testing
version: 1.0.0
source: registry
risk_level: safe
status: active
tags: go,test
---
Use Go tests.
`
	sha := sha256String(content)
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[{"manifest_url":"` + baseURL + `/go-testing.json"}]}`))
		case "/go-testing.json":
			_, _ = w.Write([]byte(`{"id":"go-testing","name":"Go Testing","version":"1.0.0","content_path":"` + baseURL + `/go-testing/SKILL.md","sha256":"` + sha + `"}`))
		case "/go-testing/SKILL.md":
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)

	dir := t.TempDir()
	installRoot := filepath.Join(dir, "skills")
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"skills": {
			"registry_url": "`+server.URL+`/index.json",
			"install_root": "`+filepath.ToSlash(installRoot)+`"
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{"skill", "sync", "--config", configPath})
	if err != nil {
		t.Fatalf("Execute(skill sync) error = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "skill_sync installed=1 quarantined=0 failed=0") {
		t.Fatalf("Execute(skill sync) output = %q, want sync summary", got)
	}
	if _, err := os.Stat(filepath.Join(installRoot, "go-testing", "SKILL.md")); err != nil {
		t.Fatalf("Stat(installed SKILL.md) error = %v, want nil", err)
	}
}

func TestSkillSyncCommandUsesAgentInstallRoot(t *testing.T) {
	t.Parallel()
	content := `---
id: writer-style
name: Writer Style
version: 1.0.0
source: registry
risk_level: safe
status: active
tags: write
---
Write with structure.
`
	sha := sha256String(content)
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"skills":[{"manifest_url":"` + baseURL + `/writer-style.json"}]}`))
		case "/writer-style.json":
			_, _ = w.Write([]byte(`{"id":"writer-style","name":"Writer Style","version":"1.0.0","content_path":"` + baseURL + `/writer-style/SKILL.md","sha256":"` + sha + `"}`))
		case "/writer-style/SKILL.md":
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)

	dir := t.TempDir()
	globalRoot := filepath.Join(dir, "skills", "global")
	writerRoot := filepath.Join(dir, "skills", "writer")
	agentsDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", agentsDir, err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "writer.json"), []byte(`{
		"id": "writer",
		"role": "writer",
		"skills": {"install_root": "`+filepath.ToSlash(writerRoot)+`"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(writer.json) error = %v, want nil", err)
	}
	configPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
		"agents": {"writer": "agents/writer.json"},
		"skills": {
			"registry_url": "`+server.URL+`/index.json",
			"install_root": "`+filepath.ToSlash(globalRoot)+`"
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", configPath, err)
	}

	var out bytes.Buffer
	err := Execute(app.New(), &out, []string{"skill", "sync", "--config", configPath, "--agent", "writer"})
	if err != nil {
		t.Fatalf("Execute(skill sync --agent writer) error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(writerRoot, "writer-style", "SKILL.md")); err != nil {
		t.Fatalf("Stat(writer skill) error = %v, want installed in writer root", err)
	}
	if _, err := os.Stat(filepath.Join(globalRoot, "writer-style", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("Stat(global skill) error = %v, want not installed in global root", err)
	}
}

func storageTestTask(id string, input string) domain.Task {
	return domain.Task{
		ID:        id,
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskDone,
		Input:     input,
		CreatedAt: time.Now(),
	}
}

func sha256String(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func writeCLIFile(t *testing.T, root string, rel string, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
}

func openCLITestSQLiteRepository(t *testing.T) *storage.SQLiteRepository {
	t.Helper()
	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return repo
}

type countingConversationStore struct {
	delegate                   conversationStore
	listConversationTurnsCalls int
}

func (s *countingConversationStore) SaveAgentSession(ctx context.Context, session domain.AgentSession) error {
	return s.delegate.SaveAgentSession(ctx, session)
}

func (s *countingConversationStore) LatestAgentSession(ctx context.Context, companyID string, agentID string) (domain.AgentSession, bool, error) {
	return s.delegate.LatestAgentSession(ctx, companyID, agentID)
}

func (s *countingConversationStore) ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error) {
	return s.delegate.ListAgentSessions(ctx, companyID, agentID)
}

func (s *countingConversationStore) AppendConversationTurn(ctx context.Context, turn domain.ConversationTurn) error {
	return s.delegate.AppendConversationTurn(ctx, turn)
}

func (s *countingConversationStore) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	s.listConversationTurnsCalls++
	return s.delegate.ListConversationTurns(ctx, sessionID, limit)
}

// fakeSessionLister is a minimal SessionLister test double: it returns items
// (or err, if set) regardless of the companyID/agentID filter arguments,
// mirroring distinctSessionBases' actual usage (ListAgentSessions(ctx, "",
// "") — no filtering).
type fakeSessionLister struct {
	items []domain.AgentSession
	err   error
}

func (f fakeSessionLister) ListAgentSessions(context.Context, string, string) ([]domain.AgentSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	if slices.Contains(got, want) {
		return
	}
	t.Fatalf("bases = %v, want to contain %q", got, want)
}

// TestDistinctSessionBasesUnionsWorkspaceRootAndWorkingDirs covers the core
// M3a Task 5 contract: the base set is workspaceRoot (always present, even
// with no sessions) union SessionBase(workspaceRoot, s.WorkingDir) for every
// session, deduplicated — a session with no working_dir resolves to
// workspaceRoot itself and must not appear as a separate entry.
func TestDistinctSessionBasesUnionsWorkspaceRootAndWorkingDirs(t *testing.T) {
	workspaceRoot := t.TempDir()
	wd1 := t.TempDir()
	sessions := fakeSessionLister{items: []domain.AgentSession{
		{ID: "s1", WorkingDir: wd1},
		{ID: "s2", WorkingDir: ""}, // no working_dir -> workspaceRoot
	}}
	bases, err := distinctSessionBases(context.Background(), sessions, workspaceRoot)
	if err != nil {
		t.Fatalf("distinctSessionBases error = %v", err)
	}
	assertContains(t, bases, workspaceRoot)
	assertContains(t, bases, sessionstate.SessionBase(workspaceRoot, wd1))
	if len(bases) != 2 {
		t.Fatalf("bases = %v, want 2 distinct", bases)
	}
}

// TestDistinctSessionBasesNilListerYieldsWorkspaceRootOnly covers the
// non-persistent storage.Driver deployment: serviceStores returns a nil
// server.SessionStore in that mode, which is a valid "no session history"
// state, not an error — distinctSessionBases must still return workspaceRoot
// so restart recovery and the timeout sweep keep scanning it.
func TestDistinctSessionBasesNilListerYieldsWorkspaceRootOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	bases, err := distinctSessionBases(context.Background(), nil, workspaceRoot)
	if err != nil {
		t.Fatalf("distinctSessionBases error = %v", err)
	}
	if len(bases) != 1 || bases[0] != workspaceRoot {
		t.Fatalf("bases = %v, want exactly [%q]", bases, workspaceRoot)
	}
}

// TestDistinctSessionBasesFailsLoudOnListError covers the fail-loud contract:
// distinctSessionBases must not swallow a ListAgentSessions error and return a
// partial (silently-incomplete) base set — it must propagate the error.
func TestDistinctSessionBasesFailsLoudOnListError(t *testing.T) {
	workspaceRoot := t.TempDir()
	wantErr := errors.New("list agent sessions boom")
	_, err := distinctSessionBases(context.Background(), fakeSessionLister{err: wantErr}, workspaceRoot)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("distinctSessionBases error = %v, want wrapped %v", err, wantErr)
	}
}

type cliCaptureMaas struct {
	response string
	prompt   string
}

func (m *cliCaptureMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.prompt = testsupport.RequestText(req)
	return port.InferenceResponse{Text: m.response}, nil
}

// toolProbingMaas issues a single read_file tool call for path on its first
// Generate call, then stops (no further tool calls), capturing the prompt the
// runtime built for the following round — which renders the tool result
// (a tool turn carries the output verbatim, or "failed: <error>", see
// runtime.renderToolResult) — so a test can observe whether the call actually
// reached the file (sandbox allowed it) or was rejected by
// WorkspacePathGuard, without a real inference backend.
type toolProbingMaas struct {
	path       string
	rounds     int
	lastPrompt string
}

func (m *toolProbingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.rounds++
	m.lastPrompt = testsupport.RequestText(req)
	if m.rounds == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "probe-1",
			Name:      "read_file",
			Arguments: map[string]string{"path": m.path},
		}}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// writeProbingMaas drives exactly one write_file tool call, then stops.
type writeProbingMaas struct {
	path       string
	content    string
	rounds     int
	lastPrompt string
}

func (m *writeProbingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	m.rounds++
	m.lastPrompt = testsupport.RequestText(req)
	if m.rounds == 1 {
		return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "write-probe-1",
			Name:      "write_file",
			Arguments: map[string]string{"path": m.path, "content": m.content},
		}}}, nil
	}
	return port.InferenceResponse{Text: "done"}, nil
}

// TestDefaultTaskRunnerCanWriteFile locks that the serve default task path can
// create files: defaultTaskRunner builds NewFileReadWriteWorkspaceRegistry, so a
// write_file tool call must actually land a file inside the sandbox root. This
// is the serve half of the write capability; the per-agent half is
// TestResolverGivesWorkerWriteFile in package runtime.
func TestDefaultTaskRunnerCanWriteFile(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	maas := &writeProbingMaas{path: "created.txt", content: "hello serve"}
	runner := &defaultTaskRunner{
		runtimeCfg:  agentruntime.Config{Events: adapter.NewMemoryEventBus(), Maas: maas},
		contextRoot: workingDir,
		audit:       adapter.NewMemoryAuditLog(),
		webOptions:  tool.WebToolOptions{},
	}

	if _, err := runner.RunTask(context.Background(), domain.Agent{}, domain.Task{ID: "serve-write", WorkingDir: workingDir}); err != nil {
		t.Fatalf("RunTask(write) error = %v, want nil", err)
	}

	got, err := os.ReadFile(filepath.Join(workingDir, "created.txt"))
	if err != nil {
		t.Fatalf("ReadFile(created.txt) error = %v, want the serve task to have written it", err)
	}
	if string(got) != "hello serve" {
		t.Errorf("written content = %q, want %q", got, "hello serve")
	}
}

// TestDefaultTaskRunnerSandboxesToolsToTaskWorkingDir guards Task 7's other
// half (M3a): the default (no-agent) task path must rebuild its tool
// registry per task, rooted at task.WorkingDir when set, instead of staying
// pinned to the fixed contextRoot built once at serve assembly. It drives a
// real read_file tool call through defaultTaskRunner.RunTask (no mocked tool
// dispatch) and asserts on the WorkspacePathGuard outcome rendered into the
// next-round prompt: a path inside task.WorkingDir must succeed even though
// it is outside contextRoot, and a path inside contextRoot (but outside
// task.WorkingDir) must be rejected — proving the sandbox root really moved
// to task.WorkingDir rather than merely also allowing it.
func TestDefaultTaskRunnerSandboxesToolsToTaskWorkingDir(t *testing.T) {
	t.Parallel()

	contextRoot := t.TempDir()
	writeCLIFile(t, contextRoot, "root-only.txt", "root-content")
	workingDir := t.TempDir()
	writeCLIFile(t, workingDir, "task-only.txt", "task-content")

	newRunner := func() *defaultTaskRunner {
		return &defaultTaskRunner{
			runtimeCfg: agentruntime.Config{
				Events: adapter.NewMemoryEventBus(),
			},
			contextRoot: contextRoot,
			audit:       adapter.NewMemoryAuditLog(),
			webOptions:  tool.WebToolOptions{},
		}
	}

	t.Run("task working dir file is reachable", func(t *testing.T) {
		t.Parallel()
		maas := &toolProbingMaas{path: filepath.Join(workingDir, "task-only.txt")}
		runner := newRunner()
		runner.runtimeCfg.Maas = maas
		if _, err := runner.RunTask(context.Background(), domain.Agent{}, domain.Task{
			ID:         "task-default-wd",
			WorkingDir: workingDir,
			Input:      "read the task file",
		}); err != nil {
			t.Fatalf("RunTask(task.WorkingDir set) error = %v, want nil", err)
		}
		if !strings.Contains(maas.lastPrompt, "task-content") {
			t.Fatalf("RunTask(task.WorkingDir set) prompt = %q, want tool success reading task-only.txt", maas.lastPrompt)
		}
	})

	t.Run("context root file is unreachable once working dir is set", func(t *testing.T) {
		t.Parallel()
		maas := &toolProbingMaas{path: filepath.Join(contextRoot, "root-only.txt")}
		runner := newRunner()
		runner.runtimeCfg.Maas = maas
		if _, err := runner.RunTask(context.Background(), domain.Agent{}, domain.Task{
			ID:         "task-default-wd-escape",
			WorkingDir: workingDir,
			Input:      "try to read outside the sandbox",
		}); err != nil {
			t.Fatalf("RunTask(escape attempt) error = %v, want nil", err)
		}
		if !strings.Contains(maas.lastPrompt, "failed: "+port.ErrPathOutsideWorkspace.Error()) {
			t.Fatalf("RunTask(escape attempt) prompt = %q, want tool call rejected as outside workspace", maas.lastPrompt)
		}
	})

	t.Run("falls back to contextRoot when task has no working dir", func(t *testing.T) {
		t.Parallel()
		maas := &toolProbingMaas{path: filepath.Join(contextRoot, "root-only.txt")}
		runner := newRunner()
		runner.runtimeCfg.Maas = maas
		if _, err := runner.RunTask(context.Background(), domain.Agent{}, domain.Task{
			ID:    "task-default-no-wd",
			Input: "read the default root file",
		}); err != nil {
			t.Fatalf("RunTask(no working dir) error = %v, want nil", err)
		}
		if !strings.Contains(maas.lastPrompt, "root-content") {
			t.Fatalf("RunTask(no working dir) prompt = %q, want tool success reading root-only.txt", maas.lastPrompt)
		}
	})
}
