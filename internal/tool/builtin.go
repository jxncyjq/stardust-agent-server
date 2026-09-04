package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/contextfiles"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
)

const (
	// searchContentMaxFileBytes caps how much of any single file search_content
	// will read. It mirrors read_file's own limit: without it one large log in
	// the workspace pulled the entire file into memory.
	searchContentMaxFileBytes = 256 * 1024
	// searchContentMaxMatches and listFilesMaxEntries cap how much these tools
	// return. Their output goes straight into the model context, so an
	// unbounded result is both a memory risk and an unusable answer. Truncation
	// is always announced — a silently shortened list reads as a complete one.
	searchContentMaxMatches = 200
	listFilesMaxEntries     = 500
	// searchContentReadChunk bounds how many bytes readFileContext consumes
	// between context checks.
	searchContentReadChunk = 32 * 1024
	// readFilePageRunes is both the default and the maximum number of runes one
	// read_file call returns. It is deliberately below runtime's
	// maxToolResultChars (4000): runtime.renderToolResultContent truncates any
	// longer tool result to that cap at dispatch time, which would silently cut
	// the page short and defeat pagination.
	readFilePageRunes = 3500
	// toolResultBudgetRunes mirrors runtime's maxToolResultChars (4000):
	// runtime.renderToolResultContent hard-truncates any longer tool result, so
	// a read_file page plus everything appended to it (the subtree agents.md
	// note, the repeat-read notice, the continuation hint) must fit inside this
	// budget. Without that accounting the continuation hint — the one thing that
	// tells the model how to read the rest — is what gets cut, which is exactly
	// the failure pagination exists to prevent. Keep in sync with
	// runtime.defaultMaxToolResultChars.
	toolResultBudgetRunes = 4000
	// minReadFilePageRunes floors the page so a large agents.md note can never
	// squeeze the actual file content down to nothing.
	minReadFilePageRunes = 500
	// maxNoteRunesInResult caps how much of the subtree agents.md note may ride
	// along with a page. The note is bounded only by ContextFiles.MaxFileChars
	// (20000 by default) — five times the whole tool result budget — so capping
	// the page alone cannot keep the result within budget: the note is appended
	// after it. Truncating the note here is what makes the budget real, and it
	// is announced in the output rather than left to runtime's silent front
	// truncation, which would eat the page instead.
	maxNoteRunesInResult = 800
	// repeatNoticeMaxRunes bounds repeatNotice's rendered length (a fixed
	// sentence plus a small count), reserved up front because whether the notice
	// applies is only known after the page has been sliced.
	repeatNoticeMaxRunes = 120
)

// paginateRunes returns the [offset, offset+limit) rune window of content, the
// offset the caller should pass next (-1 when the window reached the end) and
// the content's total rune count. Slicing by rune (not byte) keeps multibyte
// text intact.
//
// An offset at or past the end is an error rather than an empty page: an empty
// result reads to the model as "the file ends here", which is exactly the
// misunderstanding this pagination exists to prevent.
func paginateRunes(content string, offset, limit int) (string, int, int, error) {
	runes := []rune(content)
	total := len(runes)
	if offset < 0 {
		return "", 0, total, fmt.Errorf("read_file offset must not be negative, got %d", offset)
	}
	if offset >= total && total > 0 {
		return "", 0, total, fmt.Errorf("read_file offset %d is past the end of the file (%d chars)", offset, total)
	}
	if limit <= 0 || limit > readFilePageRunes {
		limit = readFilePageRunes
	}
	end := offset + limit
	if end >= total {
		return string(runes[offset:]), -1, total, nil
	}
	return string(runes[offset:end]), end, total, nil
}

// readFilePageBudget shrinks a requested page size so the whole read_file
// result — page plus continuation hint, repeat-read notice and subtree
// agents.md note — fits inside toolResultBudgetRunes. Without it a large
// agents.md note pushes the result past runtime's maxToolResultChars, which
// truncates from the front and takes the continuation hint with it, leaving the
// model unable to page through the file: the very loop this feature removes.
//
// The hint's own length is measured exactly by formatting it with the largest
// numbers it could carry (the file's total rune count in every slot) and the
// real path, so a long path is charged for rather than assumed small. The page
// never drops below minReadFilePageRunes, so an oversized note degrades the
// page instead of erasing it.
func readFilePageBudget(limit int, content, path string, noteRunes int) int {
	if limit <= 0 || limit > readFilePageRunes {
		limit = readFilePageRunes
	}
	total := len([]rune(content))
	hint := fmt.Sprintf("…[已返回第 %d-%d 字，共 %d 字；继续读用 read_file(path=%q, offset=%d)]\n",
		total, total, total, path, total)
	// noteRunes is charged at its capped size: capReadFileNote trims the note to
	// maxNoteRunesInResult before it is appended, so charging the raw length here
	// would shrink the page for space the note will not use.
	if noteRunes > maxNoteRunesInResult {
		noteRunes = maxNoteRunesInResult
	}
	budget := toolResultBudgetRunes - noteRunes - repeatNoticeMaxRunes - len([]rune(hint))
	if budget < minReadFilePageRunes {
		budget = minReadFilePageRunes
	}
	if limit > budget {
		return budget
	}
	return limit
}

