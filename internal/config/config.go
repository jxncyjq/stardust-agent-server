package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

var ErrConfigNotFound = errors.New("config file not found")

type Options struct {
	Path string
}

type Config struct {
	Maas         MaasConfig         `json:"maas"`
	Agents       map[string]string  `json:"agents"`
	Storage      StorageConfig      `json:"storage"`
	Server       ServerConfig       `json:"server"`
	Service      ServiceConfig      `json:"service"`
	Runtime      RuntimeConfig      `json:"runtime"`
	TUI          TUIConfig          `json:"tui"`
	Session      SessionConfig      `json:"session"`
	Skills       SkillsConfig       `json:"skills"`
	ContextFiles ContextFilesConfig `json:"context_files"`
	Workspace    WorkspaceConfig    `json:"workspace"`
	Tasks        TasksConfig        `json:"tasks"`
	Web          WebToolConfig      `json:"web"`
	Evolution    EvolutionConfig    `json:"evolution"`
}

// EvolutionConfig tunes the periodic degradation-detection job
// (EvolutionEvaluator). Zero values fall back to safe defaults: a 0.2 quality
// drop over a 14-day window, scanned every 60 minutes.
type EvolutionConfig struct {
	DegradationThreshold   float64 `json:"degradation_threshold"`
	DegradationWindowDays  int     `json:"degradation_window_days"`
	DegradationScanMinutes int     `json:"degradation_scan_minutes"`
}

// WebToolConfig configures the fetch_url web tool. SSRF protection is on by
// default: AllowPrivateHosts must be set true to permit loopback/private IPs.
type WebToolConfig struct {
	Enabled           bool     `json:"enabled"`
	AllowPrivateHosts bool     `json:"allow_private_hosts"`
	TimeoutSeconds    int      `json:"timeout_seconds"`
	MaxResponseKB     int      `json:"max_response_kb"`
	Allowlist         []string `json:"allowlist"`
}

type MaasConfig struct {
	BaseURL        string                 `json:"base_url"`
	APIKey         string                 `json:"api_key"`
	DefaultProfile string                 `json:"default_profile"`
	Profiles       map[string]MaasProfile `json:"profiles"`
}

type MaasProfile struct {
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	// PromptCache opts this profile into provider prompt caching: the adapter
	// marks the stable task-framing prefix with a cache_control breakpoint.
	// Optional; defaults to false (byte-for-byte identical requests), so only
	// enable it for providers that honor Anthropic-style cache_control.
	PromptCache bool `json:"prompt_cache,omitempty"`
}

type StorageConfig struct {
	Driver string `json:"driver"`
	Path   string `json:"path"`
}

type ServerConfig struct {
	ListenAddr          string `json:"listen_addr"`
	AdminToken          string `json:"admin_token"`
	PublicHealthEnabled bool   `json:"public_health_enabled"`
	// RequireIdentity makes the X-Role and X-Company-ID headers mandatory for
	// the HTTP server's RBAC and tenant checks. Defaults to false: the
	// single-machine contract where a header-less request is treated as an
	// admin of every company. Set it to true for multi-tenant or
	// network-exposed deployments (env: LEGION_AGENT_REQUIRE_IDENTITY, which
	// accepts only strconv.ParseBool values and fails Load otherwise).
	// It covers the policy-guarded endpoints only, not the untenanted list
	// endpoints; see server.Config.RequireIdentity for the exact scope.
	RequireIdentity bool   `json:"require_identity"`
	RequestIDHeader string `json:"request_id_header"`
}

type ServiceConfig struct {
	BackgroundInterval string `json:"background_interval"`
}

type RuntimeConfig struct {
	DemoResponse  string `json:"demo_response"`
	MaxToolRounds int    `json:"max_tool_rounds"`
	// LazyTools enables the on-demand (lazy) tool protocol: the model is offered
	// two small meta tools (list_tools/call_tool) instead of the full native tool
	// schema on every inference, so simple chats that need no tools avoid paying
	// the ~1800-token schema overhead. Defaults to true; set false to fall back
	// to offering the complete native tool schema every round (safety rollback).
	LazyTools bool `json:"lazy_tools"`
	// MaxConcurrentTasks caps how many tasks the coordinator runs simultaneously,
	// each on its own goroutine. Defaults to 4; 0 or negative means the default.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	// ApprovalTimeoutSeconds bounds how long a Manual-mode tool-approval ticket
	// may sit ApprovalPending before the background timeout sweep auto-denies it
	// (a contract outcome — a reject result to the model — not a silent drop).
	// Defaults to 300 (5 minutes) when 0 or negative.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds"`
}

