package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/workflow"

	_ "modernc.org/sqlite"
)

type SQLiteRepository struct {
	db *sql.DB
}

// ErrAgentSessionNotFound is returned by DeleteAgentSession when no session with
// the given id exists. Callers (e.g. the HTTP layer) match it with errors.Is to
// translate a missing session into a 404 instead of swallowing the failure.
var ErrAgentSessionNotFound = errors.New("agent session not found")

// CurrentSchemaVersion is bumped whenever schemaStatements or the idempotent
// column migrations in migrate change. Version 2 added the agent_sessions.project
// and tasks.session_id columns for two-level session grouping. Version 3 added
// the agent_sessions.archived column for archiving sessions and projects. Version
// 4 added the conversation_turns_fts FTS5 virtual table backing session_search.
// Version 5 added the agent_sessions.mode column for persisting manual/auto mode.
// Version 6 added the agent_sessions.working_dir column for persisting the host
// filesystem directory a session is bound to.
const CurrentSchemaVersion = 6

type WorkflowState struct {
	Definition workflow.Definition `json:"definition"`
	Result     workflow.Result     `json:"result"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// sqliteBusyTimeout is how long a blocked SQLite operation waits for a competing
// lock to clear before giving up. It is applied as a per-connection PRAGMA (see
// sqliteDSN) so transient writer contention — e.g. an HTTP handler writing a
// session while the coordinator's background scheduler writes a task — blocks and
// retries instead of surfacing an immediate SQLITE_BUSY as a bare 500. This only
// widens the retry window; a genuine, non-transient lock still fails loudly once
// the timeout elapses, so no error is swallowed.
const sqliteBusyTimeout = 5 * time.Second

// sqliteDSN augments a filesystem path with the PRAGMAs every physical connection
// must apply. busy_timeout is set through the DSN (rather than a one-off PRAGMA
// after Open) so it takes effect on every connection the pool opens, not just the
// first. Journal mode is deliberately left at the default rollback journal instead
// of WAL: BackupSQLite copies the single database file, and WAL would move recent
// commits into a separate -wal sidecar the copy would miss.
func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(" + strconv.Itoa(int(sqliteBusyTimeout.Milliseconds())) + ")"
}

func OpenSQLite(ctx context.Context, path string) (*SQLiteRepository, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single-writer model: the repository is the sole writer of this embedded
	// database, yet several goroutines (HTTP handlers, the coordinator scheduler,
	// background sweeps) reach it at once. Capping the pool at one connection
	// serializes every access so concurrent writers queue instead of colliding on
	// the file lock and returning SQLITE_BUSY. Reads here are infrequent and cheap,
	// so the lost read parallelism is an acceptable trade for contention-free writes.
	db.SetMaxOpenConns(1)
	repo := &SQLiteRepository{db: db}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := repo.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return repo, nil
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}

func (r *SQLiteRepository) Ping(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0)
		FROM schema_migrations
	`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (r *SQLiteRepository) Add(ctx context.Context, task domain.Task) error {
	return r.SaveTask(ctx, task)
}

func (r *SQLiteRepository) Get(ctx context.Context, taskID string) (domain.Task, bool, error) {
	return r.GetTask(ctx, taskID)
}

func (r *SQLiteRepository) List(ctx context.Context) ([]domain.Task, error) {
	return r.ListTasks(ctx)
}

// ListTasks returns every persisted task ordered by creation time ascending. An
// empty table yields a non-nil empty slice so JSON callers serialize it as [].
func (r *SQLiteRepository) ListTasks(ctx context.Context) ([]domain.Task, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, company_id, agent_id, session_id, status, input, max_iterations, created_at
		FROM tasks
		ORDER BY created_at ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]domain.Task, 0)
	for rows.Next() {
		var task domain.Task
		var status string
		var createdAt string
		if err := rows.Scan(&task.ID, &task.CompanyID, &task.AgentID, &task.SessionID, &status, &task.Input, &task.MaxIterations, &createdAt); err != nil {
			return nil, fmt.Errorf("scan task row: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse task %q created_at: %w", task.ID, err)
		}
		task.Status = domain.TaskStatus(status)
		task.CreatedAt = parsed
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task rows: %w", err)
	}
	return tasks, nil
}

// SaveTask writes a whole task row, overwriting every column. The row is a
// partial projection of domain.Task: Mode, WorkingDir and Images have no
// columns and do not survive the round trip, so a task read back from here is
// not a full substitute for the live one. Callers holding only part of a task
// want UpdateTaskStatus instead.
func (r *SQLiteRepository) SaveTask(ctx context.Context, task domain.Task) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, company_id, agent_id, session_id, status, input, max_iterations, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			company_id = excluded.company_id,
			agent_id = excluded.agent_id,
			session_id = excluded.session_id,
			status = excluded.status,
			input = excluded.input,
			max_iterations = excluded.max_iterations,
			created_at = excluded.created_at
	`, task.ID, task.CompanyID, task.AgentID, task.SessionID, string(task.Status), task.Input, task.MaxIterations, formatTime(task.CreatedAt))
	if err != nil {
		return fmt.Errorf("save task %q: %w", task.ID, err)
	}
	return nil
}

// UpdateTaskStatus records a task state change, and nothing else: only the
// status and agent_id columns move, so a caller holding a partially populated
// domain.Task cannot blank out the rest of the row. Use SaveTask to write a
// whole task.
//
// A task with no row is not an error. The tasks table records only tasks that
// entered through a creation path; workflow-internal tasks live solely in the
// scheduler and have nothing to update. That absence is contract, not a
// swallowed failure.
func (r *SQLiteRepository) UpdateTaskStatus(ctx context.Context, taskID string, status domain.TaskStatus, agentID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, agent_id = ? WHERE id = ?
	`, string(status), agentID, taskID)
	if err != nil {
		return fmt.Errorf("update task %q status to %s: %w", taskID, status, err)
	}
	return nil
}

func (r *SQLiteRepository) GetTask(ctx context.Context, taskID string) (domain.Task, bool, error) {
	var task domain.Task
	var status string
	var createdAt string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, company_id, agent_id, session_id, status, input, max_iterations, created_at
		FROM tasks
		WHERE id = ?
	`, taskID).Scan(&task.ID, &task.CompanyID, &task.AgentID, &task.SessionID, &status, &task.Input, &task.MaxIterations, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, false, nil
	}
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("get task %q: %w", taskID, err)
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("parse task %q created_at: %w", taskID, err)
	}
	task.Status = domain.TaskStatus(status)
	task.CreatedAt = parsed
	return task, true, nil
}

func (r *SQLiteRepository) TryLock(ctx context.Context, taskID string, ownerID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO task_locks (task_id, owner_id, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			owner_id = excluded.owner_id,
			expires_at = excluded.expires_at
		WHERE task_locks.expires_at <= ?
	`, taskID, ownerID, formatTime(expiresAt), formatTime(now))
	if err != nil {
		return false, fmt.Errorf("try lock task %q: %w", taskID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check lock task %q rows affected: %w", taskID, err)
	}
	return affected == 1, nil
}