// capReadFileNote trims a subtree agents.md note to maxNoteRunesInResult and
// says so in the text it returns. The note is otherwise bounded only by
// ContextFiles.MaxFileChars, which is far larger than the whole tool result
// budget; leaving it uncapped would push the result past
// maxToolResultChars and hand it to runtime's front truncation, which cuts the
// file content and the continuation hint rather than the note.
func capReadFileNote(note string) string {
	runes := []rune(note)
	if len(runes) <= maxNoteRunesInResult {
		return note
	}
	return string(runes[:maxNoteRunesInResult]) +
		fmt.Sprintf("\n…[本目录约定已截断，省略 %d 字；完整内容见该目录的 agents.md]", len(runes)-maxNoteRunesInResult)
}

// intArg parses an optional integer tool argument. An absent or empty value
// yields def; a present but unparseable value is an error rather than a silent
// fallback to the default, which would hide a malformed call from the model.
func intArg(args map[string]string, name string, def int) (int, error) {
	raw := strings.TrimSpace(args[name])
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("read_file %s must be an integer, got %q: %w", name, raw, err)
	}
	return v, nil
}

// WorkspaceRegistryOption configures optional behavior of a workspace registry
// built by NewWorkspaceRegistry, such as the per-directory agents.md injection
// that write_file performs after a successful write.
type WorkspaceRegistryOption func(*workspaceRegistryOptions)

type workspaceRegistryOptions struct {
	maxFileChars int
	homeDir      string // user home directory for resident-path exclusion
	projectRoot  string
	injected     *injectedAgentsSet
	// injectionEnabled records that WithAgentsInjection was supplied,
	// distinguishing "injection off" from "maxFileChars/homeDir happen to be
	// zero value" so finalizeWorkspaceRegistryOptions knows whether to seed
	// projectRoot/injected at all.
	injectionEnabled bool
	// readHistory tracks read_file repeats within this registry's task, so an
	// unchanged repeat read can be flagged via repeatNotice. Always non-nil by
	// the time a registry is returned from NewWorkspaceRegistry /
	// NewFileReadWriteWorkspaceRegistry — see those constructors.
	readHistory *readHistory
	// pluginTools is the process-wide registry mounted plugins contribute to.
	// When set, this registry INHERITS from it (see Registry.InheritFrom) and
	// its permission enforcer learns to admit whatever the plugins registered.
	pluginTools *Registry
}

// WithPluginTools makes the built registry reach the tools mounted plugins
// contribute, so the model can call them.
//
// Two halves, and both are needed: the registry inherits from plugins (name
// resolution and the catalog), and its permission enforcer takes plugins as a
// dynamic source of admissible tool names (a plugin's name appears at run time
// and can never be in the compile-time whitelist). With only the first, every
// plugin tool would resolve and then be refused.
//
// What it does NOT change: the built registry keeps its own execution policy,
// guardrails and audit log, so a plugin's tool runs under this task's rules.
// A plugin tool declaring a high or critical risk level is still refused by
// the execution policy, exactly as a builtin one would be.
func WithPluginTools(plugins *Registry) WorkspaceRegistryOption {
	return func(o *workspaceRegistryOptions) { o.pluginTools = plugins }
}

// applyPluginTools wires both halves of WithPluginTools, or does nothing when
// no plugin registry was supplied.
//
// It PANICS on an enforcer it does not recognise rather than skipping the
// dynamic source: skipping would produce a registry where every plugin tool
// resolves and is then refused — a deployment that looks wired and works for
// nothing.
func applyPluginTools(registry *Registry, plugins *Registry) {
	if plugins == nil {
		return
	}
	registry.InheritFrom(plugins)
	batch, ok := registry.enforcer.(BatchRolePermissionEnforcer)
	if !ok {
		panic(fmt.Sprintf("tool: WithPluginTools: enforcer is %T, which has no dynamic tool source; "+
			"every plugin tool would resolve and then be refused", registry.enforcer))
	}
	registry.enforcer = batch.WithDynamicTools(plugins.HasTool)
}

// injectedAgentsSet tracks which non-resident agents.md files have already
// been injected into a task's tool results, so the same directory's
// conventions are not repeated on every write_file call within that task.
//
// It supports two seeding modes. newInjectedAgentsSet takes an
// already-computed resident set and seeds immediately — used directly by
// tests and anywhere the resident set is already in hand. newLazyInjectedAgentsSet
// instead stores root/homeDir and defers the contextfiles.ResidentAgentsPaths
// call to the first markIfNew, because the registry constructors that own
// this set (NewWorkspaceRegistry, NewFileReadWriteWorkspaceRegistry) have no
// error return through which a ResidentAgentsPaths failure could be reported
// at construction time; deferring lets that error surface as the tool call's
// own error instead of being silently dropped (CLAUDE.md fail-loud).
type injectedAgentsSet struct {
	mu      sync.Mutex
	seen    map[string]bool
	seeded  bool
	root    string
	homeDir string
}

