package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/stardust/legion-agent/internal/toolauth"
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
	Browser      BrowserConfig      `json:"browser"`
	Evolution    EvolutionConfig    `json:"evolution"`
	Plugins      PluginsConfig      `json:"plugins"`
}

// PluginsConfig is the WASM plugin deployment this process runs: which
// deployment manifest declares the target state, where the plugin packages
// live, the resource ceiling every plugin is held to, and how long a
// convergence may wait for a task boundary.
//
// Manifest is what turns the whole section on, and its absence is a
// CONTRACT-DECLARED OPTIONAL rather than a fallback: a config with no
// "plugins.manifest" key runs no plugins at all, which is the supported
// deployment for every installation that does not use them. Configuring a path
// is the opposite statement — serve then fails to start if the file cannot be
// read or parsed, because an operator who named a manifest meant to run it.
type PluginsConfig struct {
	// Manifest is the path to the deployment manifest (plugins.json, see
	// internal/plugin/manifest.ParseDeployment). Empty means plugins are not
	// enabled: no loader is built, nothing is mounted, and `agent plugins
	// status` says so. A non-empty path that cannot be read or parsed fails
	// serve assembly.
	//
	// A relative path resolves against the PROCESS working directory, not the
	// directory the config file lives in: `agent serve --config /etc/agent.json`
	// started from /srv reads /srv/plugins.json, not /etc/plugins.json. Use an
	// absolute path whenever the config is not read from the working directory.
	Manifest string `json:"manifest"`

	// Root is the directory every manifest entry's "source" resolves against,
	// and the boundary plugin code may be read from — a source that escapes it
	// is refused. It must be non-empty whenever Manifest is set.
	//
	// Like Manifest, a relative Root (the default "plugins" is one) resolves
	// against the PROCESS working directory rather than the config file's
	// directory, so where serve was started from decides which packages it
	// loads.
	Root string `json:"root"`

	// Cache is the directory unpacked REMOTE plugin packages are filed in,
	// under the sha256 digest that names them (see
	// internal/plugin/fetch.Cache). Like Manifest, a relative path resolves
	// against the PROCESS working directory.
	//
	// It has NO default, and its absence is not "use a temporary directory":
	// a manifest with a remote entry and no cache configured fails serve
	// assembly, naming the entry. Where downloaded code is written to disk is
	// a deployment decision, and choosing a directory on the operator's behalf
	// would be exactly the silent degradation this project's rules forbid. A
	// deployment whose entries are all local needs no cache and configures
	// none.
	Cache string `json:"cache"`

	// AllowInsecureSources permits plugin sources whose URL scheme is
	// "http://", as a POINTER so that "the operator wrote false" and "the
	// operator wrote nothing" stay distinguishable. Read it through
	// InsecureSourcesAllowed rather than dereferencing it: nil means REFUSED.
	//
	// It is the same technique RequireSignature uses, pointed the same way: a
	// security switch's unstated value must be the safe one. Turning it on is
	// a debugging aid, and it relaxes THE SCHEME AND NOTHING ELSE — a plaintext
	// entry still carries a mandatory digest, its bytes are still checked
	// against that digest, and its package is still signature-verified.
	//
	// Load normalizes it to a non-nil pointer, so a config that came through
	// Load states its policy explicitly.
	AllowInsecureSources *bool `json:"allow_insecure_sources"`

	// Fetch bounds the download of one remote plugin artifact.
	Fetch PluginFetchConfig `json:"fetch"`

	// Health is the deployment's tolerance for a plugin that keeps failing.
	Health PluginHealthConfig `json:"health"`

	// Keyring is the path to the trust keyring (a document of public keys, see
	// internal/plugin/sign.ParseKeyring) every plugin package's signature is
	// checked against. Like Manifest, a relative path resolves against the
	// PROCESS working directory, and a configured path that cannot be read or
	// parsed fails serve assembly rather than degrading to "no trust set".
	//
	// It must be non-empty whenever signatures are required (see
	// SignatureRequired): "verify every package" with nothing to verify
	// against is not a runnable deployment, and serve refuses to start in it.
	//
	// That rule is NOT enforced here, which is the difference between it and
	// Root's identically-worded one above. validatePlugins rejects an empty
	// Root because Root is decidable from the config text alone; deciding this
	// one means reading and parsing the keyring file, and Load never does file
	// I/O. It is enforced one layer out, at serve assembly, by
	// cli.resolvePluginKeyring — so a plugins section that satisfied Load is
	// not yet one serve will start on.
	Keyring string `json:"keyring"`

	// RequireSignature is the deployment's signature policy, as a POINTER so
	// that "the operator wrote false" and "the operator wrote nothing" stay
	// distinguishable. Read it through SignatureRequired rather than
	// dereferencing it: nil means REQUIRED.
	//
	// The pointer is the same technique manifest.Entry.Enabled uses, pointed
	// the other way: an absent "enabled" installs the plugin, an absent
	// "require_signature" demands a signature. A security switch's unstated
	// value must be the safe one, or every deployment written before the
	// switch existed runs without it.
	//
	// Load normalizes it to a non-nil pointer, so a config that came through
	// Load states its policy explicitly.
	RequireSignature *bool `json:"require_signature"`

	// Limits is the deployment's own resource ceiling, applied on top of each
	// plugin's own request (each limit becomes min(plugin's request, this)).
	Limits PluginLimitsConfig `json:"limits"`

	// ApplyWaitMs is how long a convergence waits for the tasks already running
	// to finish before giving up, in milliseconds. It must be positive whenever
	// Manifest is set: a convergence that waited forever would hold the task
	// gate shut against every new task with no way for anyone to recover.
	ApplyWaitMs int `json:"apply_wait_ms"`
}

