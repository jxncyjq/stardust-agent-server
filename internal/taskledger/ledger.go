package taskledger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidEvent      = errors.New("invalid task ledger event")
	ErrDuplicateClaim    = errors.New("task owner already claimed")
	ErrProjectionTooLong = errors.New("task ledger projection exceeds configured line limit")
)

var eventIDCounter atomic.Uint64

type Config struct {
	WorkspaceRoot    string
	IndexPath        string
	Root             string
	ArchiveRoot      string
	MaxIndexLines    int
	MaxTaskLines     int
	MaxMessageChars  int
	ActiveStatuses   []string
	DoneStatuses     []string
	AllowedAgentIDs  []string
	Now              func() time.Time
	EventIDGenerator func(time.Time) string
}

type Ledger struct {
	cfg Config
	mu  sync.Mutex
}

// New creates an event-backed task ledger rooted inside a workspace.
func New(cfg Config) (*Ledger, error) {
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = "."
	}
	workspaceRoot, err := filepath.Abs(cfg.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	cfg.WorkspaceRoot = workspaceRoot
	if cfg.IndexPath == "" {
		cfg.IndexPath = "tasks.md"
	}
	if cfg.Root == "" {
		cfg.Root = "tasks"
	}
	if cfg.ArchiveRoot == "" {
		cfg.ArchiveRoot = filepath.Join(cfg.Root, "archive")
	}
	if cfg.MaxIndexLines <= 0 {
		cfg.MaxIndexLines = 500
	}
	if cfg.MaxTaskLines <= 0 {
		cfg.MaxTaskLines = 300
	}
	if cfg.MaxMessageChars <= 0 {
		cfg.MaxMessageChars = 300
	}
	if len(cfg.ActiveStatuses) == 0 {
		cfg.ActiveStatuses = []string{"planned", "ready", "in_progress", "blocked", "review"}
	}
	if len(cfg.DoneStatuses) == 0 {
		cfg.DoneStatuses = []string{"done", "cancelled"}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.EventIDGenerator == nil {
		cfg.EventIDGenerator = defaultEventID
	}
	if _, err := resolveWithin(workspaceRoot, cfg.IndexPath); err != nil {
		return nil, err
	}
	if _, err := resolveWithin(workspaceRoot, cfg.Root); err != nil {
		return nil, err
	}
	if _, err := resolveWithin(workspaceRoot, cfg.ArchiveRoot); err != nil {
		return nil, err
	}
	return &Ledger{cfg: cfg}, nil
}

func (l *Ledger) Append(ctx context.Context, event Event) (appended Event, err error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.acquireLock(ctx)
	if err != nil {
		return Event{}, err
	}
	// Surface a failed release when the write itself succeeded: a lock left
	// behind blocks every later write, so reporting it is the difference
	// between one failed call and a stuck ledger.
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && err == nil {
			appended, err = Event{}, unlockErr
		}
	}()
	event = l.normalizeEvent(event)
	if err := l.validateEvent(event); err != nil {
		return Event{}, err
	}
	// Ownership is decided here, while the lock is held. Deciding it later (in
	// the projection) meant every racing claimant got a success and the loser
	// only found out by reading a Markdown conflict line it never sees.
	if event.Type == EventTaskClaimed {
		if err := l.checkClaim(event); err != nil {
			return Event{}, err
		}
	}
	path, err := l.eventPath(event.CreatedAt)
	if err != nil {
		return Event{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Event{}, fmt.Errorf("create event directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return Event{}, fmt.Errorf("open event log: %w", err)
	}
	defer file.Close()
	data, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode event: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return Event{}, fmt.Errorf("append event: %w", err)
	}
	return event, nil
}

func (l *Ledger) Rebuild(ctx context.Context) (projection Projection, rebuildErr error) {
	if err := ctx.Err(); err != nil {
		return Projection{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.acquireLock(ctx)
	if err != nil {
		return Projection{}, err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && rebuildErr == nil {
			projection, rebuildErr = Projection{}, unlockErr
		}
	}()
	events, err := l.ReadEvents(ctx)
	if err != nil {
		return Projection{}, err
	}
	projection = BuildProjection(events, l.cfg)
	if err := l.writeProjection(ctx, projection); err != nil {
		return Projection{}, err
	}
	return projection, nil
}

// Snapshot replays events into a projection without writing projection files.
func (l *Ledger) Snapshot(ctx context.Context) (Projection, error) {
	if err := ctx.Err(); err != nil {
		return Projection{}, err
	}
	events, err := l.ReadEvents(ctx)
	if err != nil {
		return Projection{}, err
	}
	return BuildProjection(events, l.cfg), nil
}

func (l *Ledger) ReadEvents(ctx context.Context) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := l.eventsRoot()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var events []Event
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Never swallow: a partial event set would be handed to Rebuild,
			// which atomically overwrites tasks.md and every tasks/*.md — so a
			// single unreadable file would silently erase history.
			return fmt.Errorf("walk events at %q: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		fileEvents, err := readEventFile(path)
		if err != nil {
			return err
		}
		events = append(events, fileEvents...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	sortEvents(events)
	return dedupeEvents(events), nil
}

func (l *Ledger) normalizeEvent(event Event) Event {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = l.cfg.Now()
	}
	if event.EventID == "" {
		event.EventID = l.cfg.EventIDGenerator(event.CreatedAt)
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = schemaVersion
	}
	if event.ActorAgentID == "" {
		event.ActorAgentID = event.From
	}
	event.Summary = truncateRunes(strings.TrimSpace(event.Summary), l.cfg.MaxMessageChars)
	event.Title = strings.TrimSpace(event.Title)
	event.Status = strings.TrimSpace(event.Status)
	event.Owner = strings.TrimSpace(event.Owner)
	event.Artifact = strings.TrimSpace(event.Artifact)
	return event
}

func (l *Ledger) validateEvent(event Event) error {
	if event.EventID == "" {
		return fmt.Errorf("%w: event_id required", ErrInvalidEvent)
	}
	// Validate before the event becomes durable. Deferring this to the
	// projection stage is what allowed an unsafe id to be written and then brick
	// every subsequent Rebuild.
	if err := ValidateTaskID(event.TaskID); err != nil {
		return err
	}
	if err := l.validateStatus(event.Status); err != nil {
		return err
	}
	if event.Type == "" {
		return fmt.Errorf("%w: type required", ErrInvalidEvent)
	}
	if event.SchemaVersion != schemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidEvent, event.SchemaVersion)
	}
	if !validEventType(event.Type) {
		return fmt.Errorf("%w: unsupported type %q", ErrInvalidEvent, event.Type)
	}
	if event.ActorAgentID != "" && !l.agentAllowed(event.ActorAgentID) {
		return fmt.Errorf("%w: unknown actor_agent_id %q", ErrInvalidEvent, event.ActorAgentID)
	}
	if event.To != "" && !l.agentAllowed(event.To) {
		return fmt.Errorf("%w: unknown to agent %q", ErrInvalidEvent, event.To)
	}
	if event.Artifact != "" {
		if _, err := resolveWithin(l.cfg.WorkspaceRoot, event.Artifact); err != nil {
			return err
		}
	}
	return nil
}

// ownersPath is the owner index: taskID -> current owner. It is a cache derived
// entirely from the event log, kept next to it so a claim can be decided under
// the lock without replaying every event on every call.
func (l *Ledger) ownersPath() (string, error) {
	return resolveWithin(l.cfg.WorkspaceRoot, filepath.Join(l.cfg.Root, ".owners.json"))
}

// readOwners loads the owner index, rebuilding it from the event log when it is
// absent or unreadable.
//
// A missing index is normal (first run, or a ledger written before the index
// existed). A corrupt one is not a reason to give up: the events remain the
// source of truth, so it is rebuilt rather than treated as "no owners" — the
// latter would silently let a claimed task be claimed again.
func (l *Ledger) readOwners(ctx context.Context) (map[string]string, error) {
	path, err := l.ownersPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read owner index %q: %w", path, err)
		}
		return l.ownersFromEvents(ctx)
	}
	owners := make(map[string]string)
	if err := json.Unmarshal(data, &owners); err != nil {
		return l.ownersFromEvents(ctx)
	}
	return owners, nil
}