// newInjectedAgentsSet returns a set seeded immediately with resident
// (always-in-context) agents.md paths, so markIfNew reports them as already
// seen from the start.
func newInjectedAgentsSet(resident map[string]bool) *injectedAgentsSet {
	seen := make(map[string]bool, len(resident)+8)
	for p := range resident {
		seen[filepath.Clean(p)] = true
	}
	return &injectedAgentsSet{seen: seen, seeded: true}
}

// newLazyInjectedAgentsSet returns a set that seeds itself with resident
// agents.md paths for root/homeDir (via contextfiles.ResidentAgentsPaths) on
// the first markIfNew call rather than at construction. See injectedAgentsSet
// for why.
func newLazyInjectedAgentsSet(root, homeDir string) *injectedAgentsSet {
	return &injectedAgentsSet{root: root, homeDir: homeDir}
}

// markIfNew returns true the first time absPath is seen, false afterwards (and
// for paths seeded as resident). Concurrency-safe for parallel tool calls. For
// a lazily-constructed set, the first call resolves the resident set via
// contextfiles.ResidentAgentsPaths; a failure there is returned rather than
// swallowed, per CLAUDE.md fail-loud.
func (s *injectedAgentsSet) markIfNew(absPath string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seeded {
		resident, err := contextfiles.ResidentAgentsPaths(s.root, s.homeDir)
		if err != nil {
			return false, fmt.Errorf("resolve resident agents.md paths: %w", err)
		}
		seen := make(map[string]bool, len(resident)+8)
		for p := range resident {
			seen[filepath.Clean(p)] = true
		}
		s.seen = seen
		s.seeded = true
	}
	p := filepath.Clean(absPath)
	if s.seen[p] {
		return false, nil
	}
	s.seen[p] = true
	return true, nil
}

// readEntry holds the last-seen content hash and cumulative read count for one
// file path within a task.
type readEntry struct {
	hash  string
	count int
}

// readHistory tracks, per task (one per workspace registry), how many times each
// file path has been read and the hash of the last content returned, so a repeat
// read of unchanged content can be flagged to the model instead of silently
// re-sending an identical full copy every round.
type readHistory struct {
	mu   sync.Mutex
	seen map[string]readEntry
}

// newReadHistory returns an empty readHistory ready for concurrent use.
func newReadHistory() *readHistory {
	return &readHistory{seen: make(map[string]readEntry)}
}

// record notes a read of path with the given content. It returns the running
// read count for that path (including this read) and whether this read returned
// the same content as the previous read of the same path (unchanged is always
// false on the first read). Thread-safe for concurrent tool calls.
func (h *readHistory) record(path string, content string) (count int, unchanged bool) {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.seen[path]
	count = prev.count + 1
	unchanged = count > 1 && prev.hash == hash
	h.seen[path] = readEntry{hash: hash, count: count}
	return count, unchanged
}

// repeatNotice is the visible banner prepended to a read_file result when the
// model re-reads a file whose content has not changed within the task. It names
// the repeat count and steers the model toward search_content so it stops
// re-reading whole files (the tool-loop token blow-up this addresses).
func repeatNotice(count int) string {
	return fmt.Sprintf(
		"⚠️ 本任务已第 %d 次读取此文件，内容与前次相同、未变化。该内容此前已在上文出现；"+
			"若只需其中某段，请改用 search_content 精确检索，避免重复整篇读取消耗上下文。\n\n",
		count,
	)
}

// WithAgentsInjection enables the read_file/search_content/write_file tools to
// append any not-yet-seen subdirectory agents.md / AGENTS.md (local directory
// conventions) encountered on the path to the file they just touched. maxFileChars
// caps the injected content. homeDir is the user home directory (used to compute
// the resident ~/.stardust/agents.md path so it is never re-injected). An empty
// homeDir degrades gracefully: only the two workspace-local resident paths are
// excluded.
func WithAgentsInjection(maxFileChars int, homeDir string) WorkspaceRegistryOption {
	return func(o *workspaceRegistryOptions) {
		o.maxFileChars = maxFileChars
		o.homeDir = homeDir
		o.injectionEnabled = true
	}
}

// WithProjectRoot sets the root from which subtree agents.md injection walks
// down to a touched file's directory (contextfiles.SubtreeAgentsChain's
// projectRoot). It only matters together with WithAgentsInjection; when
// injection is enabled but WithProjectRoot is not supplied,
// finalizeWorkspaceRegistryOptions defaults it to the registry's own sandbox
// root, since the project root and the file-tool root are always the same
// directory for these workspace registries.
func WithProjectRoot(projectRoot string) WorkspaceRegistryOption {
	return func(o *workspaceRegistryOptions) { o.projectRoot = projectRoot }
}

// finalizeWorkspaceRegistryOptions completes options after opts have been
// applied. When agents.md injection was not requested (WithAgentsInjection
// never called), it is a no-op — the zero-value options already make
// subtreeAgentsNote a no-op via its own nil/empty guards. When injection was
// requested, it defaults projectRoot to absRoot (unless WithProjectRoot
// overrode it) and lazily seeds injected, so callers of NewWorkspaceRegistry /
// NewFileReadWriteWorkspaceRegistry never have to seed either by hand.
func finalizeWorkspaceRegistryOptions(absRoot string, options workspaceRegistryOptions) workspaceRegistryOptions {
	if !options.injectionEnabled {
		return options
	}
	if strings.TrimSpace(options.projectRoot) == "" {
		options.projectRoot = absRoot
	}
	if options.injected == nil {
		options.injected = newLazyInjectedAgentsSet(options.projectRoot, options.homeDir)
	}
	return options
}

