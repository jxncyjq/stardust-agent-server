package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesDefaultsAndJSONFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"maas": {"base_url": "https://maas.example.test", "api_key": "file-key"},
		"storage": {"driver": "sqlite", "path": "data/agent.db"},
		"server": {
			"listen_addr": ":9090",
			"admin_token": "file-token",
			"public_health_enabled": false,
			"request_id_header": "X-Correlation-ID"
		},
		"service": {"background_interval": "250ms"},
		"runtime": {"demo_response": "from config"},
		"skills": {"registry_url": "https://skills.example.test/index.json", "install_root": "data/skills"}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Maas.BaseURL != "https://maas.example.test" {
		t.Fatalf("Load(%q).Maas.BaseURL = %q, want file value", path, cfg.Maas.BaseURL)
	}
	if cfg.Maas.APIKey != "file-key" {
		t.Fatalf("Load(%q).Maas.APIKey = %q, want file value", path, cfg.Maas.APIKey)
	}
	if cfg.Storage.Driver != "sqlite" || cfg.Storage.Path != "data/agent.db" {
		t.Fatalf("Load(%q).Storage = %#v, want sqlite file storage", path, cfg.Storage)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Fatalf("Load(%q).Server.ListenAddr = %q, want :9090", path, cfg.Server.ListenAddr)
	}
	if cfg.Server.AdminToken != "file-token" {
		t.Fatalf("Load(%q).Server.AdminToken = %q, want file-token", path, cfg.Server.AdminToken)
	}
	if cfg.Server.PublicHealthEnabled {
		t.Fatalf("Load(%q).Server.PublicHealthEnabled = %t, want false", path, cfg.Server.PublicHealthEnabled)
	}
	if cfg.Server.RequestIDHeader != "X-Correlation-ID" {
		t.Fatalf("Load(%q).Server.RequestIDHeader = %q, want X-Correlation-ID", path, cfg.Server.RequestIDHeader)
	}
	if cfg.Skills.RegistryURL != "https://skills.example.test/index.json" {
		t.Fatalf("Load(%q).Skills.RegistryURL = %q, want skills registry", path, cfg.Skills.RegistryURL)
	}
	if cfg.Skills.InstallRoot != "data/skills" {
		t.Fatalf("Load(%q).Skills.InstallRoot = %q, want data/skills", path, cfg.Skills.InstallRoot)
	}
	if cfg.Service.BackgroundInterval != "250ms" {
		t.Fatalf("Load(%q).Service.BackgroundInterval = %q, want 250ms", path, cfg.Service.BackgroundInterval)
	}
	if cfg.Runtime.DemoResponse != "from config" {
		t.Fatalf("Load(%q).Runtime.DemoResponse = %q, want file value", path, cfg.Runtime.DemoResponse)
	}
	if cfg.Runtime.MaxToolRounds != 4 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want default 4", path, cfg.Runtime.MaxToolRounds)
	}
	if !cfg.TUI.ShowPrompt || !cfg.TUI.ShowThinking {
		t.Fatalf("Load(%q).TUI = %#v, want default prompt/thinking visible", path, cfg.TUI)
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("LEGION_AGENT_MAAS_URL", "https://env-maas.example.test")
	t.Setenv("LEGION_AGENT_MAAS_API_KEY", "env-key")
	t.Setenv("LEGION_AGENT_STORAGE_PATH", "env-agent.db")
	t.Setenv("LEGION_AGENT_SERVER_ADDR", ":7070")
	t.Setenv("LEGION_AGENT_ADMIN_TOKEN", "env-token")
	t.Setenv("LEGION_AGENT_PUBLIC_HEALTH", "0")
	t.Setenv("LEGION_AGENT_REQUEST_ID_HEADER", "X-Trace-ID")
	t.Setenv("LEGION_AGENT_SKILL_REGISTRY_URL", "https://env-skills.example.test/index.json")
	t.Setenv("LEGION_AGENT_SKILL_INSTALL_ROOT", "env-skills")
	t.Setenv("LEGION_AGENT_BACKGROUND_INTERVAL", "500ms")
	t.Setenv("LEGION_AGENT_MAX_TOOL_ROUNDS", "6")
	t.Setenv("LEGION_AGENT_TUI_SHOW_PROMPT", "0")
	t.Setenv("LEGION_AGENT_TUI_SHOW_THINKING", "false")

	// Isolate the default config location: this test is about the env
	// overlay, not about whatever config the developer has in ~/.stardust.
	t.Setenv("STARDUST_HOME", t.TempDir())

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(defaults with env) error = %v, want nil", err)
	}
	if cfg.Maas.BaseURL != "https://env-maas.example.test" {
		t.Fatalf("Load().Maas.BaseURL = %q, want env override", cfg.Maas.BaseURL)
	}
	if cfg.Maas.APIKey != "env-key" {
		t.Fatalf("Load().Maas.APIKey = %q, want env override", cfg.Maas.APIKey)
	}
	if cfg.Storage.Path != "env-agent.db" {
		t.Fatalf("Load().Storage.Path = %q, want env override", cfg.Storage.Path)
	}
	if cfg.Server.ListenAddr != ":7070" {
		t.Fatalf("Load().Server.ListenAddr = %q, want env override", cfg.Server.ListenAddr)
	}
	if cfg.Server.AdminToken != "env-token" {
		t.Fatalf("Load().Server.AdminToken = %q, want env-token", cfg.Server.AdminToken)
	}
	if cfg.Server.PublicHealthEnabled {
		t.Fatalf("Load().Server.PublicHealthEnabled = %t, want false", cfg.Server.PublicHealthEnabled)
	}
	if cfg.Server.RequestIDHeader != "X-Trace-ID" {
		t.Fatalf("Load().Server.RequestIDHeader = %q, want X-Trace-ID", cfg.Server.RequestIDHeader)
	}
	if cfg.Skills.RegistryURL != "https://env-skills.example.test/index.json" {
		t.Fatalf("Load().Skills.RegistryURL = %q, want env skill registry", cfg.Skills.RegistryURL)
	}
	if cfg.Skills.InstallRoot != "env-skills" {
		t.Fatalf("Load().Skills.InstallRoot = %q, want env-skills", cfg.Skills.InstallRoot)
	}
	if cfg.Service.BackgroundInterval != "500ms" {
		t.Fatalf("Load().Service.BackgroundInterval = %q, want env override", cfg.Service.BackgroundInterval)
	}
	if cfg.Runtime.MaxToolRounds != 6 {
		t.Fatalf("Load().Runtime.MaxToolRounds = %d, want env override 6", cfg.Runtime.MaxToolRounds)
	}
	if cfg.TUI.ShowPrompt {
		t.Fatalf("Load().TUI.ShowPrompt = true, want env override false")
	}
	if cfg.TUI.ShowThinking {
		t.Fatalf("Load().TUI.ShowThinking = true, want env override false")
	}
}

func TestLoadTUIConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"tui": {
			"show_prompt": false,
			"show_thinking": false
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.TUI.ShowPrompt {
		t.Fatalf("Load(%q).TUI.ShowPrompt = true, want false", path)
	}
	if cfg.TUI.ShowThinking {
		t.Fatalf("Load(%q).TUI.ShowThinking = true, want false", path)
	}
}

func TestLoadRuntimeMaxToolRoundsConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"demo_response": "from config",
			"max_tool_rounds": 8
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != 8 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want 8", path, cfg.Runtime.MaxToolRounds)
	}
}

func TestLoadSessionConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"session": {
			"enabled": true,
			"default_recent_turns": 8,
			"max_turn_chars": 3000,
			"restore_latest_on_tui_start": false,
			"cache_enabled": false,
			"cache_max_entries": 16
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Session.Enabled {
		t.Fatalf("Load(%q).Session.Enabled = false, want true", path)
	}
	if cfg.Session.DefaultRecentTurns != 8 {
		t.Fatalf("Load(%q).Session.DefaultRecentTurns = %d, want 8", path, cfg.Session.DefaultRecentTurns)
	}
	if cfg.Session.MaxTurnChars != 3000 {
		t.Fatalf("Load(%q).Session.MaxTurnChars = %d, want 3000", path, cfg.Session.MaxTurnChars)
	}
	if cfg.Session.RestoreLatestOnTUIStart {
		t.Fatalf("Load(%q).Session.RestoreLatestOnTUIStart = true, want false", path)
	}
	if cfg.Session.CacheEnabled {
		t.Fatalf("Load(%q).Session.CacheEnabled = true, want false", path)
	}
	if cfg.Session.CacheMaxEntries != 16 {
		t.Fatalf("Load(%q).Session.CacheMaxEntries = %d, want 16", path, cfg.Session.CacheMaxEntries)
	}
}

