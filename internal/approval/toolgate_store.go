package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/sessionstate"
)

// ApprovalStatus is the lifecycle state of a ToolApproval ticket.
type ApprovalStatus string

const (
	// ApprovalPending marks a ticket awaiting a human decision.
	ApprovalPending ApprovalStatus = "pending"
	// ApprovalApproved marks a ticket a human has approved.
	ApprovalApproved ApprovalStatus = "approved"
	// ApprovalDenied marks a ticket a human has denied.
	ApprovalDenied ApprovalStatus = "denied"
)

// approvalsDirName is the subdirectory (under a session dir) holding one JSON
// file per ToolApproval ticket.
const approvalsDirName = "approvals"

// ToolApproval is a single Manual-mode tool-call approval ticket, persisted as
// one JSON file per ticket under the owning session's directory. Disk is the
// source of truth: a ToolGateStore never caches state in memory, so a ticket
// survives process restart and is visible to any process sharing base.
type ToolApproval struct {
	TicketID   string            `json:"ticket_id"`
	SessionKey string            `json:"session_key"`
	TaskID     string            `json:"task_id"`
	ToolCallID string            `json:"tool_call_id"`
	ToolName   string            `json:"tool_name"`
	Arguments  map[string]string `json:"arguments,omitempty"`
	Status     ApprovalStatus    `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	// WorkingDir is the host filesystem directory the owning task's session is
	// bound to, if any. When set, the ticket resolves its session base to
	// <WorkingDir>/.stardust (sessionstate.SessionBase) rather than the
	// workspace root, so approvals for a working_dir-scoped session live
	// alongside that directory.
	WorkingDir string `json:"working_dir,omitempty"`
}

// ToolGateStore persists ToolApproval tickets under a session directory tree:
// <base>/session/<sessionKey>/approvals/<ticketID>.json, where base is
// resolved per call via sessionstate.SessionBase(workspaceRoot, workingDir):
// workspaceRoot is used when a ticket (or lookup) carries no working_dir. It
// is a separate type from Service (internal/approval/service.go) because it
// serves a different schema and lifecycle — Manual-mode tool gating,
// disk-backed, keyed by (taskID, toolCallID) — rather than Service's
// in-memory workflow/hard-loop tickets.
type ToolGateStore struct {
	workspaceRoot string

	// mu serializes all disk I/O (temp-file+rename writes and reads) against
	// this store's ticket files. Without it, a concurrent Decide racing
	// another Decide or a ListForTask/ListPending read can observe a file
	// mid-rename: on Windows NTFS, os.Rename and os.ReadFile on the same path
	// collide with a sharing violation (the rename is atomic on Linux, which
	// masks the same underlying lack of serialization). A single coarse lock
	// is deliberate: approval I/O is rare and cheap, so correctness and
	// portability matter far more than fine-grained concurrency here.
	mu sync.Mutex
}

// NewToolGateStore returns a ToolGateStore rooted at workspaceRoot, the base
// used when a ticket (or lookup) carries no working_dir (the same root a
// sessionstate.Store uses).
func NewToolGateStore(workspaceRoot string) *ToolGateStore {
	return &ToolGateStore{workspaceRoot: workspaceRoot}
}

// ticketIDReplacer sanitizes filesystem-hostile characters out of a derived
// ticket ID so it is always safe to use as a file name component.
var ticketIDReplacer = strings.NewReplacer(
	"/", "_",
	`\`, "_",
	":", "_",
	"*", "_",
	"?", "_",
	`"`, "_",
	"<", "_",
	">", "_",
	"|", "_",
)

// TicketID derives a deterministic ID from (taskID, toolCallID) so Open is
// idempotent per call and lookups need no separate index. The result is
// sanitized to be filesystem-safe.
func TicketID(taskID, toolCallID string) string {
	return ticketIDReplacer.Replace(taskID + "__" + toolCallID)
}

func (s *ToolGateStore) ticketPath(workingDir, sessionKey, ticketID string) string {
	base := sessionstate.SessionBase(s.workspaceRoot, workingDir)
	return filepath.Join(sessionstate.SessionDir(base, sessionKey), approvalsDirName, ticketID+".json")
}