// NewWorkspaceRegistry returns a registry with read-only tools (read_file,
// search_content, list_files) plus the write_file tool. write_file requires
// overwrite=true when the target file already exists. When WithAgentsInjection
// is supplied, a successful write also appends the nearest subdirectory
// AGENTS.md (excluding the resident root and config-directory AGENTS.md) to the
// tool result so the model sees local directory conventions.
func NewWorkspaceRegistry(root string, audit port.AuditLog, opts ...WorkspaceRegistryOption) *Registry {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		// filepath.Abs only fails when os.Getwd does — the process has no
		// resolvable working directory, an unrecoverable environment fault.
		// Falling back to the relative root would hand NewWorkspacePathGuard a
		// path whose containment comparison is meaningless, silently disabling
		// the sandbox. A security boundary must not degrade quietly.
		panic(fmt.Sprintf("tool: cannot resolve workspace root %q: %v", root, err))
	}
	var options workspaceRegistryOptions
	for _, opt := range opts {
		opt(&options)
	}
	guard := port.NewWorkspacePathGuard(absRoot)
	registry := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{
			"read_file", "search_content", "list_files", "write_file",
			"create_task", "claim_task", "update_task", "append_task_message", "read_task", "rebuild_tasks",
			"send_message", "read_messages",
			"fetch_url", "web_search", "web_extract", "session_search", "delegate_task", "moa_consult",
		}}),
		NewBatchRolePermissionEnforcer(map[string]bool{
			"developer:read_file":           true,
			"developer:search_content":      true,
			"developer:list_files":          true,
			"developer:write_file":          true,
			"developer:delegate_task":       true,
			"developer:moa_consult":         true,
			"developer:create_task":         true,
			"developer:claim_task":          true,
			"developer:update_task":         true,
			"developer:append_task_message": true,
			"developer:read_task":           true,
			"developer:rebuild_tasks":       true,
			"developer:send_message":        true,
			"developer:read_messages":       true,
			"developer:fetch_url":           true,
			"developer:web_search":          true,
			"developer:web_extract":         true,
			"developer:session_search":      true,
			"developer:browser_open":        true,
			"developer:browser_read":        true,
			"developer:browser_click":       true,
			"developer:browser_type":        true,
			"developer:browser_close":       true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	options = finalizeWorkspaceRegistryOptions(absRoot, options)
	if options.readHistory == nil {
		options.readHistory = newReadHistory()
	}
	registerReadOnlyDescriptors(registry, absRoot, guard, options)
	registerWriteFileDescriptor(registry, absRoot, guard, options)
	applyPluginTools(registry, options.pluginTools)
	return registry
}

// registerWriteFileDescriptor adds the write_file tool to an already-constructed
// registry. write_file requires overwrite=true when the target already exists,
// and its filesystem access is bounded by guard. Whether a successful write
// appends not-yet-seen directory agents.md conventions to the result is
// decided entirely by options (specifically options.injected != nil, set up by
// finalizeWorkspaceRegistryOptions when WithAgentsInjection was supplied) — see
// subtreeAgentsNote. The registry's execution policy and permission enforcer
// must already allow write_file; this only registers the descriptor.
func registerWriteFileDescriptor(registry *Registry, absRoot string, guard port.WorkspacePathGuard, options workspaceRegistryOptions) {
	registry.RegisterDescriptor(Descriptor{
		Name:        "write_file",
		Description: fmt.Sprintf("Write content to a file inside the workspace root (%s). Arguments: path, content, optional overwrite (default false). Fails if the file exists and overwrite is not true.", absRoot),
		RiskLevel:   "medium",
		Timeout:     5 * time.Second,
		Group:       "files",
		Sensitive:   true, // writes to the filesystem
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": fmt.Sprintf("File path relative to workspace root (%s) or absolute path within workspace root.", absRoot)},
				"content":   map[string]any{"type": "string"},
				"overwrite": map[string]any{"type": "boolean"},
			},
		},
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return writeFileTool(ctx, absRoot, guard, call, options)
	}))
}

