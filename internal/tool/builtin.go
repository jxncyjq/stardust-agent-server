package tool

import (
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
		absRoot = root
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
	registry.RegisterDescriptor(Descriptor{
		Name:        "write_file",
		Description: fmt.Sprintf("Write content to a file inside the workspace root (%s). Arguments: path, content, optional overwrite (default false). Fails if the file exists and overwrite is not true.", absRoot),
		RiskLevel:   "medium",
		Timeout:     5 * time.Second,
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
	return registry
}

func NewReadOnlyWorkspaceRegistry(root string, audit port.AuditLog) *Registry {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
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

// registerReadOnlyDescriptors adds read_file, search_content, and list_files
// to an already-constructed registry. Shared by both workspace registry constructors.
func registerReadOnlyDescriptors(registry *Registry, absRoot string, guard port.WorkspacePathGuard) {
	registry.RegisterDescriptor(Descriptor{
		Name:        "read_file",
		Description: fmt.Sprintf("Read a UTF-8 text file inside the workspace root (%s). The path argument can be relative (resolved against workspace root) or absolute (must be within workspace root).", absRoot),
		RiskLevel:   "low",
		Timeout:     2 * time.Second,
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
	rel, _ := filepath.Rel(root, resolved)
	output := fmt.Sprintf("wrote %d bytes to %s", len(content), rel)
	if note, err := nearestAgentsNote(root, filepath.Dir(resolved), options); err != nil {
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
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
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
		if len(extensions) > 0 && !extensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		if !strings.Contains(strings.ToLower(text), strings.ToLower(pattern)) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		line := firstMatchingLine(text, pattern)
		matches = append(matches, rel+": "+line)
		return nil
	})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("search content: %w", err)
	}
	if len(matches) == 0 {
		matches = append(matches, "no matches")
	}
	return domain.ToolResult{
		CallID:  call.ID,
		Success: true,
		Output:  strings.Join(matches, "\n"),
	}, nil
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
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
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