// Open creates (or, if the same (TaskID, ToolCallID) call already has a
// ticket, idempotently returns) a pending ToolApproval for rec. rec must carry
// a non-empty SessionKey, TaskID, and ToolCallID; anything else is rejected
// fail-loud. The returned ticket's TicketID, Status, CreatedAt, and UpdatedAt
// are always populated by Open, overriding whatever rec supplied. The ticket
// is filed under sessionstate.SessionBase(s.workspaceRoot, rec.WorkingDir).
func (s *ToolGateStore) Open(rec ToolApproval) (ToolApproval, error) {
	if rec.SessionKey == "" || rec.TaskID == "" || rec.ToolCallID == "" {
		return ToolApproval{}, fmt.Errorf("open approval: empty SessionKey/TaskID/ToolCallID (session=%q task=%q call=%q)", rec.SessionKey, rec.TaskID, rec.ToolCallID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ticketID := TicketID(rec.TaskID, rec.ToolCallID)
	existing, ok, err := s.getLocked(rec.SessionKey, ticketID, rec.WorkingDir)
	if err != nil {
		return ToolApproval{}, fmt.Errorf("open approval: check existing ticket %s: %w", ticketID, err)
	}
	if ok {
		return existing, nil
	}
	now := time.Now()
	rec.TicketID = ticketID
	rec.Status = ApprovalPending
	rec.CreatedAt = now
	rec.UpdatedAt = now
	if err := s.writeLocked(rec); err != nil {
		return ToolApproval{}, fmt.Errorf("open approval: %w", err)
	}
	return rec, nil
}

// Get reads the ticket ticketID under sessionKey, resolved via
// sessionstate.SessionBase(s.workspaceRoot, workingDir). A ticket that does
// not exist is a legitimate absence: it returns (zero, false, nil). Any other
// read fault — an unreadable file or corrupt JSON — returns a fail-loud
// error; Get never treats a decode failure as "not found".
func (s *ToolGateStore) Get(sessionKey, ticketID, workingDir string) (ToolApproval, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(sessionKey, ticketID, workingDir)
}

// getLocked is Get's implementation. Callers must hold s.mu.
func (s *ToolGateStore) getLocked(sessionKey, ticketID, workingDir string) (ToolApproval, bool, error) {
	path := s.ticketPath(workingDir, sessionKey, ticketID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ToolApproval{}, false, nil
	}
	if err != nil {
		return ToolApproval{}, false, fmt.Errorf("read approval %q: %w", path, err)
	}
	var rec ToolApproval
	if err := json.Unmarshal(data, &rec); err != nil {
		return ToolApproval{}, false, fmt.Errorf("decode approval %q: %w", path, err)
	}
	return rec, true, nil
}

// Decide records a human decision on ticketID (looked up under
// sessionstate.SessionBase(s.workspaceRoot, workingDir)): status must be
// ApprovalApproved or ApprovalDenied. An unknown ticketID returns an error
// wrapping ErrTicketNotFound (so callers can errors.Is-match it, e.g. an HTTP
// layer mapping it to 404). A ticket that is not currently ApprovalPending —
// already decided — is rejected fail-loud with an error wrapping
// ErrTicketAlreadyDecided (so callers can errors.Is-match it, e.g. an HTTP
// layer mapping it to 409, or a concurrent sweep tolerating the race) rather
// than silently overwritten.
func (s *ToolGateStore) Decide(sessionKey, ticketID string, status ApprovalStatus, workingDir string) (ToolApproval, error) {
	if status != ApprovalApproved && status != ApprovalDenied {
		return ToolApproval{}, fmt.Errorf("decide approval: invalid status %q (want approved|denied)", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok, err := s.getLocked(sessionKey, ticketID, workingDir)
	if err != nil {
		return ToolApproval{}, fmt.Errorf("decide approval: %w", err)
	}
	if !ok {
		return ToolApproval{}, fmt.Errorf("decide approval: ticket %s not found: %w", ticketID, ErrTicketNotFound)
	}
	if rec.Status != ApprovalPending {
		return ToolApproval{}, fmt.Errorf("decide approval: ticket %s already decided (%s): %w", ticketID, rec.Status, ErrTicketAlreadyDecided)
	}
	rec.Status = status
	rec.UpdatedAt = time.Now()
	if err := s.writeLocked(rec); err != nil {
		return ToolApproval{}, fmt.Errorf("decide approval: %w", err)
	}
	return rec, nil
}

// ListForTask returns every ticket recorded under sessionKey whose TaskID
// matches taskID, resolved via sessionstate.SessionBase(s.workspaceRoot,
// workingDir). A session with no approvals directory yet is a legitimate
// empty result. A corrupt ticket file inside the directory fails loud.
func (s *ToolGateStore) ListForTask(sessionKey, taskID, workingDir string) ([]ToolApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := sessionstate.SessionBase(s.workspaceRoot, workingDir)
	dir := filepath.Join(sessionstate.SessionDir(base, sessionKey), approvalsDirName)
	recs, err := readApprovalsDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list approvals for task %s: %w", taskID, err)
	}
	var out []ToolApproval
	for _, rec := range recs {
		if rec.TaskID == taskID {
			out = append(out, rec)
		}
	}
	return out, nil
}

// ListPendingIn returns every ApprovalPending ticket across all sessions
// under base, for the timeout sweep. A corrupt ticket file anywhere in the
// tree fails loud rather than being silently skipped.
func (s *ToolGateStore) ListPendingIn(base string) ([]ToolApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pattern := filepath.Join(base, "session", "*", approvalsDirName, "*.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("list pending approvals: glob %q: %w", pattern, err)
	}
	var out []ToolApproval
	for _, path := range paths {
		rec, err := readApprovalFile(path)
		if err != nil {
			return nil, fmt.Errorf("list pending approvals: %w", err)
		}
		if rec.Status == ApprovalPending {
			out = append(out, rec)
		}
	}
	return out, nil
}

// ListPending returns every ApprovalPending ticket under the workspace root
// (ListPendingIn(s.workspaceRoot)). It does not see tickets filed under a
// working_dir base; enumerating across working_dir bases is Task 5's concern.
func (s *ToolGateStore) ListPending() ([]ToolApproval, error) {
	return s.ListPendingIn(s.workspaceRoot)
}

// writeLocked atomically persists rec to its ticket path (temp file +
// rename), mirroring sessionstate.Store.Save so a crash mid-write never
// leaves a half-written ticket file behind. It is stored under
// sessionstate.SessionBase(s.workspaceRoot, rec.WorkingDir). Callers must hold
// s.mu: the rename must not race a concurrent read of the same path
// (Get/ListForTask/ListPending), which on Windows NTFS collides with a
// sharing violation.
func (s *ToolGateStore) writeLocked(rec ToolApproval) error {
	base := sessionstate.SessionBase(s.workspaceRoot, rec.WorkingDir)
	dir := filepath.Join(sessionstate.SessionDir(base, rec.SessionKey), approvalsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create approvals dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal approval %s: %w", rec.TicketID, err)
	}
	final := filepath.Join(dir, rec.TicketID+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write approval tmp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename approval %q: %w", final, err)
	}
	return nil
}

// readApprovalsDir decodes every *.json ticket file directly inside dir. A
// missing dir (no approvals opened yet for this session) is a legitimate
// empty result. Any other read fault or corrupt JSON fails loud.
func readApprovalsDir(dir string) ([]ToolApproval, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read approvals dir %q: %w", dir, err)
	}
	var out []ToolApproval
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rec, err := readApprovalFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// readApprovalFile reads and decodes a single ticket file. It never treats a
// missing or corrupt file as absence — callers that need "not found" as a
// legitimate outcome (Get) handle os.ErrNotExist themselves before reaching
// this helper's callers that assume the path exists (a directory listing).
func readApprovalFile(path string) (ToolApproval, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ToolApproval{}, fmt.Errorf("read approval %q: %w", path, err)
	}
	var rec ToolApproval
	if err := json.Unmarshal(data, &rec); err != nil {
		return ToolApproval{}, fmt.Errorf("decode approval %q: %w", path, err)
	}
	return rec, nil
}
