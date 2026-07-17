package contextfiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Config holds the parameters for loading context files into an inference
// context block. The three AGENTS.md locations (global, workspace, workspace
// .stardust) are always derived from Root and the user's home directory; they
// are not configurable per-path — any present file at those fixed locations is
// loaded automatically.
type Config struct {
	Enabled      bool
	Root         string
	SoulPath     string
	ToolsPath    string
	UserPath     string
	MemoryPath   string
	MaxFileChars int
}

// Block carries the loaded content for every resident context section. The
// three AGENTS.md slots (GlobalAgents, WorkspaceAgents, StardustAgents) are
// loaded from the three fixed locations described in Load.
type Block struct {
	Soul            string
	GlobalAgents    string // ~/.stardust/agents.md (or AGENTS.md)
	WorkspaceAgents string // <root>/agents.md (or AGENTS.md)
	StardustAgents  string // <root>/.stardust/agents.md (or AGENTS.md)
	Tools           string
	User            string
	Memory          string
	Blocked         []string
}

// Load reads the resident context files from cfg and returns a populated Block.
// Three AGENTS.md locations are always checked (each may be absent without
// error):
//  1. Global:             <homeDir>/.stardust/agents.md  (or AGENTS.md)
//  2. Workspace:          <root>/agents.md               (or AGENTS.md)
//  3. Workspace .stardust: <root>/.stardust/agents.md    (or AGENTS.md)
//
// At every location the filename is resolved by trying "agents.md" first, then
// "AGENTS.md" as a fallback, so both casings are accepted. The global location
// is outside the workspace sandbox and is explicitly trusted (it is the user's
// own configuration); it therefore skips the workspace-root boundary check but
// still passes through the prompt-injection scan and size truncation.
func Load(ctx context.Context, cfg Config) (Block, error) {
	if err := ctx.Err(); err != nil {
		return Block{}, err
	}
	if !cfg.Enabled {
		return Block{}, nil
	}
	if cfg.Root == "" {
		cfg.Root = "."
	}
	if cfg.MaxFileChars <= 0 {
		cfg.MaxFileChars = 20000
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return Block{}, fmt.Errorf("resolve context root: %w", err)
	}
	root = filepath.Clean(root)

	var block Block

	// ── 1. Soul / persona ──────────────────────────────────────────────────
	if block.Soul, err = loadOne(root, cfg.SoulPath, "SOUL.md", cfg.MaxFileChars, &block); err != nil {
		return Block{}, err
	}

	// ── 2. Three fixed AGENTS.md locations (always resident) ───────────────

	// 2a. Global: ~/.stardust/agents.md — sandbox-exempt, still scanned.
	homeDir, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return Block{}, fmt.Errorf("resolve home directory for global agents: %w", homeErr)
	}
	globalAgentsPath := findAgentsFile(filepath.Join(homeDir, ".stardust"))
	if globalAgentsPath != "" {
		if block.GlobalAgents, err = readGlobal(globalAgentsPath, "~/.stardust/agents.md", cfg.MaxFileChars, &block); err != nil {
			return Block{}, err
		}
	}

	// 2b. Workspace: <root>/agents.md
	wsAgentsPath := findAgentsFile(root)
	if wsAgentsPath != "" {
		if block.WorkspaceAgents, err = loadOneFull(root, wsAgentsPath, "agents.md", cfg.MaxFileChars, &block); err != nil {
			return Block{}, err
		}
	}

	// 2c. Workspace .stardust: <root>/.stardust/agents.md
	wsStardustAgentsPath := findAgentsFile(filepath.Join(root, ".stardust"))
	if wsStardustAgentsPath != "" {
		if block.StardustAgents, err = loadOneFull(root, wsStardustAgentsPath, ".stardust/agents.md", cfg.MaxFileChars, &block); err != nil {
			return Block{}, err
		}
	}

	// ── 3. Remaining persona / tool / user / memory files ─────────────────
	if block.Tools, err = loadOne(root, cfg.ToolsPath, "TOOLS.md", cfg.MaxFileChars, &block); err != nil {
		return Block{}, err
	}
	if block.User, err = loadOne(root, cfg.UserPath, "USER.md", cfg.MaxFileChars, &block); err != nil {
		return Block{}, err
	}
	if block.Memory, err = loadOne(root, cfg.MemoryPath, "MEMORY.md", cfg.MaxFileChars, &block); err != nil {
		return Block{}, err
	}
	return block, nil
}