// ownersFromEvents derives the index from the event log, which is authoritative.
func (l *Ledger) ownersFromEvents(ctx context.Context) (map[string]string, error) {
	events, err := l.ReadEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("rebuild owner index: %w", err)
	}
	sortEvents(events)
	owners := make(map[string]string)
	for _, event := range events {
		if event.Type != EventTaskClaimed {
			continue
		}
		owner := firstNonEmptyLedgerValue(event.Owner, event.ActorAgentID)
		if owner == "" {
			continue
		}
		// First claim wins, matching how the projection resolves ownership.
		if _, taken := owners[event.TaskID]; !taken {
			owners[event.TaskID] = owner
		}
	}
	return owners, nil
}

func (l *Ledger) writeOwners(owners map[string]string) error {
	path, err := l.ownersPath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(owners)
	if err != nil {
		return fmt.Errorf("encode owner index: %w", err)
	}
	if err := writeFileAtomic(path, string(data)); err != nil {
		return fmt.Errorf("write owner index %q: %w", path, err)
	}
	return nil
}

// checkClaim enforces exclusive ownership. It runs inside Append while the
// cross-process lock is held, so the read-decide-write sequence cannot
// interleave with another claimant.
//
// Re-claiming a task one already owns succeeds: a caller that lost its response
// must be able to retry without the retry looking like a conflict.
func (l *Ledger) checkClaim(event Event) error {
	ctx := context.Background()
	owners, err := l.readOwners(ctx)
	if err != nil {
		return err
	}
	claimant := firstNonEmptyLedgerValue(event.Owner, event.ActorAgentID)
	if claimant == "" {
		return fmt.Errorf("%w: claim requires an owner", ErrInvalidEvent)
	}
	if current, taken := owners[event.TaskID]; taken && current != claimant {
		return fmt.Errorf("%w: task %q is owned by %q", ErrDuplicateClaim, event.TaskID, current)
	}
	owners[event.TaskID] = claimant
	return l.writeOwners(owners)
}

func firstNonEmptyLedgerValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// validateStatus rejects a status outside the configured vocabulary. status is
// not cosmetic: isTerminal compares it against DoneStatuses to decide archival,
// so a typo like "donee" produced a task that could never be archived and
// stayed in the active index forever.
//
// An empty status is allowed — most event types (messages, handoffs) carry no
// status, and create_task has its own default injected upstream. When neither
// list is configured the ledger has no vocabulary to check against, so anything
// is accepted; that is a configuration choice, not a silent fallback.
func (l *Ledger) validateStatus(status string) error {
	if status == "" {
		return nil
	}
	allowed := make([]string, 0, len(l.cfg.ActiveStatuses)+len(l.cfg.DoneStatuses))
	allowed = append(allowed, l.cfg.ActiveStatuses...)
	allowed = append(allowed, l.cfg.DoneStatuses...)
	if len(allowed) == 0 {
		return nil
	}
	if slices.Contains(allowed, status) {
		return nil
	}
	return fmt.Errorf("%w: unknown status %q (want one of %s)", ErrInvalidEvent, status, strings.Join(allowed, ", "))
}

func (l *Ledger) agentAllowed(agentID string) bool {
	if len(l.cfg.AllowedAgentIDs) == 0 {
		return true
	}
	return slices.Contains(l.cfg.AllowedAgentIDs, agentID)
}

func (l *Ledger) writeProjection(ctx context.Context, projection Projection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	index, err := l.indexPath()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(index, projection.IndexMarkdown); err != nil {
		return err
	}
	for taskID, content := range projection.TaskMarkdown {
		if err := ctx.Err(); err != nil {
			return err
		}
		task := projection.Tasks[taskID]
		activePath, err := l.taskPath(taskID)
		if err != nil {
			return err
		}
		if isTerminal(task.Status, l.cfg.DoneStatuses) {
			path, err := l.archiveTaskPath(taskID)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(path, content); err != nil {
				return err
			}
			if err := os.Remove(activePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove active task projection %q: %w", activePath, err)
			}
			continue
		}
		if err := writeFileAtomic(activePath, content); err != nil {
			return err
		}
		archivePath, err := l.archiveTaskPath(taskID)
		if err != nil {
			return err
		}
		if err := os.Remove(archivePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove archived task projection %q: %w", archivePath, err)
		}
	}
	return nil
}

// lockStaleAfter is how long a .lock may sit untouched before it is treated as
// abandoned. It must exceed the longest legitimate Append/Rebuild, and the
// projection rewrite is the slow part; a killed process should not block writes
// for longer than this.
const lockStaleAfter = 2 * time.Minute

// acquireLock takes the cross-process ledger lock, reclaiming one left behind by
// a process that died mid-write.
//
// The returned release function reports its own failure instead of discarding
// it: a lock that cannot be removed blocks every later write, so it must not be
// swallowed.
func (l *Ledger) acquireLock(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := l.rootPath()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, ".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}

	// Two attempts at most: acquire, and if an abandoned lock was cleared,
	// acquire again. More would risk two writers ping-ponging over one lock.
	for attempt := range 2 {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			// The stamp is diagnostic only — staleness is judged by mtime, which
			// cannot be corrupted by a partial write.
			if _, writeErr := file.WriteString(time.Now().Format(time.RFC3339Nano)); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("stamp task ledger lock: %w", writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("close task ledger lock: %w", closeErr)
			}
			return func() error {
				if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("release task ledger lock %q: %w", lockPath, err)
				}
				return nil
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquire task ledger lock: %w", err)
		}
		if attempt > 0 {
			break
		}

		info, statErr := os.Stat(lockPath)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				// Released between our attempt and the stat; retry immediately.
				continue
			}
			return nil, fmt.Errorf("inspect task ledger lock: %w", statErr)
		}
		if age := time.Since(info.ModTime()); age <= lockStaleAfter {
			return nil, fmt.Errorf("acquire task ledger lock: held by another writer since %s (%s ago)",
				info.ModTime().Format(time.RFC3339), age.Truncate(time.Second))
		}
		if removeErr := os.Remove(lockPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, fmt.Errorf("reclaim stale task ledger lock: %w", removeErr)
		}
	}
	return nil, fmt.Errorf("acquire task ledger lock: still held after reclaiming a stale lock")
}

