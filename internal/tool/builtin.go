package tool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
)

// WorkspaceRegistryOption configures optional behavior of a workspace registry
// built by NewWorkspaceRegistry, such as the per-directory agents.md injection
// that write_file performs after a successful write.
type WorkspaceRegistryOption func(*workspaceRegistryOptions)

type workspaceRegistryOptions struct {
	maxFileChars int
	homeDir      string // user home directory for resident-path exclusion
}

// WithAgentsInjection enables write_file to append the nearest subdirectory
// agents.md / AGENTS.md (the local directory conventions) to its result after
// writing a file. maxFileChars caps the injected content. homeDir is the user
// home directory (used to compute the resident ~/.stardust/agents.md path so it
// is never re-injected). An empty homeDir degrades gracefully: only the two
// workspace-local resident paths are excluded.
func WithAgentsInjection(maxFileChars int, homeDir string) WorkspaceRegistryOption {
	return func(o *workspaceRegistryOptions) {
		o.maxFileChars = maxFileChars
		o.homeDir = homeDir
	}
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
			"fetch_url", "session_search", "delegate_task", "moa_consult",
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
			"developer:session_search":      true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	registerReadOnlyDescriptors(registry, absRoot, guard)
	registerWriteFileDescriptor(registry, absRoot, guard, true, options)
	return registry
}

// registerWriteFileDescriptor adds the write_file tool to an already-constructed
// registry. write_file requires overwrite=true when the target already exists,
// and its filesystem access is bounded by guard. injectAgentsNote controls
// whether a successful write appends the nearest directory's agents.md
// conventions to the result — an interactive-CLI UX feature that serve and
// per-agent tasks turn off. The registry's execution policy and permission
// enforcer must already allow write_file; this only registers the descriptor.
func registerWriteFileDescriptor(registry *Registry, absRoot string, guard port.WorkspacePathGuard, injectAgentsNote bool, options workspaceRegistryOptions) {
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
		return writeFileTool(ctx, absRoot, guard, call, injectAgentsNote, options)
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
			"fetch_url", "session_search", "delegate_task", "moa_consult",
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
			"developer:session_search":      true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	registerReadOnlyDescriptors(registry, absRoot, guard)
	return registry
}

// NewFileReadWriteWorkspaceRegistry returns a registry with the read-only file
// tools plus write_file, all sandboxed to root. It is the serve / per-agent
// counterpart to NewWorkspaceRegistry: same write capability, but without the
// interactive-CLI agents.md injection on write (a directory's agents.md must not
// leak into a server task's tool results). write_file stays Sensitive, so Manual
// mode still gates it and Plan mode still excludes it. Callers add task-ledger,
// messaging and web tools afterwards, exactly as with the read-only constructor.
func NewFileReadWriteWorkspaceRegistry(root string, audit port.AuditLog) *Registry {
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
			"read_file", "search_content", "list_files", "write_file",
			"create_task", "claim_task", "update_task", "append_task_message", "read_task", "rebuild_tasks",
			"send_message", "read_messages",
			"fetch_url", "session_search", "delegate_task", "moa_consult",
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
			"developer:session_search":      true,
		}, nil),
		NoopGuardrails{},
	).WithAuditLog(audit).WithOutputSanitizer(quality.NewOutputSanitizer())
	registerReadOnlyDescriptors(registry, absRoot, guard)
	registerWriteFileDescriptor(registry, absRoot, guard, false, workspaceRegistryOptions{})
	return registry
}