// Render serialises the block into a human/model-readable string with labelled
// sections. Sections whose content is empty are omitted.
func (b Block) Render() string {
	var out strings.Builder
	writeSection(&out, "Agent identity (SOUL.md)", b.Soul)
	writeSection(&out, "Global instructions (~/.stardust/agents.md)", b.GlobalAgents)
	writeSection(&out, "Workspace instructions (agents.md)", b.WorkspaceAgents)
	writeSection(&out, "Workspace instructions (.stardust/agents.md)", b.StardustAgents)
	writeSection(&out, "Tool policy (TOOLS.md)", b.Tools)
	writeSection(&out, "User profile (USER.md)", b.User)
	writeSection(&out, "Agent memory (MEMORY.md)", b.Memory)
	for _, blocked := range b.Blocked {
		writeSection(&out, "Blocked context file", blocked)
	}
	return strings.TrimSpace(out.String())
}

// ResidentAgentsPaths returns the set of absolute paths that are permanently
// loaded into context (the three fixed AGENTS.md locations). write_file uses
// this to avoid re-injecting a file that the model already sees in every prompt.
// root must be the absolute, cleaned workspace root; homeDir is the user home
// directory (passed in so callers don't need to call os.UserHomeDir twice).
func ResidentAgentsPaths(root string, homeDir string) map[string]bool {
	set := make(map[string]bool, 3)
	for _, dir := range []string{
		filepath.Join(homeDir, ".stardust"),
		root,
		filepath.Join(root, ".stardust"),
	} {
		p := findAgentsFile(dir)
		if p != "" {
			set[filepath.Clean(p)] = true
		}
	}
	return set
}

// findAgentsFile returns the absolute path of the first agents file found in
// dir, trying "agents.md" before "AGENTS.md". The comparison is done against
// the actual on-disk entry names (via os.ReadDir) so that the returned path
// carries the true filename even on case-insensitive filesystems (e.g.
// Windows). Returns "" when neither name exists in the directory.
func findAgentsFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// Build a lookup from actual on-disk name → full path.
	actual := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			actual[e.Name()] = filepath.Join(dir, e.Name())
		}
	}
	for _, want := range []string{"agents.md", "AGENTS.md"} {
		if p, ok := actual[want]; ok {
			return p
		}
	}
	return ""
}

// loadOne resolves rel against root, enforces the workspace sandbox, then
// delegates to readOne. A missing file is silently skipped.
func loadOne(root string, rel string, label string, maxChars int, block *Block) (string, error) {
	if rel == "" {
		return "", nil
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, rel)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	abs = filepath.Clean(abs)
	content, blocked, err := readOne(root, abs, label, maxChars)
	if err != nil {
		return "", err
	}
	if blocked {
		block.Blocked = append(block.Blocked, fmt.Sprintf("[blocked: %s contained unsafe context pattern]", label))
		return "", nil
	}
	return content, nil
}

// loadOneFull is like loadOne but takes an already-resolved absolute path
// (used for the fixed AGENTS.md locations where the path is determined by
// findAgentsFile before the call).
func loadOneFull(root string, absPath string, label string, maxChars int, block *Block) (string, error) {
	content, blocked, err := readOne(root, absPath, label, maxChars)
	if err != nil {
		return "", err
	}
	if blocked {
		block.Blocked = append(block.Blocked, fmt.Sprintf("[blocked: %s contained unsafe context pattern]", label))
		return "", nil
	}
	return content, nil
}

// readGlobal reads the global agents file (e.g. ~/.stardust/agents.md). It
// skips the workspace sandbox check (the file is outside the workspace root by
// design) but still applies the prompt-injection scan and size truncation.
func readGlobal(absPath string, label string, maxChars int, block *Block) (string, error) {
	data, err := os.ReadFile(absPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", nil
	}
	if isUnsafeContext(trimmed) {
		block.Blocked = append(block.Blocked, fmt.Sprintf("[blocked: %s contained unsafe context pattern]", label))
		return "", nil
	}
	if maxChars <= 0 {
		maxChars = 20000
	}
	return truncate(trimmed, label, maxChars), nil
}