// NewFileReadOnlyWorkspaceRegistry returns a registry whose *filesystem* access
// is read-only: read_file, search_content and list_files, with no write_file.
//
// The name says "file read-only" rather than plain "read-only" on purpose. The
// registry is not side-effect free: callers routinely add task-ledger, agent
// messaging and web tools to it afterwards (see agent_resolver.ResolveTaskRunner
// and cli.defaultTaskRunner.RunTask), and those do write — to the ledger, to
// other agents' inboxes, and out to the network. Reading the old name as "this
// agent cannot change anything" was the misconception worth removing.
func NewFileReadOnlyWorkspaceRegistry(root string, audit port.AuditLog) *Registry {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		// filepath.Abs only fails when os.Getwd does — the process has no
		// resolvable working directory, an unrecoverable environment fault.
		// Falling back to the relative root would hand NewWorkspacePathGuard a
		// path whose containment comparison is meaningless, silently disabling
		// the sandbox. A security boundary must not degrade quietly.
		panic(fmt.Sprintf("tool: cannot resolve workspace root %q: %v", root, err))
	}
	guard := port.NewWorkspacePathGuard(absRoot)
	registry := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{
			"read_file", "search_content", "list_files",
			"create_task", "claim_task", "update_task", "append_task_message", "read_task", "rebuild_tasks",
			"send_message", "read_messages",
			"fetch_url", "web_search", "web_extract", "session_search", "delegate_task", "moa_consult",
		}}),
		NewBatchRolePermissionEnforcer(map[string]bool{
			"developer:read_file":           true,
			"developer:search_content":      true,
			"developer:list_files":          true,
			"developer:delegate_task":       true,
			"developer:moa_consult":         true,
			"developer:create_task":         true,
			"developer:claim_task":          true,
			"developer:update_task":         true,
			"developer:append_task_message": true,
			"developer:read_task":           true,
			"developer:rebuild_tasks":       true,
			"developer:send_message":        true,
			"developer:read_messages":       true,
			"developer:fetch_url":           true,
			"developer:web_search":          true,
			"developer:web_extract":         true,
			"developer:session_search":      true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	registerReadOnlyDescriptors(registry, absRoot, guard, workspaceRegistryOptions{})
	return registry
}

// NewFileReadWriteWorkspaceRegistry returns a registry with the read-only file
// tools plus write_file, all sandboxed to root. It is the serve / per-agent
// counterpart to NewWorkspaceRegistry: same write capability, and — like
// NewWorkspaceRegistry — agents.md injection across read_file/search_content/
// write_file only happens when a caller opts in via WithAgentsInjection; by
// default (no opts) a directory's agents.md does not leak into a server task's
// tool results. write_file stays Sensitive, so Manual mode still gates it and
// Plan mode still excludes it. Callers add task-ledger, messaging and web
// tools afterwards, exactly as with the read-only constructor.
func NewFileReadWriteWorkspaceRegistry(root string, audit port.AuditLog, opts ...WorkspaceRegistryOption) *Registry {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		// filepath.Abs only fails when os.Getwd does — the process has no
		// resolvable working directory, an unrecoverable environment fault.
		// Falling back to the relative root would hand NewWorkspacePathGuard a
		// path whose containment comparison is meaningless, silently disabling
		// the sandbox. A security boundary must not degrade quietly.
		panic(fmt.Sprintf("tool: cannot resolve workspace root %q: %v", root, err))
	}
	var options workspaceRegistryOptions
	for _, opt := range opts {
		opt(&options)
	}
	guard := port.NewWorkspacePathGuard(absRoot)
	registry := NewRegistry(
		NewExecutionPolicy(ExecutionPolicyConfig{AutoAllowTools: []string{
			"read_file", "search_content", "list_files", "write_file",
			"create_task", "claim_task", "update_task", "append_task_message", "read_task", "rebuild_tasks",
			"send_message", "read_messages",
			"fetch_url", "web_search", "web_extract", "session_search", "delegate_task", "moa_consult",
		}}),
		NewBatchRolePermissionEnforcer(map[string]bool{
			"developer:read_file":           true,
			"developer:search_content":      true,
			"developer:list_files":          true,
			"developer:write_file":          true,
			"developer:delegate_task":       true,
			"developer:moa_consult":         true,
			"developer:create_task":         true,
			"developer:claim_task":          true,
			"developer:update_task":         true,
			"developer:append_task_message": true,
			"developer:read_task":           true,
			"developer:rebuild_tasks":       true,
			"developer:send_message":        true,
			"developer:read_messages":       true,
			"developer:fetch_url":           true,
			"developer:web_search":          true,
			"developer:web_extract":         true,
			"developer:session_search":      true,
			"developer:browser_open":        true,
			"developer:browser_read":        true,
			"developer:browser_click":       true,
			"developer:browser_type":        true,
			"developer:browser_close":       true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	options = finalizeWorkspaceRegistryOptions(absRoot, options)
	if options.readHistory == nil {
		options.readHistory = newReadHistory()
	}
	registerReadOnlyDescriptors(registry, absRoot, guard, options)
	registerWriteFileDescriptor(registry, absRoot, guard, options)
	applyPluginTools(registry, options.pluginTools)
	return registry
}

// registerReadOnlyDescriptors adds read_file, search_content, and list_files
// to an already-constructed registry. Shared by all three workspace registry
// constructors; options carries the (possibly disabled) agents.md injection
// settings that read_file and search_content pass through to
// subtreeAgentsNote after resolving their target directory.
func registerReadOnlyDescriptors(registry *Registry, absRoot string, guard port.WorkspacePathGuard, options workspaceRegistryOptions) {
	registry.RegisterDescriptor(Descriptor{
		Name:        "read_file",
		Description: fmt.Sprintf("Read a UTF-8 text file inside the workspace root (%s). The path argument can be relative (resolved against workspace root) or absolute (must be within workspace root). Long files are returned one page at a time: pass offset to continue reading where the previous page ended.", absRoot),
		RiskLevel:   "low",
		Timeout:     2 * time.Second,
		Group:       "files",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": fmt.Sprintf("File path relative to workspace root (%s) or absolute path within workspace root.", absRoot)},
				"offset": map[string]any{"type": "number", "description": "Start reading at this character (rune) offset. Defaults to 0. Use the offset printed at the end of a truncated page to read the rest."},
				"limit":  map[string]any{"type": "number", "description": fmt.Sprintf("Maximum characters to return, capped at %d (the default).", readFilePageRunes)},
			},
		},
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return readFileTool(ctx, absRoot, guard, call, options)
	}))
	registry.RegisterDescriptor(Descriptor{
		Name:        "search_content",
		Description: fmt.Sprintf("Search text files inside the workspace root (%s). Arguments: pattern, optional directory and file_types.", absRoot),
		RiskLevel:   "low",
		Timeout:     5 * time.Second,
		Group:       "files",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"pattern"},
			"properties": map[string]any{
				"pattern":    map[string]any{"type": "string"},
				"directory":  map[string]any{"type": "string", "description": fmt.Sprintf("Subdirectory to search within workspace root (%s). Defaults to workspace root.", absRoot)},
				"file_types": map[string]any{"type": "string"},
			},
		},
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return searchContentTool(ctx, absRoot, guard, call, options)
	}))
	registry.RegisterDescriptor(Descriptor{
		Name:        "list_files",
		Description: fmt.Sprintf("List files and directories inside the workspace root (%s). Arguments: optional directory.", absRoot),
		RiskLevel:   "low",
		Timeout:     5 * time.Second,
		Group:       "files",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"directory": map[string]any{"type": "string", "description": fmt.Sprintf("Subdirectory to list within workspace root (%s). Defaults to workspace root.", absRoot)},
			},
		},
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return listFilesTool(ctx, absRoot, guard, call)
	}))
}