type SessionConfig struct {
	Enabled                 bool `json:"enabled"`
	DefaultRecentTurns      int  `json:"default_recent_turns"`
	MaxTurnChars            int  `json:"max_turn_chars"`
	RestoreLatestOnTUIStart bool `json:"restore_latest_on_tui_start"`
	CacheEnabled            bool `json:"cache_enabled"`
	CacheMaxEntries         int  `json:"cache_max_entries"`
}

type ThemeConfig struct {
	Accent   string `json:"accent"`    // titles, active items, progress bar
	Accent2  string `json:"accent2"`   // panel borders, secondary highlights
	Text     string `json:"text"`      // normal body text, subdued output
	Dim      string `json:"dim"`       // help text, footer hints
	Error    string `json:"error"`     // error messages
	StatusFg string `json:"status_fg"` // status bar foreground (unused, reserved)
	StatusBg string `json:"status_bg"` // status bar background (unused, reserved)
	ShellBg  string `json:"shell_bg"`  // main shell/output area background
}

type TUIConfig struct {
	ShowPrompt   bool        `json:"show_prompt"`
	ShowThinking bool        `json:"show_thinking"`
	ColorProfile string      `json:"color_profile"`
	Theme        ThemeConfig `json:"theme"`
}

type SkillsConfig struct {
	RegistryURL string `json:"registry_url"`
	InstallRoot string `json:"install_root"`
}

// ContextFilesConfig holds configuration for resident context files loaded into
// every inference context. The three AGENTS.md locations (global
// ~/.stardust/agents.md, workspace agents.md, workspace .stardust/agents.md)
// are always derived from Root and the user home directory — they are not
// configurable here. AgentsPath and ConfigRoot are retained for JSON
// compatibility with existing agent.json files but are no longer used by the
// loader.
type ContextFilesConfig struct {
	Enabled      bool   `json:"enabled"`
	Root         string `json:"root"`
	AgentsPath   string `json:"agents_path"` // deprecated: no longer used for resident loading
	ConfigRoot   string `json:"config_root"` // deprecated: no longer used
	SoulPath     string `json:"soul_path"`
	ToolsPath    string `json:"tools_path"`
	UserPath     string `json:"user_path"`
	MemoryPath   string `json:"memory_path"`
	MaxFileChars int    `json:"max_file_chars"`
}

type WorkspaceConfig struct {
	// Root is the base directory for per-session state and workspace-relative
	// docs/memory. "~" is expanded; an unset/invalid value falls back to
	// <home>/.stardust (see sessionstate.ResolveWorkspaceRoot). DocsRoot and
	// MemoryRoot are resolved relative to it.
	Root       string `json:"root"`
	DocsRoot   string `json:"docs_root"`
	MemoryRoot string `json:"memory_root"`
}

type TasksConfig struct {
	IndexPath       string   `json:"index_path"`
	Root            string   `json:"root"`
	ArchiveRoot     string   `json:"archive_root"`
	MaxIndexLines   int      `json:"max_index_lines"`
	MaxTaskLines    int      `json:"max_task_lines"`
	MaxMessageChars int      `json:"max_message_chars"`
	ActiveStatuses  []string `json:"active_statuses"`
	DoneStatuses    []string `json:"done_statuses"`
}

func Load(ctx context.Context, opts Options) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	if opts.Path != "" {
		data, err := os.ReadFile(opts.Path)
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%w: %s", ErrConfigNotFound, opts.Path)
		}
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", opts.Path, err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode config %q: %w", opts.Path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	cfg.Runtime.MaxToolRounds = normalizeMaxToolRounds(cfg.Runtime.MaxToolRounds)
	return cfg, nil
}

