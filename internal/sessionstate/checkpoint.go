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
//
// v3 adds the Loaded field (capability catalog entries pinned into the run's
// loaded block). A v3 checkpoint that happens to carry no "loaded" key still
// decodes fine — Loaded simply comes back nil, which is the legitimate state
// for a run that never called load_capabilities. That is a JSON-decoding
// nicety, not a schema-version concession: a checkpoint tagged with an older
// SchemaVersion is still rejected outright by the check below, same as before.
// v4 replaces ToolEntries with Messages, the append-only multi-turn exchange
// (user / assistant-with-tool_calls / tool-result) the runtime now sends. A v3
// checkpoint cannot be upgraded: its tool context was collapsed by
// (name, arguments) and the assistant turns were never recorded at all, so the
// conversation it describes cannot be reconstructed. Load rejects it outright
// rather than resuming a run from a history that never existed.
const CheckpointSchemaVersion = 4

// checkpointFileName is the single per-session checkpoint file, per design §4.0.
const checkpointFileName = "task-state.json"

// MessageSnapshot is the serialisable form of one conversation turn (the
// runtime's internal conversation has unexported fields). Role decides which
// fields carry meaning, matching port.InferenceMessage: Images on user,
// ToolCalls on assistant, ToolCallID on tool.
type MessageSnapshot struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Images     []string          `json:"images,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
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
	Mode       string `json:"mode,omitempty"`
	BasePrompt string `json:"base_prompt"`
	Round      int    `json:"round"`
	// Messages is the exchange as it stood when the run suspended. Restoring it
	// verbatim is what lets a resumed run keep seeing the calls it already made.
	Messages []MessageSnapshot `json:"messages"`
	// TaskStart 是本任务自己的消息在 Messages 里的起点：G3 打开时会话历史以
	// transcript 排在 message[0] 之后，这个下标就落在历史之后；关闭时它是 1。
	//
	// 存它是因为重复调用熔断按它决定扫描起点（runtime.conversation.taskStart）。
	// 不存的话，续跑的任务会把历史算进「连续重复的轮次」，一条正常的会话就足以
	// 触发重复警告。
	//
	// 0 表示**字段缺席**。这个编码没有歧义，靠的是「0 从来不是合法的 taskStart」：
	// 四个写入方给出的值分别是 1（newConversation）、>=1（appendHistory）、
	// >=2（applyCompaction）、>=1（restoreConversation）。omitempty 因此不会把一个
	// 合法值编码成缺席。若将来有人让 taskStart 合法地取 0，这套编码会静默崩塌。
	//
	// 缺席的检查点按 1 处理。**这不等于「那份检查点里没有历史段」**：G3 的 transcript
	// 注入早于本字段存在，所以由旧二进制在 G3 打开时写下的检查点，Messages 里带着
	// 历史而 task_start 缺席，边界已不可恢复——续跑那一次仍可能把历史算进 streak。
	// 这是升级窗口的已知代价，只影响升级瞬间已挂起的任务。
	//
	// 为什么不 bump CheckpointSchemaVersion（本文件 v2→v3 仅因新增 Loaded 字段就
	// bump 过，本次偏离了那个惯例）：Load 对版本是严格相等比较，bump 会让升级瞬间
	// **所有**挂起中的检查点直接失效，包括等待人工审批的任务——代价大于上面那一次
	// 误报。取舍写在这里，不是漏了。
	TaskStart        int               `json:"task_start,omitempty"`
	PendingCalls     []domain.ToolCall `json:"pending_calls"`
	PromptTokens     int               `json:"prompt_tokens"`
	CompletionTokens int               `json:"completion_tokens"`
	CachedTokens     int               `json:"cached_tokens"`
	TotalTokens      int               `json:"total_tokens"`
	Images           []string          `json:"images,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	// WorkingDir captures the task's working_dir at suspend time, so a resumed
	// run resolves the same session base (SessionBase(workspaceRoot, WorkingDir))
	// to locate this checkpoint's session directory rather than defaulting back
	// to the workspace root.
	WorkingDir string `json:"working_dir,omitempty"`
	// Loaded carries the capabilities whose full definitions the model pulled
	// during this run (via load_capabilities), so a resumed task does not have
	// to rediscover and reload them. An empty/absent Loaded is legitimate: a
	// fresh task, one that never called load_capabilities, or a checkpoint
	// written before this field existed all restore to no loaded capabilities,
	// and the model can simply load them again if it needs to.
	Loaded []LoadedCapability `json:"loaded,omitempty"`
}

// LoadedCapability is one entry of the loaded block, persisted verbatim so a
// resumed run's prompt can re-render the exact same "Loaded capabilities:"
// section the suspended run had, without re-querying the capability catalog.
type LoadedCapability struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Store persists task checkpoints under a base directory, one file per session
// (SessionDir(base, key)/task-state.json). base is resolved per call via
// SessionBase(workspaceRoot, workingDir): workspaceRoot is used when working_dir
// is empty.
type Store struct {
	workspaceRoot string
}

// NewStore returns a checkpoint store rooted at workspaceRoot, the base used
// when a checkpoint carries no working_dir.
func NewStore(workspaceRoot string) *Store {
	return &Store{workspaceRoot: workspaceRoot}
}