// SignatureRequired reports whether this deployment requires every plugin
// package to carry a signature made by a key in its keyring.
//
// An UNSTATED policy (RequireSignature nil) is required, not optional. That is
// the one rule this method exists to hold: it is also what a PluginsConfig
// built as a struct literal — by an embedder, by a test — gets, and a zero
// value that read as "signatures off" would silently disarm every such
// assembly. Turning verification off is possible, but only by writing
// "require_signature": false down.
func (c PluginsConfig) SignatureRequired() bool {
	if c.RequireSignature == nil {
		return true
	}
	return *c.RequireSignature
}

// InsecureSourcesAllowed reports whether this deployment permits plugin
// sources served over plaintext http://.
//
// An UNSTATED policy (AllowInsecureSources nil) is REFUSED, which is also what
// a PluginsConfig built as a struct literal — by an embedder, by a test — gets.
// Allowing plaintext is possible, but only by writing
// "allow_insecure_sources": true down.
func (c PluginsConfig) InsecureSourcesAllowed() bool {
	if c.AllowInsecureSources == nil {
		return false
	}
	return *c.AllowInsecureSources
}

// PluginFetchConfig bounds the download of one remote plugin artifact. Both
// fields are FINITE and both are defaulted (30s, 32 MiB); neither reads zero
// as "unlimited", and a zero or negative value fails config validation by name
// whenever a manifest is configured. An unbounded download of code that is
// about to be executed is not a configuration this deployment offers.
type PluginFetchConfig struct {
	// TimeoutMs bounds one artifact download end to end, redirects included,
	// in milliseconds. Default 30000.
	TimeoutMs int `json:"timeout_ms"`

	// MaxBytes is the hard cap on the response body of one artifact download.
	// Default 33554432 (32 MiB). It bounds the COMPRESSED bytes coming off the
	// network; the decompressed package is bounded separately, where the
	// archive is unpacked.
	MaxBytes int64 `json:"max_bytes"`
}

// PluginHealthConfig is the deployment's tolerance for a misbehaving plugin.
//
// A plugin whose calls keep failing is not merely useless: it is advertised to
// the model on every prompt, so each round spends tokens offering a tool that
// will fail again. Past a threshold the deployment unloads it and says so.
//
// The count is CONSECUTIVE — one answered call resets it — and only the
// failures internal/plugin/host.ClassifyCallFault recognises are counted: a
// caller's cancellation is not one, a denial is not one (the plugin
// overstepped, it is not broken), and a tool answering "I could not do it" is
// not one either (that is the plugin working).
type PluginHealthConfig struct {
	// MaxConsecutiveFaults is the count at which a plugin is unloaded. Default
	// 5.
	//
	// It has NO "zero means unlimited" reading: validatePlugins rejects a
	// non-positive value whenever a manifest is configured. A deployment that
	// wants a high tolerance states a high number; leaving the policy unstated
	// and silently getting "never unload" is the degradation this project's
	// rules forbid.
	MaxConsecutiveFaults int `json:"max_consecutive_faults"`
}