func (l *Ledger) eventsRoot() (string, error) {
	root, err := l.rootPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "events"), nil
}

func (l *Ledger) eventPath(t time.Time) (string, error) {
	root, err := l.eventsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, t.Format("2006-01-02")+".jsonl"), nil
}

// rootPath resolves the ledger root inside the workspace. It reports the sandbox
// check's failure instead of returning "": a discarded error here degraded every
// later filepath.Join into a process-cwd-relative path, writing events, .lock and
// projections outside the ledger while the containment check appeared to pass.
func (l *Ledger) rootPath() (string, error) {
	path, err := resolveWithin(l.cfg.WorkspaceRoot, l.cfg.Root)
	if err != nil {
		return "", fmt.Errorf("resolve task ledger root: %w", err)
	}
	return path, nil
}

func (l *Ledger) indexPath() (string, error) {
	return resolveWithin(l.cfg.WorkspaceRoot, l.cfg.IndexPath)
}

// ValidateTaskID is the single gate for task_id legality. Both the write path
// (validateEvent, before the event is durable) and the projection path
// (taskPath, when the id becomes a filename) go through it.
//
// They must never diverge: a rule that accepts on write but rejects on
// projection produces an event that is written yet un-rebuildable, and since
// nothing can delete a persisted event, every later Rebuild fails on it
// forever.
//
// The id format itself is deliberately NOT constrained — ids like
// "TASK-20260523-101" are generated and consumed internally. Only what makes an
// id unsafe as a path component is rejected.
func ValidateTaskID(taskID string) error {
	if taskID == "" {
		return fmt.Errorf("%w: task_id required", ErrInvalidEvent)
	}
	if strings.TrimSpace(taskID) != taskID || strings.TrimSpace(taskID) == "" {
		return fmt.Errorf("%w: task_id %q has leading or trailing whitespace", ErrInvalidEvent, taskID)
	}
	if strings.ContainsAny(taskID, "/\\") || strings.Contains(taskID, string(filepath.Separator)) {
		return fmt.Errorf("%w: unsafe task_id %q: path separators are not allowed", ErrInvalidEvent, taskID)
	}
	if strings.Contains(taskID, "..") {
		return fmt.Errorf("%w: unsafe task_id %q: %q is not allowed", ErrInvalidEvent, taskID, "..")
	}
	for _, r := range taskID {
		// Control characters would corrupt both the JSONL event log and the
		// Markdown projection.
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: task_id %q contains a control character", ErrInvalidEvent, taskID)
		}
	}
	return nil
}

func (l *Ledger) taskPath(taskID string) (string, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return "", err
	}
	return resolveWithin(l.cfg.WorkspaceRoot, filepath.Join(l.cfg.Root, taskID+".md"))
}

func (l *Ledger) archiveTaskPath(taskID string) (string, error) {
	if strings.Contains(taskID, string(filepath.Separator)) || strings.Contains(taskID, "/") || strings.Contains(taskID, "\\") {
		return "", fmt.Errorf("%w: unsafe task_id %q", ErrInvalidEvent, taskID)
	}
	return resolveWithin(l.cfg.WorkspaceRoot, filepath.Join(l.cfg.ArchiveRoot, taskID+".md"))
}

func readEventFile(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open event file %q: %w", path, err)
	}
	defer file.Close()
	var events []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decode event file %q: %w", path, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan event file %q: %w", path, err)
	}
	return events, nil
}

func validEventType(eventType string) bool {
	switch eventType {
	case EventTaskCreated, EventTaskClaimed, EventTaskStatusChanged, EventMessageAppended, EventResultAppended, EventHandoffAppended, EventReviewAppended:
		return true
	default:
		return strings.HasPrefix(eventType, EventConflictPrefix)
	}
}

func defaultEventID(t time.Time) string {
	seq := eventIDCounter.Add(1)
	return fmt.Sprintf("evt-%s-%06d", t.UTC().Format("20060102-150405.000000000"), seq)
}