// readOne reads a single context file at absPath after enforcing the workspace
// sandbox (absPath must be within root), then trims, scans for prompt-injection
// patterns, and truncates to maxChars. It returns blocked=true when the file
// exists but contains an unsafe context pattern (content is then empty). A
// missing file is not an error: it returns ("", false, nil). label is used only
// for error and truncation messages.
func readOne(root string, absPath string, label string, maxChars int) (content string, blocked bool, err error) {
	if !isWithinRoot(root, absPath) {
		return "", false, fmt.Errorf("%s path outside context root: %s", label, absPath)
	}
	data, err := os.ReadFile(absPath)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", label, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", false, nil
	}
	if isUnsafeContext(trimmed) {
		return "", true, nil
	}
	if maxChars <= 0 {
		maxChars = 20000
	}
	return truncate(trimmed, label, maxChars), false, nil
}

// LoadAgentsFile reads an agents.md / AGENTS.md file at absPath, enforcing the
// workspace sandbox, prompt-injection scan, and size truncation. absPath must
// already be absolute and within root. It returns blocked=true when the file
// exists but contains an unsafe pattern (content is then empty), and
// ("", false, nil) when the file does not exist. This is the reusable
// single-file reader shared by the resident loader and the on-demand write_file
// injection path.
func LoadAgentsFile(root string, absPath string, maxChars int) (content string, blocked bool, err error) {
	return readOne(root, absPath, "agents.md", maxChars)
}

// NearestAgentsFile walks upward from startDir to root (inclusive) and returns
// the first agents file found (trying "agents.md" before "AGENTS.md" at each
// level). found reports whether any file existed in range. absPath is the
// absolute path of the located file (empty when found is false). The walk never
// escapes root: startDir must be within root. A missing file at every level
// yields (found=false) without error; an unsafe file yields
// (found=true, blocked=true, content="").
func NearestAgentsFile(root string, startDir string, maxChars int) (content string, absPath string, found bool, blocked bool, err error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", false, false, fmt.Errorf("resolve context root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", "", false, false, fmt.Errorf("resolve agents.md start dir: %w", err)
	}
	dir = filepath.Clean(dir)
	if !isWithinRoot(absRoot, dir) {
		return "", "", false, false, fmt.Errorf("agents.md start dir outside context root: %s", dir)
	}
	for {
		candidate := findAgentsFile(dir)
		if candidate != "" {
			body, isBlocked, readErr := readOne(absRoot, candidate, "agents.md", maxChars)
			if readErr != nil {
				return "", "", false, false, readErr
			}
			if isBlocked {
				return "", candidate, true, true, nil
			}
			if body != "" {
				return body, candidate, true, false, nil
			}
		}
		if dir == absRoot {
			return "", "", false, false, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false, false, nil
		}
		dir = parent
	}
}

func isWithinRoot(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func isUnsafeContext(content string) bool {
	lower := strings.ToLower(content)
	patterns := []string{
		"ignore all previous instructions",
		"ignore previous instructions",
		"forget all instructions",
		"print secrets",
		"exfiltrate",
		"reveal your system prompt",
		"api_key=",
		"password=",
		"private key",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return strings.ContainsRune(content, '‮') || !utf8.ValidString(content)
}

func truncate(content string, label string, maxChars int) string {
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}
	head := maxChars * 7 / 10
	tail := maxChars * 2 / 10
	if head <= 0 || tail <= 0 || head+tail >= len(runes) {
		return string(runes[:maxChars])
	}
	return string(runes[:head]) +
		fmt.Sprintf("\n\n[...truncated %s: kept %d+%d of %d chars...]\n\n", label, head, tail, len(runes)) +
		string(runes[len(runes)-tail:])
}

func writeSection(out *strings.Builder, title string, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	out.WriteString("## ")
	out.WriteString(title)
	out.WriteString("\n\n")
	out.WriteString(content)
	out.WriteString("\n\n")
}
