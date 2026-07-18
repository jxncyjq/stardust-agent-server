package sessionstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// CheckpointSchemaVersion versions the on-disk checkpoint format. Load rejects a
// checkpoint whose version it does not recognise (fail-loud) rather than
// half-decoding a future/older layout and resuming a task from wrong state.
const CheckpointSchemaVersion = 2

// checkpointFileName is the single per-session checkpoint file, per design §4.0.
const checkpointFileName = "task-state.json"

// ToolEntrySnapshot is the serialisable form of the runtime's internal toolEntry
// (whose fields are unexported). It preserves the dedup key and rendered text so
// a resumed loop rebuilds identical accumulated tool context.
type ToolEntrySnapshot struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// Checkpoint is the serialised mid-flight state of a suspended tool loop: enough
// to re-enter RunTask and finish deterministically. It is written at a tool-round
// boundary — PendingCalls are the tool calls not yet executed when the runtime
// suspended.
type Checkpoint struct {
	SchemaVersion int    `json:"schema_version"`
	TaskID        string `json:"task_id"`
	AgentID       string `json:"agent_id"`
	SessionKey    string `json:"session_key"`
	// Mode is the task's working mode (manual|plan|auto) captured at suspend time,
	// so a resumed run re-applies the same gating (e.g. Manual still gates sensitive
	// tools) instead of losing it and executing side effects unguarded.
	Mode             string              `json:"mode,omitempty"`
	BasePrompt       string              `json:"base_prompt"`
	Round            int                 `json:"round"`
	ToolEntries      []ToolEntrySnapshot `json:"tool_entries"`
	PendingCalls     []domain.ToolCall   `json:"pending_calls"`
	PromptTokens     int                 `json:"prompt_tokens"`
	CompletionTokens int                 `json:"completion_tokens"`
	CachedTokens     int                 `json:"cached_tokens"`
	TotalTokens      int                 `json:"total_tokens"`
	Images           []string            `json:"images,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
}

// Store persists task checkpoints under a base directory, one file per session
// (SessionDir(base, key)/task-state.json).
type Store struct {
	base string
}

// NewStore returns a checkpoint store rooted at base (the resolved workspace
// root, or <working_dir>/.stardust once working_dir lands).
func NewStore(base string) *Store {
	return &Store{base: base}
}

// Save writes the checkpoint atomically (temp file + rename) so a crash mid-write
// never leaves a half-written task-state.json that Load would reject.
func (s *Store) Save(cp Checkpoint) error {
	if cp.SessionKey == "" {
		return errors.New("save checkpoint: empty SessionKey")
	}
	dir := SessionDir(s.base, cp.SessionKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create session dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint %s: %w", cp.SessionKey, err)
	}
	final := filepath.Join(dir, checkpointFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint tmp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename checkpoint %q: %w", final, err)
	}
	return nil
}

// Load reads the checkpoint for sessionKey. Absence is legitimate (fresh task):
// it returns (zero, false, nil). Any other fault — unreadable file, corrupt JSON,
// or an unrecognised schema version — returns a fail-loud error rather than
// pretending the task has no prior state.
func (s *Store) Load(sessionKey string) (Checkpoint, bool, error) {
	path := filepath.Join(SessionDir(s.base, sessionKey), checkpointFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("read checkpoint %q: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, false, fmt.Errorf("decode checkpoint %q: %w", path, err)
	}
	if cp.SchemaVersion != CheckpointSchemaVersion {
		return Checkpoint{}, false, fmt.Errorf("checkpoint %q schema version %d unsupported (want %d)", path, cp.SchemaVersion, CheckpointSchemaVersion)
	}
	return cp, true, nil
}

// Delete removes a session's checkpoint. A missing file is not an error (delete
// is idempotent — a completed or already-cleaned task is the normal case).
func (s *Store) Delete(sessionKey string) error {
	path := filepath.Join(SessionDir(s.base, sessionKey), checkpointFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove checkpoint %q: %w", path, err)
	}
	return nil
}

// WritePlan writes a Plan-mode artifact to <base>/session/<sessionKey>/plans/<filename>,
// creating the directory. It returns the absolute path written. An empty
// sessionKey or filename is rejected (fail-loud — never write to a malformed
// path). This is where an OKF "one concept, one file" plan lands (design §4.2).
func (s *Store) WritePlan(sessionKey, filename, content string) (string, error) {
	if sessionKey == "" || filename == "" {
		return "", fmt.Errorf("write plan: empty sessionKey or filename (key=%q file=%q)", sessionKey, filename)
	}
	dir := filepath.Join(SessionDir(s.base, sessionKey), "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create plans dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write plan %q: %w", path, err)
	}
	return path, nil
}

// ListSuspended loads every checkpoint under <base>/session/*/task-state.json.
// A missing base dir yields an empty slice (no sessions yet). A corrupt or
// version-mismatched checkpoint fails loud — recovery must not silently skip a
// task it cannot restore.
func (s *Store) ListSuspended() ([]Checkpoint, error) {
	sessionsRoot := filepath.Join(s.base, "session")
	entries, err := os.ReadDir(sessionsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions root %q: %w", sessionsRoot, err)
	}
	var out []Checkpoint
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cp, ok, err := s.Load(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("load suspended checkpoint for %q: %w", entry.Name(), err)
		}
		if !ok {
			continue // session dir without a checkpoint (e.g. only plans/) — legitimate
		}
		out = append(out, cp)
	}
	return out, nil
}