func TestDefaultSessionCacheConfig(t *testing.T) {
	// Not parallel: it points STARDUST_HOME at an empty directory, and a test
	// that reads "no config file" must not depend on whether the developer
	// running it happens to have ~/.stardust/agent.json.
	t.Setenv("STARDUST_HOME", t.TempDir())

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(default) error = %v, want nil", err)
	}
	if !cfg.Session.CacheEnabled {
		t.Fatalf("Load(default).Session.CacheEnabled = false, want true")
	}
	if cfg.Session.CacheMaxEntries != 128 {
		t.Fatalf("Load(default).Session.CacheMaxEntries = %d, want 128", cfg.Session.CacheMaxEntries)
	}
}

// TestLoadRuntimeMaxToolRoundsZeroMeansUnlimited pins the contract that an
// explicit max_tool_rounds of 0 (or negative) removes the per-task tool-round
// cap: the model may keep calling tools until it finishes the task. The value is
// normalized to UnlimitedToolRoundsCap, a large runaway hard cap that still stops
// a truly looping model. An ABSENT field keeps the safe default 4 (see
// TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault).
func TestLoadRuntimeMaxToolRoundsZeroMeansUnlimited(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"max_tool_rounds": 0
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != UnlimitedToolRoundsCap {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want unlimited cap %d", path, cfg.Runtime.MaxToolRounds, UnlimitedToolRoundsCap)
	}
}

// TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault guards that omitting the field
// entirely still yields the safe default 4 — only an explicit 0 opts into
// unlimited, so existing deployments that never set it are unaffected.
func TestLoadRuntimeMaxToolRoundsAbsentKeepsDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"demo_response": "no rounds field here"
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Runtime.MaxToolRounds != 4 {
		t.Fatalf("Load(%q).Runtime.MaxToolRounds = %d, want default 4 when field absent", path, cfg.Runtime.MaxToolRounds)
	}
}

func TestLoadContextFilesAndWorkspaceConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"context_files": {
			"enabled": true,
			"root": ".",
			"agents_path": "AGENTS.md",
			"soul_path": "configs/persona/SOUL.md",
			"tools_path": "configs/persona/TOOLS.md",
			"user_path": "configs/persona/USER.md",
			"memory_path": "configs/persona/MEMORY.md",
			"max_file_chars": 4096
		},
		"workspace": {
			"docs_root": "docs",
			"memory_root": "memory"
		},
		"tasks": {
			"index_path": "tasks.md",
			"root": "tasks",
			"archive_root": "tasks/archive",
			"max_index_lines": 400,
			"max_task_lines": 120,
			"max_message_chars": 280,
			"active_statuses": ["planned", "ready", "in_progress", "blocked", "review"],
			"done_statuses": ["done", "cancelled"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.ContextFiles.Enabled {
		t.Fatalf("Load(%q).ContextFiles.Enabled = false, want true", path)
	}
	if cfg.ContextFiles.AgentsPath != "AGENTS.md" {
		t.Fatalf("Load(%q).ContextFiles.AgentsPath = %q, want AGENTS.md", path, cfg.ContextFiles.AgentsPath)
	}
	if cfg.ContextFiles.MemoryPath != "configs/persona/MEMORY.md" {
		t.Fatalf("Load(%q).ContextFiles.MemoryPath = %q, want configs/persona/MEMORY.md", path, cfg.ContextFiles.MemoryPath)
	}
	if cfg.ContextFiles.MaxFileChars != 4096 {
		t.Fatalf("Load(%q).ContextFiles.MaxFileChars = %d, want 4096", path, cfg.ContextFiles.MaxFileChars)
	}
	if cfg.Workspace.DocsRoot != "docs" || cfg.Workspace.MemoryRoot != "memory" {
		t.Fatalf("Load(%q).Workspace = %#v, want docs/memory roots", path, cfg.Workspace)
	}
	if cfg.Tasks.IndexPath != "tasks.md" || cfg.Tasks.Root != "tasks" || cfg.Tasks.ArchiveRoot != "tasks/archive" {
		t.Fatalf("Load(%q).Tasks paths = %#v, want tasks.md/tasks/tasks/archive", path, cfg.Tasks)
	}
	if cfg.Tasks.MaxIndexLines != 400 || cfg.Tasks.MaxTaskLines != 120 || cfg.Tasks.MaxMessageChars != 280 {
		t.Fatalf("Load(%q).Tasks limits = %#v, want configured limits", path, cfg.Tasks)
	}
	if len(cfg.Tasks.ActiveStatuses) != 5 || cfg.Tasks.ActiveStatuses[3] != "blocked" {
		t.Fatalf("Load(%q).Tasks.ActiveStatuses = %#v, want configured active statuses", path, cfg.Tasks.ActiveStatuses)
	}
	if len(cfg.Tasks.DoneStatuses) != 2 || cfg.Tasks.DoneStatuses[1] != "cancelled" {
		t.Fatalf("Load(%q).Tasks.DoneStatuses = %#v, want configured done statuses", path, cfg.Tasks.DoneStatuses)
	}
}

func TestLoadTasksEnvOverrides(t *testing.T) {
	t.Setenv("LEGION_AGENT_TASKS_INDEX_PATH", "work/tasks.md")
	t.Setenv("LEGION_AGENT_TASKS_ROOT", "work/tasks")
	t.Setenv("LEGION_AGENT_TASKS_ARCHIVE_ROOT", "work/tasks/archive")
	t.Setenv("LEGION_AGENT_TASKS_MAX_INDEX_LINES", "250")
	t.Setenv("LEGION_AGENT_TASKS_MAX_TASK_LINES", "80")
	t.Setenv("LEGION_AGENT_TASKS_MAX_MESSAGE_CHARS", "180")

	// Isolate the default config location: this test is about the env
	// overlay, not about whatever config the developer has in ~/.stardust.
	t.Setenv("STARDUST_HOME", t.TempDir())

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load(default with task env) error = %v, want nil", err)
	}
	if cfg.Tasks.IndexPath != "work/tasks.md" {
		t.Fatalf("Load().Tasks.IndexPath = %q, want work/tasks.md", cfg.Tasks.IndexPath)
	}
	if cfg.Tasks.Root != "work/tasks" || cfg.Tasks.ArchiveRoot != "work/tasks/archive" {
		t.Fatalf("Load().Tasks roots = %#v, want env roots", cfg.Tasks)
	}
	if cfg.Tasks.MaxIndexLines != 250 || cfg.Tasks.MaxTaskLines != 80 || cfg.Tasks.MaxMessageChars != 180 {
		t.Fatalf("Load().Tasks limits = %#v, want env limits", cfg.Tasks)
	}
}

func TestLoadMaasProfiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"maas":{"default_profile":"fast","profiles":{"fast":{"base_url":"https://fast.example.test","api_key":"fast-key"},"review":{"base_url":"https://review.example.test","api_key":"review-key"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}
	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Maas.DefaultProfile != "fast" {
		t.Fatalf("Load(%q).Maas.DefaultProfile = %q, want fast", path, cfg.Maas.DefaultProfile)
	}
	if cfg.Maas.Profiles["review"].BaseURL != "https://review.example.test" {
		t.Fatalf("Load(%q).Maas.Profiles[review].BaseURL = %q, want review URL", path, cfg.Maas.Profiles["review"].BaseURL)
	}
}

func TestLoadAgentsConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"agents":{"researcher":"agents/researcher.json","writer":"agents/writer.json"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Agents["researcher"] != "agents/researcher.json" {
		t.Fatalf("Load(%q).Agents[researcher] = %q, want agents/researcher.json", path, cfg.Agents["researcher"])
	}
	if cfg.Agents["writer"] != "agents/writer.json" {
		t.Fatalf("Load(%q).Agents[writer] = %q, want agents/writer.json", path, cfg.Agents["writer"])
	}
}

func TestLoadMissingFileReturnsErrConfigNotFound(t *testing.T) {
	t.Parallel()
	_, err := Load(context.Background(), Options{Path: filepath.Join(t.TempDir(), "missing.json")})
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Load(missing) error = %v, want ErrConfigNotFound", err)
	}
}