// registerReadOnlyDescriptors adds read_file, search_content, and list_files
// to an already-constructed registry. Shared by both workspace registry constructors.
func registerReadOnlyDescriptors(registry *Registry, absRoot string, guard port.WorkspacePathGuard) {
	registry.RegisterDescriptor(Descriptor{
		Name:        "read_file",
		Description: fmt.Sprintf("Read a UTF-8 text file inside the workspace root (%s). The path argument can be relative (resolved against workspace root) or absolute (must be within workspace root).", absRoot),
		RiskLevel:   "low",
		Timeout:     2 * time.Second,
		Group:       "files",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": fmt.Sprintf("File path relative to workspace root (%s) or absolute path within workspace root.", absRoot)},
			},
		},
	}, HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		return readFileTool(ctx, absRoot, guard, call)
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
		return searchContentTool(ctx, absRoot, guard, call)
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

func readFileTool(ctx context.Context, root string, guard port.WorkspacePathGuard, call domain.ToolCall) (domain.ToolResult, error) {
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
	// (and flag) truncation.
	const maxReadFileBytes = 256 * 1024
	data, err := io.ReadAll(io.LimitReader(file, maxReadFileBytes+1))
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("read file: %w", err)
	}
	output := string(data)
	if len(data) > maxReadFileBytes {
		output = string(data[:maxReadFileBytes]) + fmt.Sprintf("\n…[truncated: file exceeds %d bytes]", maxReadFileBytes)
	}
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  output,
	}, nil
}

func writeFileTool(_ context.Context, root string, guard port.WorkspacePathGuard, call domain.ToolCall, injectAgentsNote bool, options workspaceRegistryOptions) (domain.ToolResult, error) {
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
	// The agents.md note is an interactive-CLI convenience. serve / per-agent
	// tasks disable it (injectAgentsNote=false) so a directory's agents.md does
	// not leak into their tool results.
	if injectAgentsNote {
		if note, err := nearestAgentsNote(root, filepath.Dir(resolved), options); err != nil {
			return domain.ToolResult{}, err
		} else if note != "" {
			output += note
		}
	}
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  output,
	}, nil
}

// nearestAgentsNote returns the on-demand agents.md injection appended to a
// write_file result. It locates the nearest agents.md / AGENTS.md walking up
// from startDir to root, skipping the three resident locations (global
// ~/.stardust/agents.md, workspace agents.md, workspace .stardust/agents.md —
// those are always in context), and renders the file's local-directory
// conventions. An empty string means there is nothing to inject (no nearby file,
// or the only match is a resident file). A file flagged as unsafe yields a note
// saying it was ignored rather than its content.
func nearestAgentsNote(root string, startDir string, options workspaceRegistryOptions) (string, error) {
	content, absPath, found, blocked, err := contextfiles.NearestAgentsFile(root, startDir, options.maxFileChars)
	if err != nil {
		return "", fmt.Errorf("locate nearest agents.md for write_file injection: %w", err)
	}
	if !found {
		return "", nil
	}
	if isResidentAgents(root, absPath, options.homeDir) {
		return "", nil
	}
	rel, relErr := filepath.Rel(root, absPath)
	if relErr != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)
	if blocked {
		return fmt.Sprintf("\n\n[本目录 agents.md 含不安全内容，已忽略: %s]", rel), nil
	}
	return fmt.Sprintf("\n\n📁 本目录约定 (%s)：\n%s", rel, content), nil
}

// isResidentAgents reports whether absPath is one of the three agents.md files
// that are loaded permanently into context, which must not be re-injected by
// write_file. The three resident paths are:
//  1. <homeDir>/.stardust/agents.md  (or AGENTS.md)
//  2. <root>/agents.md               (or AGENTS.md)
//  3. <root>/.stardust/agents.md     (or AGENTS.md)
//
// homeDir may be empty, in which case only the two workspace-local paths are
// excluded (graceful degradation).
func isResidentAgents(root string, absPath string, homeDir string) bool {
	residents := contextfiles.ResidentAgentsPaths(root, homeDir)
	return residents[filepath.Clean(absPath)]
}

func searchContentTool(ctx context.Context, rootPath string, guard port.WorkspacePathGuard, call domain.ToolCall) (domain.ToolResult, error) {
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
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  strings.Join(matches, "\n"),
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