func readFileTool(ctx context.Context, root string, guard port.WorkspacePathGuard, call domain.ToolCall, options workspaceRegistryOptions) (domain.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	resolved, err := guard.Check(ctx, resolveToolPath(root, call.Arguments["path"]))
	if err != nil {
		return domain.ToolResult{}, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("read file: %w", err)
	}
	defer file.Close()
	// Cap how much of a file enters context. A read_file with no limit lets a
	// single huge file blow up the prompt; read one byte past the cap to detect
	// (and flag) truncation. Raised from 256KB to 512KB now that pagination
	// lets the model reach any part of that range via offset instead of only
	// ever seeing the front of the file.
	const maxReadFileBytes = 512 * 1024
	data, err := io.ReadAll(io.LimitReader(file, maxReadFileBytes+1))
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("read file: %w", err)
	}
	output := string(data)
	if len(data) > maxReadFileBytes {
		output = string(data[:maxReadFileBytes]) + fmt.Sprintf("\n…[truncated: file exceeds %d bytes]", maxReadFileBytes)
	}
	offset, err := intArg(call.Arguments, "offset", 0)
	if err != nil {
		return domain.ToolResult{}, err
	}
	limit, err := intArg(call.Arguments, "limit", readFilePageRunes)
	if err != nil {
		return domain.ToolResult{}, err
	}
	// Resolve the subtree agents.md note before slicing the page: it is the only
	// step left that can fail, and its length has to be charged against the same
	// budget as the page (see toolResultBudgetRunes) — an agents.md note can run
	// to MaxFileChars, which alone dwarfs the budget.
	note, err := subtreeAgentsNote(filepath.Dir(resolved), options)
	if err != nil {
		return domain.ToolResult{}, err
	}
	note = capReadFileNote(note)
	// fileContent is the pre-pagination text: the stable identity used for repeat
	// detection below, unaffected by page size or note presence.
	fileContent := output
	limit = readFilePageBudget(limit, output, call.Arguments["path"], len([]rune(note)))
	page, next, total, err := paginateRunes(output, offset, limit)
	if err != nil {
		return domain.ToolResult{}, err
	}
	output = page
	if next >= 0 {
		// Prepended, not appended: if anything downstream still overruns the
		// budget, truncation cuts the tail, so a leading hint survives and the
		// model can always see how to read the rest.
		output = fmt.Sprintf("…[已返回第 %d-%d 字，共 %d 字；继续读用 read_file(path=%q, offset=%d)]\n",
			offset+1, next, total, call.Arguments["path"], next) + output
	}
	// Repeat detection keys on (path, offset) and hashes the whole file, never the
	// rendered page. The page is not a stable identity: its size depends on the
	// agents.md note, which subtreeAgentsNote emits only the first time a
	// directory is seen in a task — so the same file at the same offset came back
	// as a bigger page on the second read and the repeat went undetected. Hashing
	// the file keeps re-reads detectable, while including offset in the key keeps
	// legitimate paging (same file, next offset) from being flagged as a repeat.
	if options.readHistory != nil {
		key := fmt.Sprintf("%s#%d", resolved, offset)
		if count, unchanged := options.readHistory.record(key, fileContent); unchanged {
			output = repeatNotice(count) + output
		}
	}
	output += note
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  output,
	}, nil
}