// PluginLimitsConfig is the deployment-wide resource ceiling every plugin is
// held to. MaxMemoryPages and MaxInstances are "not declared" at zero, which
// leaves each plugin's own declared limit standing; TimeoutMs is not, because
// it is also the timeout of the HTTP client handed to a plugin granted the
// http capability, and a zero there would be an unbounded outbound request.
type PluginLimitsConfig struct {
	// TimeoutMs bounds one call into a plugin, and the outbound HTTP requests
	// a plugin granted "http" makes. It must be positive whenever
	// PluginsConfig.Manifest is set.
	TimeoutMs int `json:"timeout_ms"`

	// MaxMemoryPages caps a plugin instance's linear memory, in 64 KiB WASM
	// pages. Zero means the deployment declares no ceiling of its own.
	MaxMemoryPages uint32 `json:"max_memory_pages"`

	// MaxInstances caps how many instances of one plugin may exist at once.
	// Zero means the deployment declares no ceiling of its own.
	MaxInstances int `json:"max_instances"`
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

	// web_search (SearXNG) —— SearxngURL 为空则 web_search 不注册。
	SearxngURL           string `json:"searxng_url"`
	SearchEngine         string `json:"search_engine"`          // baidu|google|bing|空=实例默认
	SearchDefaultLimit   int    `json:"search_default_limit"`   // 默认 5，上限 20
	SearchTimeoutSeconds int    `json:"search_timeout_seconds"` // 默认 15
}

// BrowserConfig 配置内置浏览器运行时。默认关闭——开启需运行环境有可用 Chromium。
type BrowserConfig struct {
	Enabled  bool   `json:"enabled"`
	Headless bool   `json:"headless"`
	BinPath  string `json:"bin_path"` // 可选：指向系统 Chrome/Edge，绕过 go-rod 自动下载

	// RequireSandbox：没有外层隔离就**不启动浏览器**。
	//
	// 默认 false，因为目前只有 Windows 有实现（Job Object：kill-on-close + 内存
	// 上限），Linux/macOS 上打开它等于关掉浏览器功能。打开它的部署换来的是「要么
	// 被收束、要么明确失败」，而不是「以为自己被收束」。
	RequireSandbox bool `json:"require_sandbox"`
	// SessionTTLSeconds 是浏览器会话的空闲回收阈值（秒）。0 = 不回收（reaper 关闭）。
	SessionTTLSeconds int `json:"session_ttl_seconds"`
	// ReapIntervalSeconds 是 reaper 后台扫描间隔（秒）。0 = 默认 60s。
	ReapIntervalSeconds int `json:"reap_interval_seconds"`
	// MaxElements 是 a11y 观测保留的最大可交互元素数（次级硬上限）。0 = 默认 100。
	MaxElements int `json:"max_elements"`
	// SnapshotRuneThreshold 是渲染文本触发降级的 rune 阈值。0 = 关闭降级。
	SnapshotRuneThreshold int `json:"snapshot_rune_threshold"`
	// SnapshotTTLHours 是落盘全文快照保留时长（小时）。0 = 不清理。
	SnapshotTTLHours int `json:"snapshot_ttl_hours"`
	// SnapshotArchiveDir 是相对工具根的落盘子目录。空 = 默认 .legion/browser/snapshots。
	SnapshotArchiveDir string `json:"snapshot_archive_dir"`
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
	//
	// Measured 2026-08-16 against DeepSeek: cache_control is accepted (HTTP 200)
	// and IGNORED. DeepSeek caches automatically by longest common token prefix
	// from token 0, and credits nothing for a partial head match — an appended
	// tail hit 2048 tokens while a mid-list insertion of the same size hit 0.
	// So on DeepSeek this flag buys nothing; what pays there is keeping the
	// prefix stable, not marking it.
	PromptCache bool `json:"prompt_cache,omitempty"`
	// ContextLength is the model's context window in tokens, used by the GUI to
	// show the active model's context size. Optional (0 = unset); the GUI shows
	// "context 未设" when absent. Data-driven per the 数值必须数据驱动 铁律 —— the
	// provider /models API does not return it.
	ContextLength int `json:"context_length"`
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
	// LoopbackHardening enables the App/GUI-mode loopback hardening path: a
	// one-time bearer token is minted per startup (when AdminToken is unset) and
	// a handshake file is written so a local frontend can auto-connect. It is
	// also implied automatically when the server binds to a loopback address.
	LoopbackHardening bool `json:"loopback_hardening"`
	// HandshakeFile is where the {baseURL, token} handshake is written in
	// loopback hardening mode. Empty means the default under the platform
	// AppDataDir (handshake.json).
	HandshakeFile string `json:"handshake_file"`
	// FileBaseURL is the public base for generated-file links (no trailing slash),
	// e.g. "https://agent.example.com". Empty (default) means links are returned
	// as relative paths ("/v1/files?..."), which the loopback frontend resolves
	// against its own known base URL. Deployment sets a domain here.
	FileBaseURL string `json:"file_base_url,omitempty"`
}