func (r *SQLiteRepository) ReapExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM task_locks
		WHERE expires_at <= ?
	`, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("reap expired locks: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check reaped lock rows affected: %w", err)
	}
	return int(affected), nil
}

func (r *SQLiteRepository) SaveAgentSession(ctx context.Context, session domain.AgentSession) error {
	mode := session.Mode
	if mode == "" {
		mode = domain.ModeAuto
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_sessions (
			id, company_id, agent_id, project, title, mode, archived, working_dir, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			company_id = excluded.company_id,
			agent_id = excluded.agent_id,
			project = excluded.project,
			title = excluded.title,
			mode = excluded.mode,
			archived = excluded.archived,
			working_dir = excluded.working_dir,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at
	`, session.ID, session.CompanyID, session.AgentID, session.Project, session.Title, mode, boolToInt(session.Archived), session.WorkingDir, formatTime(session.CreatedAt), formatTime(session.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save agent session %q: %w", session.ID, err)
	}
	return nil
}

// DeleteAgentSession removes a session and cascades the delete to every
// conversation turn belonging to it, in a single transaction so a partial
// failure cannot leave orphaned turns. A session id that does not exist is
// reported as an error rather than silently treated as success, per the
// fail-loud rule: deleting a nonexistent session is an inconsistent request the
// caller must learn about.
func (r *SQLiteRepository) DeleteAgentSession(ctx context.Context, sessionID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete agent session %q: %w", sessionID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM conversation_turns WHERE session_id = ?
	`, sessionID); err != nil {
		return fmt.Errorf("delete conversation turns for session %q: %w", sessionID, err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM agent_sessions WHERE id = ?
	`, sessionID)
	if err != nil {
		return fmt.Errorf("delete agent session %q: %w", sessionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted agent session %q rows affected: %w", sessionID, err)
	}
	if affected == 0 {
		return fmt.Errorf("delete agent session %q: %w", sessionID, ErrAgentSessionNotFound)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete agent session %q: %w", sessionID, err)
	}
	return nil
}

func (r *SQLiteRepository) LatestAgentSession(ctx context.Context, companyID string, agentID string) (domain.AgentSession, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, company_id, agent_id, project, title, mode, archived, working_dir, created_at, updated_at
		FROM agent_sessions
		WHERE company_id = ? AND agent_id = ?
		ORDER BY updated_at DESC, id DESC
		LIMIT 1
	`, companyID, agentID)
	session, err := scanAgentSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentSession{}, false, nil
	}
	if err != nil {
		return domain.AgentSession{}, false, fmt.Errorf("latest agent session for %q/%q: %w", companyID, agentID, err)
	}
	return session, true, nil
}

// GetAgentSession loads a single session by its id. The boolean is false when no
// session with that id exists, which is a legitimate "not found" state callers
// must handle; any other failure is returned as a wrapped error.
func (r *SQLiteRepository) GetAgentSession(ctx context.Context, sessionID string) (domain.AgentSession, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, company_id, agent_id, project, title, mode, archived, working_dir, created_at, updated_at
		FROM agent_sessions
		WHERE id = ?
	`, sessionID)
	session, err := scanAgentSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentSession{}, false, nil
	}
	if err != nil {
		return domain.AgentSession{}, false, fmt.Errorf("get agent session %q: %w", sessionID, err)
	}
	return session, true, nil
}

func (r *SQLiteRepository) ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, company_id, agent_id, project, title, mode, archived, working_dir, created_at, updated_at
		FROM agent_sessions
		WHERE (? = '' OR company_id = ?) AND (? = '' OR agent_id = ?)
		ORDER BY updated_at DESC, id DESC
	`, companyID, companyID, agentID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent sessions: %w", err)
	}
	defer rows.Close()
	var sessions []domain.AgentSession
	for rows.Next() {
		session, err := scanAgentSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent sessions: %w", err)
	}
	return sessions, nil
}

func (r *SQLiteRepository) AppendConversationTurn(ctx context.Context, turn domain.ConversationTurn) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append conversation turn %q: %w", turn.ID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_turns (
			id, session_id, task_id, agent_id, model_profile, role, content, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, turn.ID, turn.SessionID, turn.TaskID, turn.AgentID, turn.ModelProfile, string(turn.Role), turn.Content, formatTime(turn.CreatedAt)); err != nil {
		return fmt.Errorf("append conversation turn %q: %w", turn.ID, err)
	}
	if err := indexConversationTurn(ctx, tx, turn); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_sessions
		SET updated_at = ?
		WHERE id = ?
	`, formatTime(turn.CreatedAt), turn.SessionID); err != nil {
		return fmt.Errorf("touch agent session %q: %w", turn.SessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append conversation turn %q: %w", turn.ID, err)
	}
	return nil
}