func TestLoadMaxConcurrentTasksDefault(t *testing.T) {
	cfg, err := Load(context.Background(), Options{Path: ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runtime.MaxConcurrentTasks != 4 {
		t.Fatalf("default MaxConcurrentTasks = %d, want 4", cfg.Runtime.MaxConcurrentTasks)
	}
}

func TestLoadApprovalTimeoutSecondsDefault(t *testing.T) {
	cfg, err := Load(context.Background(), Options{Path: ""})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runtime.ApprovalTimeoutSeconds != 300 {
		t.Fatalf("default ApprovalTimeoutSeconds = %d, want 300", cfg.Runtime.ApprovalTimeoutSeconds)
	}
}

// TestNormalizeMaxToolRounds pins the config-layer normalization invariant
// directly, so the "0/negative = unlimited cap" contract is a locked regression
// line rather than only inferred through Load. This function carries a safety
// role — the cap is the only thing stopping a runaway in-flight tool loop, since
// hard-loop detection cannot break one — so changing it must break a test.
func TestNormalizeMaxToolRounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"explicit zero maps to the unlimited cap", 0, UnlimitedToolRoundsCap},
		{"negative maps to the unlimited cap", -1, UnlimitedToolRoundsCap},
		{"positive is used as-is", 5, 5},
		{"one is used as-is", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeMaxToolRounds(tc.in); got != tc.want {
				t.Errorf("normalizeMaxToolRounds(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestLoadDisabledToolsUnknownNameFailsLoud validates that an unknown tool name
// in disabled_tools causes Load to return an error with the bad name included.
// This is a fail-loud invariant: typos in the config must be caught eagerly.
func TestLoadDisabledToolsUnknownNameFailsLoud(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"disabled_tools": ["writ_file"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	_, err := Load(context.Background(), Options{Path: path})
	if err == nil {
		t.Fatal("Load with unknown disabled_tools name should return error, got nil")
	}
	if !strings.Contains(err.Error(), "writ_file") {
		t.Fatalf("Load error %q should mention unknown tool name 'writ_file'", err.Error())
	}
}

// TestLoadDisabledToolsValidNameSucceeds validates that a valid tool name in
// disabled_tools is accepted and the config loads successfully.
func TestLoadDisabledToolsValidNameSucceeds(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"runtime": {
			"disabled_tools": ["write_file"]
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load with valid disabled_tools name should succeed, got error: %v", err)
	}
	if len(cfg.Runtime.DisabledTools) != 1 || cfg.Runtime.DisabledTools[0] != "write_file" {
		t.Fatalf("Load().Runtime.DisabledTools = %v, want [write_file]", cfg.Runtime.DisabledTools)
	}
}

// TestRuntimeConfigCompactThresholdParses validates that
// runtime.compact_token_threshold decodes into RuntimeConfig.CompactTokenThreshold.
func TestRuntimeConfigCompactThresholdParses(t *testing.T) {
	var rc RuntimeConfig
	if err := json.Unmarshal([]byte(`{"compact_token_threshold":60000}`), &rc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rc.CompactTokenThreshold != 60000 {
		t.Fatalf("CompactTokenThreshold=%d want 60000", rc.CompactTokenThreshold)
	}
}

func TestMaasProfileContextLength(t *testing.T) {
	raw := `{"maas":{"profiles":{"dev":{"model":"deepseek-v4-flash","context_length":128000}}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.Maas.Profiles["dev"].ContextLength; got != 128000 {
		t.Fatalf("ContextLength = %d, want 128000", got)
	}
}

// TestLoadServerFileBaseURLConfig validates that a configured
// server.file_base_url loads and that a trailing slash is trimmed, so
// callers can safely concatenate "/v1/files?..." onto it without a double
// slash.
func TestLoadServerFileBaseURLConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{
		"server": {
			"file_base_url": "https://agent.example.com/"
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Server.FileBaseURL != "https://agent.example.com" {
		t.Fatalf("Load(%q).Server.FileBaseURL = %q, want trailing slash trimmed", path, cfg.Server.FileBaseURL)
	}
}

// TestLoadServerFileBaseURLAbsentDefaultsToEmpty pins the contract that an
// absent server.file_base_url is a legitimate optional default (empty
// string), not an error — generated-file links then stay relative paths.
func TestLoadServerFileBaseURLAbsentDefaultsToEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := Load(context.Background(), Options{Path: ""})
	if err != nil {
		t.Fatalf("Load(default) error = %v, want nil", err)
	}
	if cfg.Server.FileBaseURL != "" {
		t.Fatalf("Load(default).Server.FileBaseURL = %q, want empty default", cfg.Server.FileBaseURL)
	}
}

// TestLoadServerFileBaseURLInvalidFailsLoud validates that an invalid
// server.file_base_url (unparseable or non-http(s) scheme) causes Load to
// fail loud with an error mentioning file_base_url, rather than silently
// keeping a broken value.
func TestLoadServerFileBaseURLInvalidFailsLoud(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
	}{
		{"not a url", "not a url"},
		{"unsupported scheme", "ftp://x"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"server": {"file_base_url": "` + tc.value + `"}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
			}

			_, err := Load(context.Background(), Options{Path: path})
			if err == nil {
				t.Fatalf("Load(%q) with file_base_url %q should return error, got nil", path, tc.value)
			}
			if !strings.Contains(err.Error(), "file_base_url") {
				t.Fatalf("Load error %q should mention file_base_url", err.Error())
			}
		})
	}
}

func TestDefaultConfigWebSearchFields(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Web.SearchDefaultLimit != 5 {
		t.Errorf("SearchDefaultLimit = %d, want 5", cfg.Web.SearchDefaultLimit)
	}
	if cfg.Web.SearchTimeoutSeconds != 15 {
		t.Errorf("SearchTimeoutSeconds = %d, want 15", cfg.Web.SearchTimeoutSeconds)
	}
}

func TestBrowserSnapshotDefaults(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Browser.SnapshotRuneThreshold != 15000 {
		t.Fatalf("SnapshotRuneThreshold = %d, want 15000", cfg.Browser.SnapshotRuneThreshold)
	}
	if cfg.Browser.MaxElements != 100 {
		t.Fatalf("MaxElements = %d, want 100", cfg.Browser.MaxElements)
	}
	if cfg.Browser.SnapshotTTLHours != 24 {
		t.Fatalf("SnapshotTTLHours = %d, want 24", cfg.Browser.SnapshotTTLHours)
	}
}

// TestLoadPluginsSignaturePolicyDefaultsToRequired pins the safe side of the
// signature switch: a plugins section that says nothing about signatures
// REQUIRES them. A security control whose absent setting meant "off" would be
// off in every deployment that never heard of it.
func TestLoadPluginsSignaturePolicyDefaultsToRequired(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"keyring": "keyring.json",
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Plugins.SignatureRequired() {
		t.Errorf("Load(%q).Plugins.SignatureRequired() = false, want true: an absent require_signature must mean strict", path)
	}
	if cfg.Plugins.RequireSignature == nil {
		t.Fatalf("Load(%q).Plugins.RequireSignature = nil, want a normalized pointer: Load must state the policy explicitly", path)
	}
	if !*cfg.Plugins.RequireSignature {
		t.Errorf("Load(%q).Plugins.RequireSignature = %t, want true", path, *cfg.Plugins.RequireSignature)
	}
	if cfg.Plugins.Keyring != "keyring.json" {
		t.Errorf("Load(%q).Plugins.Keyring = %q, want keyring.json", path, cfg.Plugins.Keyring)
	}
}

// TestLoadPluginsSignatureCanBeTurnedOffExplicitly is the other half of the
// same rule: "off" is reachable, but only by writing it down.
func TestLoadPluginsSignatureCanBeTurnedOffExplicitly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"require_signature": false,
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Plugins.SignatureRequired() {
		t.Errorf("Load(%q).Plugins.SignatureRequired() = true, want false: an explicit require_signature:false must be honored", path)
	}
	if cfg.Plugins.RequireSignature == nil || *cfg.Plugins.RequireSignature {
		t.Errorf("Load(%q).Plugins.RequireSignature = %v, want a pointer to false", path, cfg.Plugins.RequireSignature)
	}
}

// TestPluginsConfigZeroValueRequiresSignature covers the path Load does not
// travel: a PluginsConfig built as a struct literal (an embedder's, a test's)
// has a nil RequireSignature, and nil must read as strict there too. If the
// zero value read as "off", every config assembled without Load would silently
// stop verifying signatures.
func TestPluginsConfigZeroValueRequiresSignature(t *testing.T) {
	t.Parallel()
	if !(PluginsConfig{}).SignatureRequired() {
		t.Error("PluginsConfig{}.SignatureRequired() = false, want true: an unstated policy is the strict one")
	}
}

// TestLoadPluginsInsecureSourcesDefaultToRefused is the remote-source half of
// the same rule the signature switch holds: a plugins section that says
// nothing about plaintext sources REFUSES them. allow_insecure_sources is a
// debugging aid, and a debugging aid whose absent setting meant "on" would be
// on in every deployment written before it existed.
func TestLoadPluginsInsecureSourcesDefaultToRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"require_signature": false,
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Plugins.InsecureSourcesAllowed() {
		t.Errorf("Load(%q).Plugins.InsecureSourcesAllowed() = true, want false: an absent switch must be the safe side", path)
	}
	if cfg.Plugins.AllowInsecureSources == nil {
		t.Fatalf("Load(%q).Plugins.AllowInsecureSources = nil, want a normalized pointer: Load must state the policy explicitly", path)
	}
	if *cfg.Plugins.AllowInsecureSources {
		t.Errorf("Load(%q).Plugins.AllowInsecureSources = %t, want false", path, *cfg.Plugins.AllowInsecureSources)
	}
	if cfg.Plugins.Cache != "" {
		t.Errorf("Load(%q).Plugins.Cache = %q, want empty: where downloaded plugin code is written is a deployment "+
			"decision, so there is no default location", path, cfg.Plugins.Cache)
	}
}

// TestLoadPluginsInsecureSourcesCanBeAllowedExplicitly is the other half: the
// switch is reachable, but only by writing it down.
func TestLoadPluginsInsecureSourcesCanBeAllowedExplicitly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"cache": "var/plugin-cache",
			"require_signature": false,
			"allow_insecure_sources": true,
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if !cfg.Plugins.InsecureSourcesAllowed() {
		t.Errorf("Load(%q).Plugins.InsecureSourcesAllowed() = false, want true: an explicit true must be honored", path)
	}
	if cfg.Plugins.Cache != "var/plugin-cache" {
		t.Errorf("Load(%q).Plugins.Cache = %q, want var/plugin-cache", path, cfg.Plugins.Cache)
	}
}

// TestPluginsConfigZeroValueRefusesInsecureSources covers the path Load does
// not travel: a PluginsConfig built as a struct literal has a nil pointer, and
// nil must read as "refused" there too.
func TestPluginsConfigZeroValueRefusesInsecureSources(t *testing.T) {
	t.Parallel()
	if (PluginsConfig{}).InsecureSourcesAllowed() {
		t.Error("PluginsConfig{}.InsecureSourcesAllowed() = true, want false: an unstated policy is the safe one")
	}
}

// TestLoadPluginsFetchBoundsDefaultToFiniteValues pins that the download
// bounds are defaulted, and defaulted to FINITE values. A deployment that
// never wrote a fetch section still downloads under a timeout and a byte cap.
func TestLoadPluginsFetchBoundsDefaultToFiniteValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"require_signature": false,
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Plugins.Fetch.TimeoutMs != 30000 {
		t.Errorf("Load(%q).Plugins.Fetch.TimeoutMs = %d, want 30000", path, cfg.Plugins.Fetch.TimeoutMs)
	}
	if cfg.Plugins.Fetch.MaxBytes != 33554432 {
		t.Errorf("Load(%q).Plugins.Fetch.MaxBytes = %d, want 33554432", path, cfg.Plugins.Fetch.MaxBytes)
	}
}

// TestLoadRejectsUnboundedPluginFetchBounds is the fail-loud half: zero does
// not mean unlimited here. A deployment that writes one down as 0 is told so
// by name rather than left with a download nothing bounds.
func TestLoadRejectsUnboundedPluginFetchBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		fetch string
		want  string
	}{
		{"no timeout", `{"timeout_ms": 0, "max_bytes": 1024}`, "plugins.fetch.timeout_ms"},
		{"negative timeout", `{"timeout_ms": -1, "max_bytes": 1024}`, "plugins.fetch.timeout_ms"},
		{"no byte cap", `{"timeout_ms": 1000, "max_bytes": 0}`, "plugins.fetch.max_bytes"},
		{"negative byte cap", `{"timeout_ms": 1000, "max_bytes": -1}`, "plugins.fetch.max_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "agent.json")
			body := `{"plugins": {"manifest": "plugins.json", "root": "plugins", "require_signature": false, ` +
				`"limits": {"timeout_ms": 1000}, "apply_wait_ms": 1000, "fetch": ` + tc.fetch + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
			}
			_, err := Load(context.Background(), Options{Path: path})
			if err == nil {
				t.Fatalf("Load(%q) error = nil, want an error naming %s", path, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load(%q) error = %v, want it to name %s", path, err, tc.want)
			}
		})
	}
}

// TestLoadPluginsHealthDefaultsToFive pins the default tolerance for a
// misbehaving plugin. A deployment that says nothing about health still gets a
// policy — an unstated one that meant "never unload" would leave every
// deployment that never heard of the setting running plugins that trap on
// every call.
func TestLoadPluginsHealthDefaultsToFive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}
	if cfg.Plugins.Health.MaxConsecutiveFaults != 5 {
		t.Errorf("Load(%q).Plugins.Health.MaxConsecutiveFaults = %d, want the default 5",
			path, cfg.Plugins.Health.MaxConsecutiveFaults)
	}
}

// TestLoadRejectsANonPositiveHealthThreshold is the other half: zero is not
// "unlimited", it is an unstated policy, and this project refuses those rather
// than picking the permissive reading on the operator's behalf.
func TestLoadRejectsANonPositiveHealthThreshold(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{
		"plugins": {
			"manifest": "plugins.json",
			"root": "plugins",
			"limits": {"timeout_ms": 1000},
			"apply_wait_ms": 1000,
			"health": {"max_consecutive_faults": 0}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	if _, err := Load(context.Background(), Options{Path: path}); err == nil {
		t.Fatal("Load with max_consecutive_faults=0 error = nil, want a refusal: " +
			"0 is not 'never unload', it is an unstated policy")
	}
}

// The tests below pin the default configuration location: ~/.stardust/agent.json
// (or $STARDUST_HOME/agent.json). Before it existed, a command run without
// --config read NO file at all — it ran on built-in defaults, which is the
// state an operator least expects when they have a config sitting in the
// conventional place.
//
// They set STARDUST_HOME rather than HOME: reading the developer's real
// ~/.stardust would make the suite depend on the machine it runs on.

func TestLoadReadsTheDefaultConfigWhenNoPathIsGiven(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STARDUST_HOME", dir)
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"maas":{"base_url":"https://from-default-dir.example"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", path, err)
	}

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load with no path error = %v, want nil", err)
	}
	if cfg.Maas.BaseURL != "https://from-default-dir.example" {
		t.Errorf("Maas.BaseURL = %q, want the value from %s", cfg.Maas.BaseURL, path)
	}
}

func TestLoadWithNoDefaultConfigStillRunsOnBuiltInDefaults(t *testing.T) {
	// An installation before its first config file is a supported state, not an
	// error: this is what every fresh machine looks like.
	t.Setenv("STARDUST_HOME", t.TempDir())

	cfg, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Load with no default config error = %v, want nil: running without a config file is supported", err)
	}
	if cfg.Storage.Driver != defaultConfig().Storage.Driver {
		t.Errorf("Storage.Driver = %q, want the built-in default %q",
			cfg.Storage.Driver, defaultConfig().Storage.Driver)
	}
}

func TestLoadPrefersAnExplicitPathOverTheDefault(t *testing.T) {
	defaultDir := t.TempDir()
	t.Setenv("STARDUST_HOME", defaultDir)
	if err := os.WriteFile(filepath.Join(defaultDir, "agent.json"),
		[]byte(`{"maas":{"base_url":"https://default.example"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	explicit := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(explicit,
		[]byte(`{"maas":{"base_url":"https://explicit.example"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	cfg, err := Load(context.Background(), Options{Path: explicit})
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", explicit, err)
	}
	if cfg.Maas.BaseURL != "https://explicit.example" {
		t.Errorf("Maas.BaseURL = %q, want the explicitly named file to win", cfg.Maas.BaseURL)
	}
}

func TestLoadRefusesABrokenDefaultConfigInsteadOfIgnoringIt(t *testing.T) {
	// The one thing the default location must NOT do: fall back to built-in
	// defaults when the file it found is broken. That would run a deployment on
	// settings nobody wrote, with the operator's real config sitting unread.
	dir := t.TempDir()
	t.Setenv("STARDUST_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}

	if _, err := Load(context.Background(), Options{}); err == nil {
		t.Fatal("Load with an undecodable default config error = nil, want a refusal")
	}
}

func TestDefaultConfigPathHonoursTheHomeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STARDUST_HOME", dir)

	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error = %v, want nil", err)
	}
	if want := filepath.Join(dir, "agent.json"); path != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", path, want)
	}
}

func TestDefaultConfigPathIfPresentIsEmptyWhenNothingIsThere(t *testing.T) {
	t.Setenv("STARDUST_HOME", t.TempDir())

	if got := DefaultConfigPathIfPresent(); got != "" {
		t.Errorf("DefaultConfigPathIfPresent() = %q, want \"\" when no file exists", got)
	}
}

func TestDefaultConfigPathIfPresentIgnoresADirectoryOfThatName(t *testing.T) {
	// A directory called agent.json is not a config file, and reporting it as
	// one would turn a confusing mistake into an unreadable error later.
	dir := t.TempDir()
	t.Setenv("STARDUST_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "agent.json"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v, want nil", err)
	}

	if got := DefaultConfigPathIfPresent(); got != "" {
		t.Errorf("DefaultConfigPathIfPresent() = %q, want \"\" for a directory", got)
	}
}