func writeFileTool(_ context.Context, root string, guard port.WorkspacePathGuard, call domain.ToolCall, options workspaceRegistryOptions) (domain.ToolResult, error) {
	resolved, err := guard.Check(context.Background(), resolveToolPath(root, call.Arguments["path"]))
	if err != nil {
		return domain.ToolResult{}, err
	}
	content := call.Arguments["content"]
	_, statErr := os.Stat(resolved)
	fileExists := statErr == nil
	if fileExists && call.Arguments["overwrite"] != "true" {
		return domain.ToolResult{}, fmt.Errorf("file already exists; set overwrite=true to replace it")
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return domain.ToolResult{}, fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return domain.ToolResult{}, fmt.Errorf("write file: %w", err)
	}
	rel, err := relativeToRoot(root, resolved)
	if err != nil {
		return domain.ToolResult{}, err
	}
	output := fmt.Sprintf("wrote %d bytes to %s", len(content), rel)
	if note, err := subtreeAgentsNote(filepath.Dir(resolved), options); err != nil {
		return domain.ToolResult{}, err
	} else if note != "" {
		output += note
	}
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  output,
	}, nil
}

// subtreeAgentsNote returns the on-demand agents.md injection appended to a
// write_file result. It walks the directory chain from options.projectRoot
// (exclusive — already resident) down to startDir (inclusive) via
// contextfiles.SubtreeAgentsChain, and renders every entry not yet seen by
// options.injected for this task, marking it seen as it goes so the same
// directory's conventions are not repeated on later write_file calls within
// the task. An entry already seen (resident or previously injected) is
// skipped. Injection is disabled — returning ("", nil) — when options.injected
// is nil or options.projectRoot is empty, which is the state before the
// registry's agents.md assembly wires them up; this also guards against
// calling markIfNew on a nil *injectedAgentsSet. A file flagged as unsafe
// yields a note saying it was ignored rather than its content.
func subtreeAgentsNote(startDir string, options workspaceRegistryOptions) (string, error) {
	if options.injected == nil || strings.TrimSpace(options.projectRoot) == "" {
		return "", nil
	}
	entries, err := contextfiles.SubtreeAgentsChain(options.projectRoot, startDir, options.maxFileChars)
	if err != nil {
		return "", fmt.Errorf("locate subtree agents.md for injection: %w", err)
	}
	var out strings.Builder
	for _, e := range entries {
		isNew, err := options.injected.markIfNew(e.Label)
		if err != nil {
			return "", err
		}
		if !isNew {
			continue
		}
		rel, relErr := filepath.Rel(options.projectRoot, e.Label)
		if relErr != nil {
			rel = e.Label
		}
		rel = filepath.ToSlash(rel)
		if e.Blocked {
			fmt.Fprintf(&out, "\n\n[本目录 agents.md 含不安全内容，已忽略: %s]", rel)
			continue
		}
		fmt.Fprintf(&out, "\n\n📁 本目录约定 (%s)：\n%s", rel, e.Content)
	}
	return out.String(), nil
}

func searchContentTool(ctx context.Context, rootPath string, guard port.WorkspacePathGuard, call domain.ToolCall, options workspaceRegistryOptions) (domain.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	pattern := strings.TrimSpace(call.Arguments["pattern"])
	if pattern == "" {
		return domain.ToolResult{}, fmt.Errorf("search_content pattern is required")
	}
	directory := call.Arguments["directory"]
	if strings.TrimSpace(directory) == "" {
		directory = "."
	}
	root, err := guard.Check(ctx, resolveToolPath(rootPath, directory))
	if err != nil {
		return domain.ToolResult{}, err
	}
	extensions := parseExtensions(call.Arguments["file_types"])
	var matches []string
	var notices []string
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A directory that cannot be walked makes the result incomplete.
			// Reporting it as a notice keeps the search usable while making the
			// gap visible; returning nil would present a partial answer as whole.
			notices = append(notices, fmt.Sprintf("skipped %s: %v", relativeToRootOrBase(root, path), walkErr))
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= searchContentMaxMatches {
			truncated = true
			return filepath.SkipAll
		}
		if len(extensions) > 0 && !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel := relativeToRootOrBase(root, path)
		info, err := entry.Info()
		if err != nil {
			notices = append(notices, fmt.Sprintf("skipped %s: %v", rel, err))
			return nil
		}
		// Cap per file. Without it a single large log read the whole workspace
		// into memory; read_file has enforced the same limit all along.
		if info.Size() > searchContentMaxFileBytes {
			notices = append(notices, fmt.Sprintf("skipped %s: file exceeds %d bytes", rel, searchContentMaxFileBytes))
			return nil
		}
		data, err := readFileContext(ctx, path, searchContentMaxFileBytes)
		if err != nil {
			// A cancelled context is the walk being torn down, not a property of
			// this file: abort the whole walk rather than filing it as a per-file
			// skip notice, which would read as "this file is broken".
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// Unreadable files are reported, not silently treated as "no match".
			notices = append(notices, fmt.Sprintf("skipped %s: %v", rel, err))
			return nil
		}
		text := string(data)
		if !strings.Contains(strings.ToLower(text), strings.ToLower(pattern)) {
			return nil
		}
		matches = append(matches, rel+": "+firstMatchingLine(text, pattern))
		return nil
	})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("search content: %w", err)
	}
	if len(matches) == 0 {
		matches = append(matches, "no matches")
	}
	if truncated {
		matches = append(matches, fmt.Sprintf("…[truncated: more than %d matches; narrow the pattern, directory or file_types]", searchContentMaxMatches))
	}
	matches = append(matches, notices...)
	output := strings.Join(matches, "\n")
	if note, err := subtreeAgentsNote(root, options); err != nil {
		return domain.ToolResult{}, err
	} else if note != "" {
		output += note
	}
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  output,
	}, nil
}