// Save writes the checkpoint atomically (temp file + rename) so a crash mid-write
// never leaves a half-written task-state.json that Load would reject. It is
// stored under SessionBase(s.workspaceRoot, cp.WorkingDir), so a session bound
// to a working_dir persists alongside that directory rather than the workspace
// root.
func (s *Store) Save(cp Checkpoint) error {
	if cp.SessionKey == "" {
		return errors.New("save checkpoint: empty SessionKey")
	}
	base := SessionBase(s.workspaceRoot, cp.WorkingDir)
	dir := SessionDir(base, cp.SessionKey)
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

// validateCheckpoint rejects a decoded checkpoint whose contents are not
// internally consistent. It is shared by the package's TWO deserialisation
// paths — Load and ListSuspendedIn — because a check that lives in only one of
// them leaves the other reading unvalidated state; the SchemaVersion comparison
// used to be inlined in both, which is exactly how they could have drifted.
//
// Everything here is a fact about a FILE ON DISK, so every fault returns an
// error rather than panicking: a checkpoint is external input (hand-edited,
// truncated by a disk fault, or written by some future bug on the write side),
// not a programming error inside this process. That distinction matters —
// RunTask runs on a task goroutine with no recover anywhere above it, so a
// panic here would turn one corrupt file into an agent-wide crash that takes
// every concurrent task with it.
//
// 守卫：TestLoadRejectsATaskStartPastTheEndOfMessages。
func validateCheckpoint(cp Checkpoint, path string) error {
	if cp.SchemaVersion != CheckpointSchemaVersion {
		return fmt.Errorf("checkpoint %q schema version %d unsupported (want %d)",
			path, cp.SchemaVersion, CheckpointSchemaVersion)
	}
	// TaskStart indexes into Messages (see its field doc). An out-of-range value
	// would slice out of bounds the moment the resumed conversation counts a
	// repeat streak, far from the file that caused it.
	//
	// An empty Messages is itself corrupt, and it needs its own check: the range
	// test below passes it (0 > 0 is false), so without this a checkpoint with no
	// messages would restore into a conversation the resumed run then indexes
	// message[0] of. The write side always snapshots a conversation carrying at
	// least message[0] (the base prompt), so an empty one never comes from us.
	// 守卫：TestLoadRejectsACheckpointWithNoMessages。
	if len(cp.Messages) == 0 {
		return fmt.Errorf("checkpoint %q has no messages; a suspended run always carries "+
			"at least its base prompt", path)
	}
	if cp.TaskStart < 0 || cp.TaskStart > len(cp.Messages) {
		return fmt.Errorf("checkpoint %q task_start %d out of range for %d messages",
			path, cp.TaskStart, len(cp.Messages))
	}
	return nil
}

// Load reads the checkpoint for sessionKey under SessionBase(s.workspaceRoot,
// workingDir). Absence is legitimate (fresh task): it returns (zero, false,
// nil). Any other fault — unreadable file, corrupt JSON, or an unrecognised
// schema version — returns a fail-loud error rather than pretending the task
// has no prior state.
func (s *Store) Load(sessionKey, workingDir string) (Checkpoint, bool, error) {
	base := SessionBase(s.workspaceRoot, workingDir)
	path := filepath.Join(SessionDir(base, sessionKey), checkpointFileName)
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
	if err := validateCheckpoint(cp, path); err != nil {
		return Checkpoint{}, false, err
	}
	return cp, true, nil
}

// Delete removes a session's checkpoint under SessionBase(s.workspaceRoot,
// workingDir). A missing file is not an error (delete is idempotent — a
// completed or already-cleaned task is the normal case).
func (s *Store) Delete(sessionKey, workingDir string) error {
	base := SessionBase(s.workspaceRoot, workingDir)
	path := filepath.Join(SessionDir(base, sessionKey), checkpointFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove checkpoint %q: %w", path, err)
	}
	return nil
}

// WritePlan writes a Plan-mode artifact to
// <SessionBase(workspaceRoot, workingDir)>/session/<sessionKey>/plans/<filename>,
// creating the directory. It returns the absolute path written. An empty
// sessionKey or filename is rejected (fail-loud — never write to a malformed
// path). This is where an OKF "one concept, one file" plan lands (design §4.2).
func (s *Store) WritePlan(sessionKey, workingDir, filename, content string) (string, error) {
	if sessionKey == "" || filename == "" {
		return "", fmt.Errorf("write plan: empty sessionKey or filename (key=%q file=%q)", sessionKey, filename)
	}
	base := SessionBase(s.workspaceRoot, workingDir)
	dir := filepath.Join(SessionDir(base, sessionKey), "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create plans dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write plan %q: %w", path, err)
	}
	return path, nil
}

// ListSuspendedIn loads every checkpoint under <base>/session/*/task-state.json.
// A missing base dir yields an empty slice (no sessions yet). A corrupt or
// version-mismatched checkpoint fails loud — recovery must not silently skip a
// task it cannot restore.
func (s *Store) ListSuspendedIn(base string) ([]Checkpoint, error) {
	sessionsRoot := filepath.Join(base, "session")
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
		path := filepath.Join(sessionsRoot, entry.Name(), checkpointFileName)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // session dir without a checkpoint (e.g. only plans/) — legitimate
		}
		if err != nil {
			return nil, fmt.Errorf("read suspended checkpoint %q: %w", path, err)
		}
		var cp Checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			return nil, fmt.Errorf("decode suspended checkpoint %q: %w", path, err)
		}
		if err := validateCheckpoint(cp, path); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, nil
}

// ListSuspended loads every checkpoint under the workspace root
// (ListSuspendedIn(s.workspaceRoot)). It does not see checkpoints filed under a
// working_dir base; enumerating across working_dir bases is Task 5's concern.
func (s *Store) ListSuspended() ([]Checkpoint, error) {
	return s.ListSuspendedIn(s.workspaceRoot)
}