// AppendConversationTurnIfAbsent inserts a turn only when no turn with the same
// id already exists, returning whether it was inserted. This gives an
// exactly-once write for turns keyed by a deterministic id (e.g. "<taskID>:user"),
// so repeated calls — such as polling the task result endpoint — do not duplicate
// the turn. The session's updated_at is touched only on a real insert. A genuine
// write failure is returned wrapped, never swallowed.
func (r *SQLiteRepository) AppendConversationTurnIfAbsent(ctx context.Context, turn domain.ConversationTurn) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin append conversation turn %q if absent: %w", turn.ID, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO conversation_turns (
			id, session_id, task_id, agent_id, model_profile, role, content, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, turn.ID, turn.SessionID, turn.TaskID, turn.AgentID, turn.ModelProfile, string(turn.Role), turn.Content, formatTime(turn.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("append conversation turn %q if absent: %w", turn.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check inserted conversation turn %q rows affected: %w", turn.ID, err)
	}
	if affected == 0 {
		return false, nil
	}
	if err := indexConversationTurn(ctx, tx, turn); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_sessions
		SET updated_at = ?
		WHERE id = ?
	`, formatTime(turn.CreatedAt), turn.SessionID); err != nil {
		return false, fmt.Errorf("touch agent session %q: %w", turn.SessionID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit append conversation turn %q if absent: %w", turn.ID, err)
	}
	return true, nil
}

func (r *SQLiteRepository) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	query := `
		SELECT id, session_id, task_id, agent_id, model_profile, role, content, created_at
		FROM conversation_turns
		WHERE session_id = ?
		ORDER BY created_at DESC, id DESC
	`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conversation turns for %q: %w", sessionID, err)
	}
	defer rows.Close()
	var reversed []domain.ConversationTurn
	for rows.Next() {
		turn, err := scanConversationTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan conversation turn for %q: %w", sessionID, err)
		}
		reversed = append(reversed, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation turns for %q: %w", sessionID, err)
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

// execer is the ExecContext subset shared by *sql.DB and *sql.Tx, so the FTS
// index write can run inside the same transaction as the source-row insert.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// indexConversationTurn mirrors a conversation turn into the FTS5 index. It runs
// inside the caller's transaction so the index and the source table commit
// together; a failure is returned wrapped rather than swallowed, so a turn is
// never persisted without being searchable.
func indexConversationTurn(ctx context.Context, ex execer, turn domain.ConversationTurn) error {
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO conversation_turns_fts (
			content, turn_id, session_id, task_id, agent_id, role, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, turn.Content, turn.ID, turn.SessionID, turn.TaskID, turn.AgentID, string(turn.Role), formatTime(turn.CreatedAt)); err != nil {
		return fmt.Errorf("index conversation turn %q for search: %w", turn.ID, err)
	}
	return nil
}

// SearchMessages runs an FTS5 full-text query over conversation turn content and
// returns the best-ranked matches (discovery mode of session_search). query must
// be a non-empty FTS5 MATCH expression; an empty query is a caller error. limit
// <= 0 defaults to 20.
func (r *SQLiteRepository) SearchMessages(ctx context.Context, query string, limit int) ([]domain.ConversationTurn, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search messages: query is required")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT turn_id, session_id, task_id, agent_id, role, content, created_at
		FROM conversation_turns_fts
		WHERE conversation_turns_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages %q: %w", query, err)
	}
	defer rows.Close()
	turns := make([]domain.ConversationTurn, 0)
	for rows.Next() {
		var turn domain.ConversationTurn
		var role string
		var createdAt string
		if err := rows.Scan(&turn.ID, &turn.SessionID, &turn.TaskID, &turn.AgentID, &role, &turn.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan search result for %q: %w", query, err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse search result %q created_at: %w", turn.ID, err)
		}
		turn.Role = domain.ConversationRole(role)
		turn.CreatedAt = parsed
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results for %q: %w", query, err)
	}
	return turns, nil
}

// ScrollMessages returns a window of turns centered on aroundID within a session
// (scroll mode of session_search): up to window turns before and after the
// anchor, in chronological order. A missing anchor is a caller error reported
// loudly, not an empty result. window <= 0 defaults to 5.
func (r *SQLiteRepository) ScrollMessages(ctx context.Context, sessionID string, aroundID string, window int) ([]domain.ConversationTurn, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("scroll messages: session_id is required")
	}
	if strings.TrimSpace(aroundID) == "" {
		return nil, fmt.Errorf("scroll messages: around_message_id is required")
	}
	if window <= 0 {
		window = 5
	}
	turns, err := r.ListConversationTurns(ctx, sessionID, 0)
	if err != nil {
		return nil, fmt.Errorf("scroll messages in %q: %w", sessionID, err)
	}
	anchor := -1
	for i := range turns {
		if turns[i].ID == aroundID {
			anchor = i
			break
		}
	}
	if anchor == -1 {
		return nil, fmt.Errorf("scroll messages: anchor %q not found in session %q", aroundID, sessionID)
	}
	start := anchor - window
	start = max(start, 0)
	end := anchor + window + 1
	end = min(end, len(turns))
	return turns[start:end], nil
}

// BrowseSessions returns the most recently updated sessions (browse mode of
// session_search). limit <= 0 defaults to 20.
func (r *SQLiteRepository) BrowseSessions(ctx context.Context, limit int) ([]domain.AgentSession, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, company_id, agent_id, project, title, mode, archived, working_dir, created_at, updated_at
		FROM agent_sessions
		ORDER BY updated_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("browse sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]domain.AgentSession, 0)
	for rows.Next() {
		session, err := scanAgentSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan browsed session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate browsed sessions: %w", err)
	}
	return sessions, nil
}

func (r *SQLiteRepository) SaveAgentMessage(ctx context.Context, message domain.AgentMessage) error {
	if message.Status == "" {
		message.Status = domain.AgentMessageUnread
	}
	if message.Type == "" {
		message.Type = domain.AgentMessageTypeMessage
	}
	if message.ThreadID == "" {
		message.ThreadID = message.TaskID
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_messages (
			id, company_id, task_id, source_event_id, thread_id, from_agent_id, to_agent_id,
			type, status, summary, artifact, created_at, read_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			company_id = excluded.company_id,
			task_id = excluded.task_id,
			source_event_id = excluded.source_event_id,
			thread_id = excluded.thread_id,
			from_agent_id = excluded.from_agent_id,
			to_agent_id = excluded.to_agent_id,
			type = excluded.type,
			status = excluded.status,
			summary = excluded.summary,
			artifact = excluded.artifact,
			created_at = excluded.created_at,
			read_at = excluded.read_at
	`, message.ID, message.CompanyID, message.TaskID, message.SourceEventID, message.ThreadID, message.FromAgentID, message.ToAgentID,
		string(message.Type), string(message.Status), message.Summary, message.Artifact, formatTime(message.CreatedAt), formatTime(message.ReadAt))
	if err != nil {
		return fmt.Errorf("save agent message %q: %w", message.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListAgentMessages(ctx context.Context, query domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	// Defence in depth. Every filter below is "empty matches anything", so a
	// fully unscoped query would return the first rows of the entire table
	// across all agents and tenants. No legitimate caller needs that, and the
	// tool layer already scopes to the caller — refusing here as well means a
	// future caller that forgets cannot silently re-open the hole.
	if strings.TrimSpace(query.CompanyID) == "" &&
		strings.TrimSpace(query.ToAgentID) == "" &&
		strings.TrimSpace(query.FromAgentID) == "" {
		return nil, fmt.Errorf("list agent messages: refusing an unscoped query: company_id, to_agent_id or from_agent_id is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, company_id, task_id, source_event_id, thread_id, from_agent_id, to_agent_id,
			type, status, summary, artifact, created_at, read_at
		FROM agent_messages
		WHERE (? = '' OR company_id = ?)
			AND (? = '' OR task_id = ?)
			AND (? = '' OR thread_id = ?)
			AND (? = '' OR from_agent_id = ?)
			AND (? = '' OR to_agent_id = ?)
			AND (? = '' OR status = ?)
			AND (? = '' OR source_event_id = ?)
		ORDER BY created_at ASC, id ASC
		LIMIT ?
	`, query.CompanyID, query.CompanyID, query.TaskID, query.TaskID, query.ThreadID, query.ThreadID,
		query.FromAgentID, query.FromAgentID, query.ToAgentID, query.ToAgentID, string(query.Status), string(query.Status),
		query.SourceEventID, query.SourceEventID, limit)
	if err != nil {
		return nil, fmt.Errorf("list agent messages: %w", err)
	}
	defer rows.Close()
	var messages []domain.AgentMessage
	for rows.Next() {
		message, err := scanAgentMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent messages: %w", err)
	}
	return messages, nil
}

func (r *SQLiteRepository) MarkAgentMessageRead(ctx context.Context, messageID string, readAt time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE agent_messages
		SET status = ?, read_at = ?
		WHERE id = ?
	`, string(domain.AgentMessageRead), formatTime(readAt), messageID)
	if err != nil {
		return fmt.Errorf("mark agent message %q read: %w", messageID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check agent message %q rows affected: %w", messageID, err)
	}
	if affected == 0 {
		return fmt.Errorf("agent message %q not found", messageID)
	}
	return nil
}

func (r *SQLiteRepository) SaveTaskRun(ctx context.Context, run domain.TaskRun) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_runs (id, task_id, agent_id, started_at, ended_at, result)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			task_id = excluded.task_id,
			agent_id = excluded.agent_id,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			result = excluded.result
	`, run.ID, run.TaskID, run.AgentID, formatTime(run.StartedAt), formatTime(run.EndedAt), run.Result)
	if err != nil {
		return fmt.Errorf("save task run %q: %w", run.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListTaskRuns(ctx context.Context, taskID string) ([]domain.TaskRun, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, agent_id, started_at, ended_at, result
		FROM task_runs
		WHERE task_id = ?
		ORDER BY started_at, id
	`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task runs for %q: %w", taskID, err)
	}
	defer rows.Close()

	var runs []domain.TaskRun
	for rows.Next() {
		var run domain.TaskRun
		var startedAt string
		var endedAt string
		if err := rows.Scan(&run.ID, &run.TaskID, &run.AgentID, &startedAt, &endedAt, &run.Result); err != nil {
			return nil, fmt.Errorf("scan task run for %q: %w", taskID, err)
		}
		parsedStartedAt, err := parseTime(startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse task run %q started_at: %w", run.ID, err)
		}
		parsedEndedAt, err := parseTime(endedAt)
		if err != nil {
			return nil, fmt.Errorf("parse task run %q ended_at: %w", run.ID, err)
		}
		run.StartedAt = parsedStartedAt
		run.EndedAt = parsedEndedAt
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task runs for %q: %w", taskID, err)
	}
	return runs, nil
}

func (r *SQLiteRepository) AppendAuditEvent(ctx context.Context, event domain.AuditEvent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, request_id, subject_type, subject_id, action, hash, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, event.ID, event.RequestID, event.SubjectType, event.SubjectID, event.Action, event.Hash, formatTime(event.CreatedAt))
	if err != nil {
		return fmt.Errorf("append audit event %q: %w", event.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListAuditEvents(ctx context.Context) ([]domain.AuditEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, request_id, subject_type, subject_id, action, hash, created_at
		FROM audit_events
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var events []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var createdAt string
		if err := rows.Scan(&event.ID, &event.RequestID, &event.SubjectType, &event.SubjectID, &event.Action, &event.Hash, &createdAt); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse audit event %q created_at: %w", event.ID, err)
		}
		event.CreatedAt = parsed
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit events: %w", err)
	}
	return events, nil
}

func (r *SQLiteRepository) AppendRuntimeEvent(ctx context.Context, event domain.RuntimeEvent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO runtime_events (type, task_id, message, created_at)
		VALUES (?, ?, ?, ?)
	`, event.Type, event.TaskID, event.Message, formatTime(event.CreatedAt))
	if err != nil {
		return fmt.Errorf("append runtime event %q for %q: %w", event.Type, event.TaskID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListRuntimeEvents(ctx context.Context) ([]domain.RuntimeEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT type, task_id, message, created_at
		FROM runtime_events
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list runtime events: %w", err)
	}
	defer rows.Close()

	var events []domain.RuntimeEvent
	for rows.Next() {
		var event domain.RuntimeEvent
		var createdAt string
		if err := rows.Scan(&event.Type, &event.TaskID, &event.Message, &createdAt); err != nil {
			return nil, fmt.Errorf("scan runtime event: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse runtime event %q created_at: %w", event.Type, err)
		}
		event.CreatedAt = parsed
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime events: %w", err)
	}
	return events, nil
}

func (r *SQLiteRepository) SaveSkill(ctx context.Context, s skill.Skill) error {
	tags, err := marshalStrings(s.Tags)
	if err != nil {
		return fmt.Errorf("marshal skill %q tags: %w", s.ID, err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO skills (
			id, name, source, version, path, hash, risk_level, status, tags, summary, content
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, version) DO UPDATE SET
			name = excluded.name,
			source = excluded.source,
			path = excluded.path,
			hash = excluded.hash,
			risk_level = excluded.risk_level,
			status = excluded.status,
			tags = excluded.tags,
			summary = excluded.summary,
			content = excluded.content
	`, s.ID, s.Name, string(s.Source), s.Version, s.Path, s.Hash, string(s.RiskLevel), string(s.Status), tags, s.Summary, s.Content)
	if err != nil {
		return fmt.Errorf("save skill %q: %w", s.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) GetSkill(ctx context.Context, id string, version string) (skill.Skill, bool, error) {
	var s skill.Skill
	var source, riskLevel, status, tags string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, name, source, version, path, hash, risk_level, status, tags, summary, content
		FROM skills
		WHERE id = ? AND version = ?
	`, id, version).Scan(&s.ID, &s.Name, &source, &s.Version, &s.Path, &s.Hash, &riskLevel, &status, &tags, &s.Summary, &s.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return skill.Skill{}, false, nil
	}
	if err != nil {
		return skill.Skill{}, false, fmt.Errorf("get skill %q@%q: %w", id, version, err)
	}
	parsedTags, err := unmarshalStrings(tags)
	if err != nil {
		return skill.Skill{}, false, fmt.Errorf("unmarshal skill %q tags: %w", id, err)
	}
	s.Source = skill.Source(source)
	s.RiskLevel = skill.RiskLevel(riskLevel)
	s.Status = skill.Status(status)
	s.Tags = parsedTags
	return s, true, nil
}

// ListSkills returns every persisted skill row. It backs the Curator lifecycle
// sweep, which needs the full set to age idle skills. Rows are returned in id,
// version order for deterministic sweeps.
func (r *SQLiteRepository) ListSkills(ctx context.Context) ([]skill.Skill, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, source, version, path, hash, risk_level, status, tags, summary, content
		FROM skills
		ORDER BY id, version
	`)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	skills := make([]skill.Skill, 0)
	for rows.Next() {
		var s skill.Skill
		var source, riskLevel, status, tags string
		if err := rows.Scan(&s.ID, &s.Name, &source, &s.Version, &s.Path, &s.Hash, &riskLevel, &status, &tags, &s.Summary, &s.Content); err != nil {
			return nil, fmt.Errorf("scan skill row: %w", err)
		}
		parsedTags, err := unmarshalStrings(tags)
		if err != nil {
			return nil, fmt.Errorf("unmarshal skill %q tags: %w", s.ID, err)
		}
		s.Source = skill.Source(source)
		s.RiskLevel = skill.RiskLevel(riskLevel)
		s.Status = skill.Status(status)
		s.Tags = parsedTags
		skills = append(skills, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate skill rows: %w", err)
	}
	return skills, nil
}

func (r *SQLiteRepository) SaveSkillScanFindings(ctx context.Context, skillID string, findings []skill.SkillScanFinding) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save scan findings for %q: %w", skillID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM skill_scan_findings WHERE skill_id = ?`, skillID); err != nil {
		return fmt.Errorf("delete scan findings for %q: %w", skillID, err)
	}
	for _, finding := range findings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO skill_scan_findings (skill_id, rule_id, severity, message, location)
			VALUES (?, ?, ?, ?, ?)
		`, finding.SkillID, finding.RuleID, string(finding.Severity), finding.Message, finding.Location); err != nil {
			return fmt.Errorf("insert scan finding for %q: %w", skillID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit scan findings for %q: %w", skillID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListSkillScanFindings(ctx context.Context, skillID string) ([]skill.SkillScanFinding, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT skill_id, rule_id, severity, message, location
		FROM skill_scan_findings
		WHERE skill_id = ?
		ORDER BY id
	`, skillID)
	if err != nil {
		return nil, fmt.Errorf("list skill scan findings for %q: %w", skillID, err)
	}
	defer rows.Close()
	var findings []skill.SkillScanFinding
	for rows.Next() {
		var finding skill.SkillScanFinding
		var severity string
		if err := rows.Scan(&finding.SkillID, &finding.RuleID, &severity, &finding.Message, &finding.Location); err != nil {
			return nil, fmt.Errorf("scan skill finding for %q: %w", skillID, err)
		}
		finding.Severity = skill.Severity(severity)
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate skill findings for %q: %w", skillID, err)
	}
	return findings, nil
}