func defaultConfig() Config {
	return Config{
		Agents: map[string]string{},
		Maas: MaasConfig{
			Profiles: map[string]MaasProfile{},
		},
		Storage: StorageConfig{
			Driver: "memory",
			Path:   "agent.db",
		},
		Server: ServerConfig{
			ListenAddr:          ":8080",
			PublicHealthEnabled: true,
			RequireIdentity:     false,
			RequestIDHeader:     "X-Request-ID",
		},
		Service: ServiceConfig{
			BackgroundInterval: "1s",
		},
		Runtime: RuntimeConfig{
			DemoResponse:           "task completed",
			MaxToolRounds:          4,
			LazyTools:              true,
			MaxConcurrentTasks:     4,
			ApprovalTimeoutSeconds: 300,
		},
		Session: SessionConfig{
			Enabled:                 true,
			DefaultRecentTurns:      6,
			MaxTurnChars:            6000,
			RestoreLatestOnTUIStart: true,
			CacheEnabled:            true,
			CacheMaxEntries:         128,
		},
		TUI: TUIConfig{
			ShowPrompt:   true,
			ShowThinking: true,
			ColorProfile: "truecolor",
			Theme: ThemeConfig{
				Accent:   "39",
				Accent2:  "33",
				Text:     "250",
				Dim:      "245",
				Error:    "196",
				StatusFg: "230",
				StatusBg: "236",
				ShellBg:  "17",
			},
		},
		Skills: SkillsConfig{
			InstallRoot: "skills",
		},
		ContextFiles: ContextFilesConfig{
			Enabled:      true,
			Root:         ".",
			SoulPath:     "configs/persona/SOUL.md",
			ToolsPath:    "configs/persona/TOOLS.md",
			UserPath:     "configs/persona/USER.md",
			MemoryPath:   "configs/persona/MEMORY.md",
			MaxFileChars: 20000,
		},
		Workspace: WorkspaceConfig{
			Root:       "~/.stardust",
			DocsRoot:   "docs",
			MemoryRoot: "memory",
		},
		Tasks: TasksConfig{
			IndexPath:       "tasks.md",
			Root:            "tasks",
			ArchiveRoot:     "tasks/archive",
			MaxIndexLines:   500,
			MaxTaskLines:    300,
			MaxMessageChars: 300,
			ActiveStatuses:  []string{"planned", "ready", "in_progress", "blocked", "review"},
			DoneStatuses:    []string{"done", "cancelled"},
		},
		Web: WebToolConfig{
			Enabled:           true,
			AllowPrivateHosts: false,
			TimeoutSeconds:    20,
			MaxResponseKB:     512,
			Allowlist:         []string{},
		},
		Evolution: EvolutionConfig{
			DegradationThreshold:   0.2,
			DegradationWindowDays:  14,
			DegradationScanMinutes: 60,
		},
	}
}