// readFileContext reads up to limit bytes of path, checking ctx between
// chunks.
//
// os.ReadFile takes no context, so a cancelled or timed-out search still had to
// wait out whatever file it was on. The per-file cap bounds that wait, but
// bounded is not responsive: the walk checks ctx per directory entry and then
// blocks inside a read it cannot interrupt. Reading in chunks puts the
// cancellation check back on the inside of that read.
//
// A file longer than limit is truncated to limit rather than reported as an
// error: callers apply their own size policy before getting here (search_content
// skips oversized files loudly), and limit is the memory backstop.
func readFileContext(ctx context.Context, path string, limit int64) (data []byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Named returns so a Close failure cannot be lost behind a successful read.
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			data, err = nil, closeErr
		}
	}()

	var buf bytes.Buffer
	chunk := make([]byte, searchContentReadChunk)
	for int64(buf.Len()) < limit {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		remaining := limit - int64(buf.Len())
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}
		n, readErr := file.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return buf.Bytes(), err
}

// relativeToRoot renders path relative to root, or fails.
//
// It is the fail-loud counterpart to relativeToRootOrBase below, and the two are
// not interchangeable. That one produces a label for a notice, where degrading to
// a base name costs nothing. This one produces the destination write_file reports
// back, and its callers hand root a path the sandbox has already resolved and
// approved — so Rel failing here means the sandbox's idea of the path and the
// real one disagree. Papering over that with "" makes the tool answer
// "wrote 1234 bytes to ", which the model reads as a successful write to an empty
// path and then cannot find again.
func relativeToRoot(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("resolve %q relative to workspace root: %w", filepath.Base(path), err)
	}
	return rel, nil
}

// relativeToRootOrBase renders path relative to root, falling back to the base
// name. The fallback deliberately avoids the absolute path: these strings go
// into tool output, and an absolute path would disclose filesystem layout
// outside the sandbox.
func relativeToRootOrBase(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return rel
}

func listFilesTool(ctx context.Context, rootPath string, guard port.WorkspacePathGuard, call domain.ToolCall) (domain.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	directory := call.Arguments["directory"]
	if strings.TrimSpace(directory) == "" {
		directory = "."
	}
	root, err := guard.Check(ctx, resolveToolPath(rootPath, directory))
	if err != nil {
		return domain.ToolResult{}, err
	}
	// A non-existent target is the common cause of a confusing empty listing: the
	// model guessed a path (e.g. a "packages/*" monorepo layout that isn't there)
	// and WalkDir returned only a cryptic OS "cannot find the path" notice. Detect
	// it up front and return actionable guidance so the model lists the real root
	// instead of retrying more invented paths.
	if info, statErr := os.Stat(root); os.IsNotExist(statErr) {
		return domain.ToolResult{
			CallID:  call.ID,
			Success: false,
			Error: fmt.Sprintf("directory %q does not exist under the workspace root; "+
				"call list_files with no directory (or directory=\".\") to see the actual layout first", directory),
		}, nil
	} else if statErr != nil {
		return domain.ToolResult{}, fmt.Errorf("list files: stat %q: %w", directory, statErr)
	} else if !info.IsDir() {
		return domain.ToolResult{
			CallID:  call.ID,
			Success: false,
			Error:   fmt.Sprintf("%q is a file, not a directory; use read_file to read it", directory),
		}, nil
	}
	var entries []string
	var notices []string
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			notices = append(notices, fmt.Sprintf("skipped %s: %v", relativeToRootOrBase(root, path), walkErr))
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) && path != root {
				return filepath.SkipDir
			}
			if path == root {
				return nil
			}
		}
		if len(entries) >= listFilesMaxEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel := relativeToRootOrBase(root, path)
		if entry.IsDir() {
			rel += string(filepath.Separator)
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("list files: %w", err)
	}
	if len(entries) == 0 {
		entries = append(entries, "no files")
	}
	if truncated {
		entries = append(entries, fmt.Sprintf("…[truncated: more than %d entries; narrow the directory]", listFilesMaxEntries))
	}
	entries = append(entries, notices...)
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  strings.Join(entries, "\n"),
	}, nil
}

func resolveToolPath(root string, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "tmp", "vendor":
		return true
	default:
		return false
	}
}

func parseExtensions(raw string) map[string]bool {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' '
	})
	extensions := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, ".") {
			part = "." + part
		}
		extensions[part] = true
	}
	return extensions
}

func firstMatchingLine(text string, pattern string) string {
	lowerPattern := strings.ToLower(pattern)
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(strings.ToLower(line), lowerPattern) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