func (r *SQLiteRepository) SaveGene(ctx context.Context, gene memory.Gene) error {
	tags, err := marshalStrings(gene.Tags)
	if err != nil {
		return fmt.Errorf("marshal gene %q tags: %w", gene.ID, err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO capability_assets (
			id, asset_type, version, status, tags, match_text, use_when, plan, avoid, constraints_text, validation, success_rate, success_count, failure_count, updated_at
		) VALUES (?, 'gene', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, asset_type) DO UPDATE SET
			version = excluded.version,
			status = excluded.status,
			tags = excluded.tags,
			match_text = excluded.match_text,
			use_when = excluded.use_when,
			plan = excluded.plan,
			avoid = excluded.avoid,
			constraints_text = excluded.constraints_text,
			validation = excluded.validation,
			success_rate = excluded.success_rate,
			success_count = excluded.success_count,
			failure_count = excluded.failure_count,
			updated_at = excluded.updated_at
	`, gene.ID, gene.Version, string(gene.Status), tags, gene.Match, gene.UseWhen, gene.Plan, gene.Avoid, gene.Constraints, gene.Validation, gene.SuccessRate, gene.SuccessCount, gene.FailureCount, formatTime(gene.UpdatedAt))
	if err != nil {
		return fmt.Errorf("save gene %q: %w", gene.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListGenes(ctx context.Context) ([]memory.Gene, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, version, status, tags, match_text, use_when, plan, avoid, constraints_text, validation, success_rate, success_count, failure_count, updated_at
		FROM capability_assets
		WHERE asset_type = 'gene'
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("list genes: %w", err)
	}
	defer rows.Close()
	var genes []memory.Gene
	for rows.Next() {
		var gene memory.Gene
		var status, tags, updatedAt string
		if err := rows.Scan(&gene.ID, &gene.Version, &status, &tags, &gene.Match, &gene.UseWhen, &gene.Plan, &gene.Avoid, &gene.Constraints, &gene.Validation, &gene.SuccessRate, &gene.SuccessCount, &gene.FailureCount, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan gene: %w", err)
		}
		parsedTags, err := unmarshalStrings(tags)
		if err != nil {
			return nil, fmt.Errorf("unmarshal gene %q tags: %w", gene.ID, err)
		}
		parsedUpdatedAt, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse gene %q updated_at: %w", gene.ID, err)
		}
		gene.Status = memory.GeneStatus(status)
		gene.Tags = parsedTags
		gene.UpdatedAt = parsedUpdatedAt
		genes = append(genes, gene)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate genes: %w", err)
	}
	return genes, nil
}

func (r *SQLiteRepository) SaveCapsule(ctx context.Context, capsule memory.Capsule) error {
	geneIDs, err := marshalStrings(capsule.GeneIDs)
	if err != nil {
		return fmt.Errorf("marshal capsule %q gene ids: %w", capsule.ID, err)
	}
	tags, err := marshalStrings(capsule.Tags)
	if err != nil {
		return fmt.Errorf("marshal capsule %q tags: %w", capsule.ID, err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO capability_assets (
			id, asset_type, gene_ids, query, tags, outcome, success_count, confidence, created_at
		) VALUES (?, 'capsule', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, asset_type) DO UPDATE SET
			gene_ids = excluded.gene_ids,
			query = excluded.query,
			tags = excluded.tags,
			outcome = excluded.outcome,
			success_count = excluded.success_count,
			confidence = excluded.confidence,
			created_at = excluded.created_at
	`, capsule.ID, geneIDs, capsule.Query, tags, capsule.Outcome, capsule.SuccessCount, capsule.Confidence, formatTime(capsule.CreatedAt))
	if err != nil {
		return fmt.Errorf("save capsule %q: %w", capsule.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListCapsules(ctx context.Context) ([]memory.Capsule, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, gene_ids, query, tags, outcome, success_count, confidence, created_at
		FROM capability_assets
		WHERE asset_type = 'capsule'
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("list capsules: %w", err)
	}
	defer rows.Close()
	var capsules []memory.Capsule
	for rows.Next() {
		var capsule memory.Capsule
		var geneIDs, tags, createdAt string
		if err := rows.Scan(&capsule.ID, &geneIDs, &capsule.Query, &tags, &capsule.Outcome, &capsule.SuccessCount, &capsule.Confidence, &createdAt); err != nil {
			return nil, fmt.Errorf("scan capsule: %w", err)
		}
		parsedGeneIDs, err := unmarshalStrings(geneIDs)
		if err != nil {
			return nil, fmt.Errorf("unmarshal capsule %q gene ids: %w", capsule.ID, err)
		}
		parsedTags, err := unmarshalStrings(tags)
		if err != nil {
			return nil, fmt.Errorf("unmarshal capsule %q tags: %w", capsule.ID, err)
		}
		parsedCreatedAt, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse capsule %q created_at: %w", capsule.ID, err)
		}
		capsule.GeneIDs = parsedGeneIDs
		capsule.Tags = parsedTags
		capsule.CreatedAt = parsedCreatedAt
		capsules = append(capsules, capsule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capsules: %w", err)
	}
	return capsules, nil
}

func (r *SQLiteRepository) AppendEvolutionEvent(ctx context.Context, event evolution.EvolutionEvent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO evolution_events (
			event_id, cycle_id, stage, agent_id, asset_id, evidence_hash, decision, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, event.EventID, event.CycleID, string(event.Stage), event.AgentID, event.AssetID, event.EvidenceHash, string(event.Decision), formatTime(event.CreatedAt))
	if err != nil {
		return fmt.Errorf("append evolution event %q: %w", event.EventID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListEvolutionEvents(ctx context.Context, cycleID string) ([]evolution.EvolutionEvent, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT event_id, cycle_id, stage, agent_id, asset_id, evidence_hash, decision, created_at
		FROM evolution_events
		WHERE cycle_id = ?
		ORDER BY created_at, event_id
	`, cycleID)
	if err != nil {
		return nil, fmt.Errorf("list evolution events for %q: %w", cycleID, err)
	}
	defer rows.Close()
	var events []evolution.EvolutionEvent
	for rows.Next() {
		var event evolution.EvolutionEvent
		var stage, decision, createdAt string
		if err := rows.Scan(&event.EventID, &event.CycleID, &stage, &event.AgentID, &event.AssetID, &event.EvidenceHash, &decision, &createdAt); err != nil {
			return nil, fmt.Errorf("scan evolution event for %q: %w", cycleID, err)
		}
		parsedCreatedAt, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse evolution event %q created_at: %w", event.EventID, err)
		}
		event.Stage = evolution.EvolutionStage(stage)
		event.Decision = evolution.EvolutionDecision(decision)
		event.CreatedAt = parsedCreatedAt
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evolution events for %q: %w", cycleID, err)
	}
	return events, nil
}

func (r *SQLiteRepository) SaveWorkflowState(ctx context.Context, def workflow.Definition, result workflow.Result) error {
	definitionJSON, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal workflow definition %q: %w", def.ID, err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal workflow result %q: %w", result.WorkflowID, err)
	}
	workflowID := result.WorkflowID
	if workflowID == "" {
		workflowID = def.ID
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO workflow_states (id, status, definition_json, result_json, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			definition_json = excluded.definition_json,
			result_json = excluded.result_json,
			updated_at = excluded.updated_at
	`, workflowID, string(result.Status), string(definitionJSON), string(resultJSON), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("save workflow state %q: %w", workflowID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListWaitingWorkflowStates(ctx context.Context) ([]WorkflowState, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT definition_json, result_json, updated_at
		FROM workflow_states
		WHERE status IN (?, ?)
		ORDER BY updated_at, id
	`, string(workflow.StatusWaitingApproval), string(workflow.StatusWaitingEvent))
	if err != nil {
		return nil, fmt.Errorf("list waiting workflow states: %w", err)
	}
	defer rows.Close()

	var states []WorkflowState
	for rows.Next() {
		var definitionJSON string
		var resultJSON string
		var updatedAt string
		if err := rows.Scan(&definitionJSON, &resultJSON, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan waiting workflow state: %w", err)
		}
		state, err := decodeWorkflowState(definitionJSON, resultJSON, updatedAt)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate waiting workflow states: %w", err)
	}
	return states, nil
}

func (r *SQLiteRepository) GetWorkflowState(ctx context.Context, workflowID string) (WorkflowState, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT definition_json, result_json, updated_at
		FROM workflow_states
		WHERE id = ?
	`, workflowID)
	var definitionJSON string
	var resultJSON string
	var updatedAt string
	if err := row.Scan(&definitionJSON, &resultJSON, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowState{}, false, nil
		}
		return WorkflowState{}, false, fmt.Errorf("get workflow state %q: %w", workflowID, err)
	}
	state, err := decodeWorkflowState(definitionJSON, resultJSON, updatedAt)
	if err != nil {
		return WorkflowState{}, false, err
	}
	return state, true, nil
}

func (r *SQLiteRepository) AppendQualityEvalRun(ctx context.Context, record quality.EvalRunRecord) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO quality_history (
			record_type, record_id, agent_id, task_id, component, status, reason, score, created_at
		) VALUES ('eval_run', ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.AgentID, record.TaskID, record.Component, string(record.Status), record.Reason, record.Score, formatTime(record.CreatedAt))
	if err != nil {
		return fmt.Errorf("append quality eval run %q: %w", record.ID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListQualityEvalRuns(ctx context.Context, query quality.TrendQuery) ([]quality.EvalRunRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT record_id, agent_id, task_id, component, status, reason, score, created_at
		FROM quality_history
		WHERE record_type = 'eval_run'
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list quality eval runs: %w", err)
	}
	defer rows.Close()
	var records []quality.EvalRunRecord
	for rows.Next() {
		var record quality.EvalRunRecord
		var status string
		var createdAt string
		if err := rows.Scan(&record.ID, &record.AgentID, &record.TaskID, &record.Component, &status, &record.Reason, &record.Score, &createdAt); err != nil {
			return nil, fmt.Errorf("scan quality eval run: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse quality eval run %q created_at: %w", record.ID, err)
		}
		record.Status = quality.EvalStatus(status)
		record.CreatedAt = parsed
		if matchesQualityEvalQuery(record, query) {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quality eval runs: %w", err)
	}
	return records, nil
}

func (r *SQLiteRepository) AppendTrustScoreSnapshot(ctx context.Context, snapshot quality.TrustScoreSnapshot) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO quality_history (
			record_type, agent_id, decision, reason, score, created_at
		) VALUES ('trust_snapshot', ?, ?, ?, ?, ?)
	`, snapshot.AgentID, string(snapshot.Decision), snapshot.Reason, snapshot.Score, formatTime(snapshot.CreatedAt))
	if err != nil {
		return fmt.Errorf("append trust score snapshot for %q: %w", snapshot.AgentID, err)
	}
	return nil
}

func (r *SQLiteRepository) ListTrustScoreSnapshots(ctx context.Context, agentID string) ([]quality.TrustScoreSnapshot, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT agent_id, decision, reason, score, created_at
		FROM quality_history
		WHERE record_type = 'trust_snapshot'
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list trust score snapshots: %w", err)
	}
	defer rows.Close()
	var snapshots []quality.TrustScoreSnapshot
	for rows.Next() {
		var snapshot quality.TrustScoreSnapshot
		var decision string
		var createdAt string
		if err := rows.Scan(&snapshot.AgentID, &decision, &snapshot.Reason, &snapshot.Score, &createdAt); err != nil {
			return nil, fmt.Errorf("scan trust score snapshot: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse trust score snapshot created_at: %w", err)
		}
		snapshot.Decision = quality.TrustDecision(decision)
		snapshot.CreatedAt = parsed
		if agentID == "" || snapshot.AgentID == agentID {
			snapshots = append(snapshots, snapshot)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trust score snapshots: %w", err)
	}
	return snapshots, nil
}

func (r *SQLiteRepository) AppendDegradationDecision(ctx context.Context, decision quality.DegradationDecision) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO quality_history (
			record_type, agent_id, component, decision, reason, quality_drop, created_at
		) VALUES ('degradation_decision', ?, ?, ?, ?, ?, ?)
	`, decision.AgentID, decision.Component, decision.Decision, decision.Reason, decision.QualityDrop, formatTime(decision.CreatedAt))
	if err != nil {
		return fmt.Errorf("append degradation decision for %q/%q: %w", decision.AgentID, decision.Component, err)
	}
	return nil
}

func (r *SQLiteRepository) ListDegradationDecisions(ctx context.Context, query quality.TrendQuery) ([]quality.DegradationDecision, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT agent_id, component, decision, reason, quality_drop, created_at
		FROM quality_history
		WHERE record_type = 'degradation_decision'
		ORDER BY created_at, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list degradation decisions: %w", err)
	}
	defer rows.Close()
	var decisions []quality.DegradationDecision
	for rows.Next() {
		var decision quality.DegradationDecision
		var createdAt string
		if err := rows.Scan(&decision.AgentID, &decision.Component, &decision.Decision, &decision.Reason, &decision.QualityDrop, &createdAt); err != nil {
			return nil, fmt.Errorf("scan degradation decision: %w", err)
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse degradation decision created_at: %w", err)
		}
		decision.CreatedAt = parsed
		if matchesQualityDegradationQuery(decision, query) {
			decisions = append(decisions, decision)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate degradation decisions: %w", err)
	}
	return decisions, nil
}

func (r *SQLiteRepository) migrate(ctx context.Context) error {
	for _, stmt := range schemaStatements {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if err := r.applyColumnMigrations(ctx); err != nil {
		return err
	}
	// One-time FTS backfill: turns written before schema version 4 predate the
	// conversation_turns_fts index. Read the prior recorded version now that
	// schema_migrations exists (0 on a fresh DB) and, when upgrading from below 4,
	// index any conversation turns not yet present. The backfill is idempotent
	// (it skips already-indexed rows), so a fresh DB with no turns is a no-op.
	priorVersion, err := r.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if priorVersion < 4 {
		if _, err := r.BackfillConversationTurnsFTS(ctx); err != nil {
			return err
		}
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO schema_migrations (version, applied_at)
		VALUES (?, ?)
	`, CurrentSchemaVersion, formatTime(time.Now())); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	return nil
}

// BackfillConversationTurnsFTS indexes every conversation turn that is not yet
// present in the FTS table, returning how many rows were added. It is idempotent
// via a NOT EXISTS guard, so re-running it never double-indexes a turn. It backs
// the one-time schema v4 upgrade and can be called directly to repair the index.
func (r *SQLiteRepository) BackfillConversationTurnsFTS(ctx context.Context) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO conversation_turns_fts (
			content, turn_id, session_id, task_id, agent_id, role, created_at
		)
		SELECT t.content, t.id, t.session_id, t.task_id, t.agent_id, t.role, t.created_at
		FROM conversation_turns t
		WHERE NOT EXISTS (
			SELECT 1 FROM conversation_turns_fts f WHERE f.turn_id = t.id
		)
	`)
	if err != nil {
		return 0, fmt.Errorf("backfill conversation turns fts: %w", err)
	}
	added, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("backfill conversation turns fts rows affected: %w", err)
	}
	return int(added), nil
}

// columnMigration describes one additive column that CREATE TABLE IF NOT EXISTS
// cannot apply to a pre-existing table. Each is run via ALTER TABLE on every
// startup; an already-applied column makes SQLite return a "duplicate column
// name" error, which is the documented idempotency signal and is the ONLY error
// we treat as "already migrated". Any other ALTER failure is propagated so the
// startup fails loudly instead of silently running on an out-of-date schema.
type columnMigration struct {
	table  string
	column string
	stmt   string
}

var columnMigrations = []columnMigration{
	{
		table:  "agent_sessions",
		column: "project",
		stmt:   `ALTER TABLE agent_sessions ADD COLUMN project TEXT NOT NULL DEFAULT ''`,
	},
	{
		table:  "tasks",
		column: "session_id",
		stmt:   `ALTER TABLE tasks ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	},
	{
		table:  "agent_sessions",
		column: "archived",
		stmt:   `ALTER TABLE agent_sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0`,
	},
	{
		table:  "agent_sessions",
		column: "mode",
		stmt:   `ALTER TABLE agent_sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'auto'`,
	},
	{
		table:  "agent_sessions",
		column: "working_dir",
		stmt:   `ALTER TABLE agent_sessions ADD COLUMN working_dir TEXT NOT NULL DEFAULT ''`,
	},
}

// applyColumnMigrations runs the additive ALTER TABLE migrations idempotently.
// It tolerates only the "duplicate column name" error (the column already
// exists), which is the legitimate already-migrated state; every other error is
// wrapped and returned so the failure is loud and locatable.
func (r *SQLiteRepository) applyColumnMigrations(ctx context.Context) error {
	for _, migration := range columnMigrations {
		_, err := r.db.ExecContext(ctx, migration.stmt)
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "duplicate column name") {
			continue
		}
		return fmt.Errorf("add column %s.%s: %w", migration.table, migration.column, err)
	}
	return nil
}

// boolToInt maps a Go bool to the 0/1 integer SQLite stores, since SQLite has no
// native boolean type. The matching read converts a nonzero column back to true.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func marshalStrings(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalStrings(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil, err
	}
	return values, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAgentSession(row scanner) (domain.AgentSession, error) {
	var session domain.AgentSession
	var archived int
	var createdAt string
	var updatedAt string
	if err := row.Scan(&session.ID, &session.CompanyID, &session.AgentID, &session.Project, &session.Title, &session.Mode, &archived, &session.WorkingDir, &createdAt, &updatedAt); err != nil {
		return domain.AgentSession{}, err
	}
	session.Archived = archived != 0
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("parse session %q created_at: %w", session.ID, err)
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return domain.AgentSession{}, fmt.Errorf("parse session %q updated_at: %w", session.ID, err)
	}
	session.CreatedAt = parsedCreatedAt
	session.UpdatedAt = parsedUpdatedAt
	return session, nil
}

func scanConversationTurn(row scanner) (domain.ConversationTurn, error) {
	var turn domain.ConversationTurn
	var role string
	var createdAt string
	if err := row.Scan(&turn.ID, &turn.SessionID, &turn.TaskID, &turn.AgentID, &turn.ModelProfile, &role, &turn.Content, &createdAt); err != nil {
		return domain.ConversationTurn{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return domain.ConversationTurn{}, fmt.Errorf("parse conversation turn %q created_at: %w", turn.ID, err)
	}
	turn.Role = domain.ConversationRole(role)
	turn.CreatedAt = parsedCreatedAt
	return turn, nil
}

func scanAgentMessage(row scanner) (domain.AgentMessage, error) {
	var message domain.AgentMessage
	var messageType string
	var status string
	var createdAt string
	var readAt string
	if err := row.Scan(
		&message.ID,
		&message.CompanyID,
		&message.TaskID,
		&message.SourceEventID,
		&message.ThreadID,
		&message.FromAgentID,
		&message.ToAgentID,
		&messageType,
		&status,
		&message.Summary,
		&message.Artifact,
		&createdAt,
		&readAt,
	); err != nil {
		return domain.AgentMessage{}, err
	}
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return domain.AgentMessage{}, fmt.Errorf("parse agent message %q created_at: %w", message.ID, err)
	}
	parsedReadAt, err := parseTime(readAt)
	if err != nil {
		return domain.AgentMessage{}, fmt.Errorf("parse agent message %q read_at: %w", message.ID, err)
	}
	message.Type = domain.AgentMessageType(messageType)
	message.Status = domain.AgentMessageStatus(status)
	message.CreatedAt = parsedCreatedAt
	message.ReadAt = parsedReadAt
	return message, nil
}

func decodeWorkflowState(definitionJSON string, resultJSON string, updatedAt string) (WorkflowState, error) {
	var state WorkflowState
	if err := json.Unmarshal([]byte(definitionJSON), &state.Definition); err != nil {
		return WorkflowState{}, fmt.Errorf("unmarshal workflow definition: %w", err)
	}
	if err := json.Unmarshal([]byte(resultJSON), &state.Result); err != nil {
		return WorkflowState{}, fmt.Errorf("unmarshal workflow result: %w", err)
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return WorkflowState{}, fmt.Errorf("parse workflow state updated_at: %w", err)
	}
	state.UpdatedAt = parsedUpdatedAt
	return state, nil
}

func matchesQualityEvalQuery(record quality.EvalRunRecord, query quality.TrendQuery) bool {
	if query.AgentID != "" && record.AgentID != query.AgentID {
		return false
	}
	if query.TaskID != "" && record.TaskID != query.TaskID {
		return false
	}
	if query.Component != "" && record.Component != query.Component {
		return false
	}
	return matchesQualityRange(record.CreatedAt, query)
}

func matchesQualityDegradationQuery(decision quality.DegradationDecision, query quality.TrendQuery) bool {
	if query.AgentID != "" && decision.AgentID != query.AgentID {
		return false
	}
	if query.Component != "" && decision.Component != query.Component {
		return false
	}
	return matchesQualityRange(decision.CreatedAt, query)
}

func matchesQualityRange(at time.Time, query quality.TrendQuery) bool {
	if !query.Since.IsZero() && at.Before(query.Since) {
		return false
	}
	if !query.Until.IsZero() && at.After(query.Until) {
		return false
	}
	return true
}

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		company_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		session_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		input TEXT NOT NULL,
		max_iterations INTEGER NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS task_locks (
		task_id TEXT PRIMARY KEY,
		owner_id TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS agent_sessions (
		id TEXT PRIMARY KEY,
		company_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		project TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'auto',
		archived INTEGER NOT NULL DEFAULT 0,
		working_dir TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS conversation_turns (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		task_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		model_profile TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	// conversation_turns_fts is a full-text index over conversation turn content,
	// backing the session_search tool (discovery mode). The non-content columns
	// are UNINDEXED: they are stored for retrieval only, not tokenized. Rows are
	// written alongside conversation_turns in the same transaction so the index
	// never drifts from the source table. Turns written before this table existed
	// are not backfilled; search covers turns recorded from schema version 4 on.
	`CREATE VIRTUAL TABLE IF NOT EXISTS conversation_turns_fts USING fts5(
		content,
		turn_id UNINDEXED,
		session_id UNINDEXED,
		task_id UNINDEXED,
		agent_id UNINDEXED,
		role UNINDEXED,
		created_at UNINDEXED
	)`,
	`CREATE TABLE IF NOT EXISTS agent_messages (
		id TEXT PRIMARY KEY,
		company_id TEXT NOT NULL,
		task_id TEXT NOT NULL,
		source_event_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL,
		from_agent_id TEXT NOT NULL,
		to_agent_id TEXT NOT NULL,
		type TEXT NOT NULL,
		status TEXT NOT NULL,
		summary TEXT NOT NULL,
		artifact TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		read_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_messages_recipient_status_created
		ON agent_messages (company_id, to_agent_id, status, created_at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_messages_task_created
		ON agent_messages (company_id, task_id, created_at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_messages_source_event
		ON agent_messages (source_event_id)`,
	`CREATE TABLE IF NOT EXISTS task_runs (
		id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT NOT NULL,
		result TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS audit_events (
		id TEXT PRIMARY KEY,
		request_id TEXT NOT NULL,
		subject_type TEXT NOT NULL,
		subject_id TEXT NOT NULL,
		action TEXT NOT NULL,
		hash TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS runtime_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		type TEXT NOT NULL,
		task_id TEXT NOT NULL,
		message TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS skills (
		id TEXT NOT NULL,
		name TEXT NOT NULL,
		source TEXT NOT NULL,
		version TEXT NOT NULL,
		path TEXT NOT NULL,
		hash TEXT NOT NULL,
		risk_level TEXT NOT NULL,
		status TEXT NOT NULL,
		tags TEXT NOT NULL,
		summary TEXT NOT NULL,
		content TEXT NOT NULL,
		PRIMARY KEY (id, version)
	)`,
	`CREATE TABLE IF NOT EXISTS skill_scan_findings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		skill_id TEXT NOT NULL,
		rule_id TEXT NOT NULL,
		severity TEXT NOT NULL,
		message TEXT NOT NULL,
		location TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS capability_assets (
		id TEXT NOT NULL,
		asset_type TEXT NOT NULL,
		version TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		gene_ids TEXT NOT NULL DEFAULT '[]',
		query TEXT NOT NULL DEFAULT '',
		tags TEXT NOT NULL DEFAULT '[]',
		match_text TEXT NOT NULL DEFAULT '',
		use_when TEXT NOT NULL DEFAULT '',
		plan TEXT NOT NULL DEFAULT '',
		avoid TEXT NOT NULL DEFAULT '',
		constraints_text TEXT NOT NULL DEFAULT '',
		validation TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		success_rate REAL NOT NULL DEFAULT 0,
		success_count INTEGER NOT NULL DEFAULT 0,
		failure_count INTEGER NOT NULL DEFAULT 0,
		confidence REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (id, asset_type)
	)`,
	`CREATE TABLE IF NOT EXISTS evolution_events (
		event_id TEXT PRIMARY KEY,
		cycle_id TEXT NOT NULL,
		stage TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		asset_id TEXT NOT NULL,
		evidence_hash TEXT NOT NULL,
		decision TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS workflow_states (
		id TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		definition_json TEXT NOT NULL,
		result_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS quality_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		record_type TEXT NOT NULL,
		record_id TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		component TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		decision TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		score REAL NOT NULL DEFAULT 0,
		quality_drop REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`,
}