// applyEnv overlays environment variables onto cfg. It returns an error only
// for security-relevant keys whose misspelling must not degrade silently into
// the permissive default; the convenience toggles keep their historical
// "true"/"1"-or-false parsing.
func applyEnv(cfg *Config) error {
	if value := os.Getenv("LEGION_AGENT_MAAS_URL"); value != "" {
		cfg.Maas.BaseURL = value
	}
	if value := os.Getenv("LEGION_AGENT_MAAS_API_KEY"); value != "" {
		cfg.Maas.APIKey = value
	}
	if value := os.Getenv("LEGION_AGENT_STORAGE_DRIVER"); value != "" {
		cfg.Storage.Driver = value
	}
	if value := os.Getenv("LEGION_AGENT_STORAGE_PATH"); value != "" {
		cfg.Storage.Path = value
	}
	if value := os.Getenv("LEGION_AGENT_SERVER_ADDR"); value != "" {
		cfg.Server.ListenAddr = value
	}
	if value := os.Getenv("LEGION_AGENT_ADMIN_TOKEN"); value != "" {
		cfg.Server.AdminToken = value
	}
	if value := os.Getenv("LEGION_AGENT_PUBLIC_HEALTH"); value != "" {
		cfg.Server.PublicHealthEnabled = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_REQUIRE_IDENTITY"); value != "" {
		// Unlike the convenience toggles above, an unparseable value here must
		// not fall back to false: an operator who writes REQUIRE_IDENTITY=yes
		// intends to harden the server and would otherwise get zero hardening
		// with zero warning.
		required, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse LEGION_AGENT_REQUIRE_IDENTITY %q: %w", value, err)
		}
		cfg.Server.RequireIdentity = required
	}
	if value := os.Getenv("LEGION_AGENT_REQUEST_ID_HEADER"); value != "" {
		cfg.Server.RequestIDHeader = value
	}
	if value := os.Getenv("LEGION_AGENT_BACKGROUND_INTERVAL"); value != "" {
		cfg.Service.BackgroundInterval = value
	}
	if value := os.Getenv("LEGION_AGENT_DEMO_RESPONSE"); value != "" {
		cfg.Runtime.DemoResponse = value
	}
	if value := os.Getenv("LEGION_AGENT_MAX_TOOL_ROUNDS"); value != "" {
		if rounds, err := strconv.Atoi(value); err == nil {
			cfg.Runtime.MaxToolRounds = rounds
		}
	}
	if value := os.Getenv("LEGION_AGENT_SESSION_ENABLED"); value != "" {
		cfg.Session.Enabled = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_SESSION_RECENT_TURNS"); value != "" {
		if turns, err := strconv.Atoi(value); err == nil {
			cfg.Session.DefaultRecentTurns = turns
		}
	}
	if value := os.Getenv("LEGION_AGENT_SESSION_MAX_TURN_CHARS"); value != "" {
		if chars, err := strconv.Atoi(value); err == nil {
			cfg.Session.MaxTurnChars = chars
		}
	}
	if value := os.Getenv("LEGION_AGENT_TUI_SHOW_PROMPT"); value != "" {
		cfg.TUI.ShowPrompt = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_TUI_SHOW_THINKING"); value != "" {
		cfg.TUI.ShowThinking = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_TUI_COLOR_PROFILE"); value != "" {
		cfg.TUI.ColorProfile = value
	}
	if value := os.Getenv("LEGION_AGENT_SKILL_REGISTRY_URL"); value != "" {
		cfg.Skills.RegistryURL = value
	}
	if value := os.Getenv("LEGION_AGENT_SKILL_INSTALL_ROOT"); value != "" {
		cfg.Skills.InstallRoot = value
	}
	if value := os.Getenv("LEGION_AGENT_CONTEXT_FILES_ENABLED"); value != "" {
		cfg.ContextFiles.Enabled = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_CONTEXT_ROOT"); value != "" {
		cfg.ContextFiles.Root = value
	}
	if value := os.Getenv("LEGION_AGENT_SOUL_PATH"); value != "" {
		cfg.ContextFiles.SoulPath = value
	}
	if value := os.Getenv("LEGION_AGENT_TOOLS_PATH"); value != "" {
		cfg.ContextFiles.ToolsPath = value
	}
	if value := os.Getenv("LEGION_AGENT_USER_PATH"); value != "" {
		cfg.ContextFiles.UserPath = value
	}
	if value := os.Getenv("LEGION_AGENT_MEMORY_PATH"); value != "" {
		cfg.ContextFiles.MemoryPath = value
	}
	if value := os.Getenv("LEGION_AGENT_DOCS_ROOT"); value != "" {
		cfg.Workspace.DocsRoot = value
	}
	if value := os.Getenv("LEGION_AGENT_MEMORY_ROOT"); value != "" {
		cfg.Workspace.MemoryRoot = value
	}
	if value := os.Getenv("LEGION_AGENT_WORKSPACE_ROOT"); value != "" {
		cfg.Workspace.Root = value
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_INDEX_PATH"); value != "" {
		cfg.Tasks.IndexPath = value
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_ROOT"); value != "" {
		cfg.Tasks.Root = value
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_ARCHIVE_ROOT"); value != "" {
		cfg.Tasks.ArchiveRoot = value
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_MAX_INDEX_LINES"); value != "" {
		if lines, err := strconv.Atoi(value); err == nil {
			cfg.Tasks.MaxIndexLines = lines
		}
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_MAX_TASK_LINES"); value != "" {
		if lines, err := strconv.Atoi(value); err == nil {
			cfg.Tasks.MaxTaskLines = lines
		}
	}
	if value := os.Getenv("LEGION_AGENT_TASKS_MAX_MESSAGE_CHARS"); value != "" {
		if chars, err := strconv.Atoi(value); err == nil {
			cfg.Tasks.MaxMessageChars = chars
		}
	}
	if value := os.Getenv("LEGION_AGENT_WEB_ENABLED"); value != "" {
		cfg.Web.Enabled = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_WEB_ALLOW_PRIVATE_HOSTS"); value != "" {
		cfg.Web.AllowPrivateHosts = value == "true" || value == "1"
	}
	if value := os.Getenv("LEGION_AGENT_WEB_TIMEOUT_SECONDS"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			cfg.Web.TimeoutSeconds = seconds
		}
	}
	if value := os.Getenv("LEGION_AGENT_WEB_MAX_RESPONSE_KB"); value != "" {
		if kb, err := strconv.Atoi(value); err == nil {
			cfg.Web.MaxResponseKB = kb
		}
	}
	return nil
}

// UnlimitedToolRoundsCap is the value max_tool_rounds normalizes to when a
// config explicitly requests no limit (0 or negative). It is not truly infinite:
// the tool loop still stops here so a model that loops forever cannot burn tokens
// without bound — the runtime's hard-loop detection only evaluates after a task's
// runner returns, so it cannot break an unbounded in-flight tool loop. A normal
// task finishes in a handful of rounds and never approaches this.
const UnlimitedToolRoundsCap = 1000

// normalizeMaxToolRounds maps a configured max_tool_rounds to its effective
// value. A positive value is used as-is. Zero or negative means "no limit" and
// maps to UnlimitedToolRoundsCap — an explicit opt-in, since Load starts from
// defaultConfig (4) and only an explicit 0 in the JSON reaches here as 0; an
// absent field keeps the default 4.
//
// Note: runtime has its own same-named normalizeMaxToolRounds that maps <=0 to
// its own default (4) for directly constructed Runtimes. Production always
// normalizes here first, so the value the runtime sees is already positive and
// never hits that branch. The two differ on purpose: this is the user-facing
// "0 = unlimited" contract; the runtime one is a construction-time fallback.
func normalizeMaxToolRounds(rounds int) int {
	if rounds <= 0 {
		return UnlimitedToolRoundsCap
	}
	return rounds
}