type ServiceConfig struct {
	BackgroundInterval string `json:"background_interval"`
}

type RuntimeConfig struct {
	DemoResponse  string `json:"demo_response"`
	MaxToolRounds int    `json:"max_tool_rounds"`
	// CompactTokenThreshold triggers conversation compaction when the tool loop's
	// accumulated prompt tokens exceed it. 0 disables compaction (default).
	CompactTokenThreshold int `json:"compact_token_threshold,omitempty"`
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
	// DisabledTools names the tools this agent may not use (deny-list). Absent /
	// null / empty means no tool is disabled — every tool is available. Each name
	// must be a known gateable tool (validated at agent assembly); meta-tools are
	// never listed here and cannot be disabled.
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// Debug turns on the inference debug probe: before each model call the
	// runtime logs a per-message breakdown of the outgoing prompt (role, rune
	// count, tool-call/image counts and a short preview) plus the total size, so
	// a bloated prompt can be traced to the exact message carrying it. Optional
	// diagnostic switch; absent / false means off (no probe output).
	Debug bool `json:"debug,omitempty"`
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

// Load reads the agent configuration.
//
// The path it reads is decided in this order:
//
//  1. opts.Path, when the caller named one (`--config`). A named file that is
//     missing or undecodable is an ERROR: the operator said which file to use.
//  2. the default location — <STARDUST_HOME or ~/.stardust>/agent.json — when
//     a regular file is actually there. A file found here is loaded on exactly
//     the same terms: if it cannot be decoded, Load fails rather than falling
//     back to built-in defaults, because running a deployment on settings
//     nobody wrote while the operator's real config sits unread is the worse
//     outcome by far.
//  3. nothing. Built-in defaults, no error — this is what a fresh install
//     looks like before its first config file exists, and it is a supported
//     way to run (a demo-response agent with no MaaS and no plugins).
//
// Environment variables are overlaid last (applyEnv) in every case.
//
// NOTE ON RELATIVE PATHS: values inside the config that name files —
// storage.path, plugins.manifest/root/cache/keyring, context_files roots —
// still resolve against the PROCESS WORKING DIRECTORY, not against the
// directory the config was read from. Loading ~/.stardust/agent.json from
// some other directory therefore does NOT move those paths into ~/.stardust.
// Use absolute paths there, or run from the directory they are relative to.
func Load(ctx context.Context, opts Options) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	path := opts.Path
	if path == "" {
		path = DefaultConfigPathIfPresent()
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%w: %s", ErrConfigNotFound, path)
		}
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("decode config %q: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	cfg.Runtime.MaxToolRounds = normalizeMaxToolRounds(cfg.Runtime.MaxToolRounds)

	// Validate DisabledTools: each name must be a known gateable tool (fail-loud).
	// Build the set once, outside the loop, for efficiency.
	gateableNames := toolauth.GateableToolNames()
	for _, name := range cfg.Runtime.DisabledTools {
		if !gateableNames[name] {
			return Config{}, fmt.Errorf("unknown disabled tool %q", name)
		}
	}

	// Validate server.file_base_url: contract-optional (empty = relative-path
	// links), but a non-empty value must be a valid http(s) URL — fail-loud
	// rather than silently keeping a broken base for generated-file links.
	if cfg.Server.FileBaseURL != "" {
		trimmed := strings.TrimRight(cfg.Server.FileBaseURL, "/")
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return Config{}, fmt.Errorf("server.file_base_url %q invalid: %w", cfg.Server.FileBaseURL, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, fmt.Errorf("server.file_base_url %q invalid: unsupported scheme %q", cfg.Server.FileBaseURL, parsed.Scheme)
		}
		cfg.Server.FileBaseURL = trimmed
	}

	// Normalize the signature policy into an explicit statement, through the
	// one function that decides what an absent one means. A config that came
	// through Load therefore says what it does rather than leaving the next
	// reader to remember which way an absent pointer falls.
	required := cfg.Plugins.SignatureRequired()
	cfg.Plugins.RequireSignature = &required

	// The plaintext-source switch is normalized the same way, through the one
	// function that decides what an absent one means.
	insecure := cfg.Plugins.InsecureSourcesAllowed()
	cfg.Plugins.AllowInsecureSources = &insecure

	if err := validatePlugins(cfg.Plugins); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validatePlugins checks the plugin section's internal consistency. An absent
// plugins.manifest is the documented "plugins are off" state and needs no
// other field; a present one makes Root, Limits.TimeoutMs and ApplyWaitMs
// load-bearing, and each is rejected by name rather than left to fail later as
// an unbounded HTTP request, an unconfined plugin source, or a convergence
// that waits forever.
func validatePlugins(cfg PluginsConfig) error {
	if strings.TrimSpace(cfg.Manifest) == "" {
		return nil
	}
	if strings.TrimSpace(cfg.Root) == "" {
		return fmt.Errorf("plugins.root is empty while plugins.manifest is %q; "+
			"every entry's source resolves against the root, and it is what bounds where plugin code is read from", cfg.Manifest)
	}
	if cfg.ApplyWaitMs <= 0 {
		return fmt.Errorf("plugins.apply_wait_ms is %d; it must be positive, "+
			"since a convergence that waits forever for a task boundary holds the gate shut against every new task", cfg.ApplyWaitMs)
	}
	if cfg.Limits.TimeoutMs <= 0 {
		return fmt.Errorf("plugins.limits.timeout_ms is %d; it must be positive, "+
			"since it also bounds the outbound HTTP requests of a plugin granted the http capability", cfg.Limits.TimeoutMs)
	}
	if cfg.Fetch.TimeoutMs <= 0 {
		return fmt.Errorf("plugins.fetch.timeout_ms is %d; it must be positive, "+
			"since a remote plugin artifact download with no deadline never fails and never finishes", cfg.Fetch.TimeoutMs)
	}
	if cfg.Health.MaxConsecutiveFaults <= 0 {
		return fmt.Errorf("plugins.health.max_consecutive_faults is %d; it must be positive "+
			"(a deployment that tolerates more failures states a larger number; zero has no "+
			"'unlimited' reading here)", cfg.Health.MaxConsecutiveFaults)
	}
	if cfg.Fetch.MaxBytes <= 0 {
		return fmt.Errorf("plugins.fetch.max_bytes is %d; it must be positive, "+
			"since zero does not mean unlimited here: it is the cap on bytes downloaded from a remote plugin source", cfg.Fetch.MaxBytes)
	}
	return nil
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
			Enabled:              true,
			AllowPrivateHosts:    false,
			TimeoutSeconds:       20,
			MaxResponseKB:        512,
			Allowlist:            []string{},
			SearxngURL:           "",
			SearchEngine:         "",
			SearchDefaultLimit:   5,
			SearchTimeoutSeconds: 15,
		},
		Browser: BrowserConfig{
			MaxElements:           100,
			SnapshotRuneThreshold: 15000,
			SnapshotTTLHours:      24,
			SnapshotArchiveDir:    ".legion/browser/snapshots",
		},
		Evolution: EvolutionConfig{
			DegradationThreshold:   0.2,
			DegradationWindowDays:  14,
			DegradationScanMinutes: 60,
		},
		// Manifest stays empty on purpose: plugins are off until an operator
		// names a deployment manifest. Everything else is defaulted so that
		// naming one is the only edit a working plugin deployment needs.
		//
		// RequireSignature stays nil on purpose too, and is the one default
		// NOT written here: a non-nil default would be indistinguishable from
		// an operator's own "true" and would sit in front of
		// SignatureRequired, leaving the meaning of an absent setting decided
		// in two places instead of one. Nil is read as "required" there.
		// AllowInsecureSources is nil for the same reason, and reads as
		// "refused"; Cache is empty for a different one — see its doc comment:
		// there is no safe default location for downloaded code.
		Plugins: PluginsConfig{
			Root:        "plugins",
			Limits:      PluginLimitsConfig{TimeoutMs: 10000, MaxMemoryPages: 256, MaxInstances: 4},
			Fetch:       PluginFetchConfig{TimeoutMs: 30000, MaxBytes: 33554432},
			Health:      PluginHealthConfig{MaxConsecutiveFaults: 5},
			ApplyWaitMs: 60000,
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
