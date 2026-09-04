package domain

import (
	"fmt"
	"strings"
	"time"
)

type AgentStatus string

const (
	AgentActive AgentStatus = "active"
)

type TaskStatus string

const (
	TaskPending       TaskStatus = "pending"
	TaskAssigned      TaskStatus = "assigned"
	TaskRunning       TaskStatus = "running"
	TaskQualityReview TaskStatus = "quality_review"
	TaskDone          TaskStatus = "done"
	TaskFailed        TaskStatus = "failed"
	TaskSuspended     TaskStatus = "suspended"
	TaskCancelled     TaskStatus = "cancelled"
)

// 会话/任务工作模式。Manual 把有副作用工具挡在人工审批后；Plan 只提供只读工具、
// 产出计划而无副作用；Auto 是默认的不受限行为。以裸 string 存在 Session/Task 上，
// 便于 JSON/DB 平凡往返。
const (
	ModeManual = "manual"
	ModePlan   = "plan"
	ModeAuto   = "auto"
)

// NormalizeMode 校验并规范化一个原始 mode 字符串。空/空白值是合法默认（auto）。
// 已识别值原样返回。其余任何值被拒绝（ok=false），使调用方 fail-loud 而非把未知
// mode 静默转成 auto。
func NormalizeMode(raw string) (mode string, ok bool) {
	switch strings.TrimSpace(raw) {
	case "":
		return ModeAuto, true
	case ModeManual:
		return ModeManual, true
	case ModePlan:
		return ModePlan, true
	case ModeAuto:
		return ModeAuto, true
	default:
		return "", false
	}
}

type Agent struct {
	ID               string      `json:"id"`
	CompanyID        string      `json:"company_id"`
	Role             string      `json:"role"`
	Status           AgentStatus `json:"status"`
	ModelPolicy      string      `json:"model_policy"`
	PermissionPolicy string      `json:"permission_policy"`
}

type Task struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
	Mode      string `json:"mode,omitempty"`
	// WorkingDir is the host filesystem directory this task's session is bound
	// to, if any. When set, session state lives under <WorkingDir>/.stardust
	// (see sessionstate.SessionBase) instead of the workspace root. Empty means
	// unbound (uses the workspace root).
	WorkingDir    string     `json:"working_dir,omitempty"`
	Status        TaskStatus `json:"status"`
	Input         string     `json:"input"`
	MaxIterations int        `json:"max_iterations"`
	CreatedAt     time.Time  `json:"created_at"`
	// Images carries optional multimodal inputs as data-URI strings
	// (e.g. "data:image/png;base64,..."). It is a task-level input visible to
	// every inference round. Empty when the task is text-only.
	Images []string `json:"images,omitempty"`
}

type TaskRun struct {
	ID               string    `json:"id"`
	TaskID           string    `json:"task_id"`
	AgentID          string    `json:"agent_id"`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at"`
	Result           string    `json:"result"`
	ReasoningSummary string    `json:"reasoning_summary,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	CachedTokens     int       `json:"cached_tokens,omitempty"`
	TotalTokens      int       `json:"total_tokens,omitempty"`
	// GeneratedFiles are workspace-relative paths of files the task produced via
	// write_file. Empty when the task wrote no files.
	GeneratedFiles []string `json:"generated_files,omitempty"`
}

type ToolCall struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
	RiskLevel string            `json:"risk_level"`
}

// GuardedToolName returns the tool name the runaway guards must count for
// call: the tool the model actually reached, not the protocol wrapper it
// reached it through.
//
// Under the lazy protocol every real tool arrives as call_tool with the real
// name in arguments.tool_name. Counting call.Name there collapses every
// distinct tool onto one counter, so the per-tool cap degrades into a global
// one that cuts healthy runs short, and the cap/failure messages blame a
// wrapper the model cannot stop using.
//
// A call_tool with no tool_name falls back to the wrapper name: dispatch
// rejects it anyway, and attributing it to the empty string would merge every
// malformed call into one nameless counter.
func GuardedToolName(call ToolCall) string {
	const metaToolCallTool = "call_tool"
	if call.Name != metaToolCallTool {
		return call.Name
	}
	if wrapped := strings.TrimSpace(call.Arguments["tool_name"]); wrapped != "" {
		return wrapped
	}
	return call.Name
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error"`
}

type AuditEvent struct {
	ID          string    `json:"id"`
	RequestID   string    `json:"request_id"`
	SubjectType string    `json:"subject_type"`
	SubjectID   string    `json:"subject_id"`
	Action      string    `json:"action"`
	Hash        string    `json:"hash"`
	CreatedAt   time.Time `json:"created_at"`
	// Origin attributes the event to whoever initiated it: "agent" for the
	// agent's own work, "delegate:depth-N" for a delegated sub-run, and
	// "plugin:<name>" once a contributor drives calls of its own. Without it a
	// forensic pass over a time window cannot tell whose calls it is reading.
	Origin string `json:"origin,omitempty"`
	// Token counts are meaningful only on model_inference_completed events; other
	// actions carry 0 (a legitimate optional per the row's action, not a fallback).
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// OriginAgent attributes an audit event to the agent's own work. It is the
// default so an unattributed event reads as the agent rather than as an empty
// string meaning "unknown".
const OriginAgent = "agent"

// DelegateOrigin returns the audit origin for a delegated sub-run at depth.
// Depth is what distinguishes a child's calls from its parent's in one trail.
func DelegateOrigin(depth int) string {
	return fmt.Sprintf("delegate:depth-%d", depth)
}

type MemoryEntry struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	TaskID    string    `json:"task_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type RuntimeEvent struct {
	Type             string `json:"type"`
	TaskID           string `json:"task_id"`
	Message          string `json:"message"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	// CachedTokens is the subset of PromptTokens served from the provider's
	// prompt cache. It is contract-optional: providers that do not report
	// prompt_tokens_details leave it at zero, which legitimately means "no
	// cache hit reported" rather than a fabricated default.
	CachedTokens int   `json:"cached_tokens,omitempty"`
	TotalTokens  int   `json:"total_tokens,omitempty"`
	ElapsedMs    int64 `json:"elapsed_ms,omitempty"`
	// GeneratedFiles carries workspace-relative paths of files the task produced
	// via write_file; populated on the task_completed event. Empty for events
	// that produced no files (a legitimate optional, not a fallback).
	GeneratedFiles []string `json:"generated_files,omitempty"`
	// SessionID / Seq / SessionEventType address ONE session event that was
	// just durably appended to the session event log. They are populated only
	// on RuntimeEventSessionEvent events (spec §7) and are zero on every other
	// type.
	//
	// The three of them are the whole payload of that notification on purpose:
	// the frame says "session S now has an event at seq N of kind K", and a
	// consumer that wants the event itself reads it back from
	// GET /v1/sessions/{id}/events?from_seq=. Carrying the event's own Data
	// here would let one large event flood a stream that also carries every
	// other lifecycle event.
	//
	// Seq is contract-REQUIRED on a session_event and 0 is a real value (it is
	// the first event of a log), so no consumer may read a zero Seq as
	// "absent". The `omitempty` below is about this struct's own JSON (the
	// /v1/runtime-events listing, where the session fields are noise on the
	// other 99% of events) — it is NOT the SSE frame, which is built field by
	// field in eventbridge.translate and always emits seq.
	//
	// The price is stated plainly rather than glossed: in THAT listing a
	// session_event sitting at seq 0 is serialised without a seq key, and a
	// reader of that one endpoint cannot tell it from an event that carries no
	// seq at all. Dropping the `omitempty` would trade that for the opposite
	// defect — every task lifecycle event growing a bare `"seq":0`, which
	// TestTranslateLeavesNonSessionEventsWithoutSessionFields (eventbridge)
	// forbids for the SSE
	// frame on the grounds that it reads as a real position. The listing is a
	// lifecycle view, not the seq-continuity channel: anything that actually
	// tracks seq must read the SSE frame or
	// GET /v1/sessions/{id}/events?from_seq=, both of which always carry it.
	SessionID        string    `json:"session_id,omitempty"`
	Seq              int64     `json:"seq,omitempty"`
	SessionEventType string    `json:"session_event_type,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// RuntimeEventSessionEvent is the Type of the notification published once per
// session event that reaches the session event log, and therefore the SSE frame
// type (`event: session_event`) a trajectory front end subscribes to with
// /v1/events?type=session_event.
const RuntimeEventSessionEvent = "session_event"

type ConversationRole string

const (
	ConversationRoleUser      ConversationRole = "user"
	ConversationRoleAssistant ConversationRole = "assistant"
)

type AgentSession struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	AgentID   string `json:"agent_id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Mode      string `json:"mode"`
	// WorkingDir is the host filesystem directory this session is bound to, if
	// any. When set, session state lives under <WorkingDir>/.stardust (see
	// sessionstate.SessionBase) instead of the workspace root. Empty means
	// unbound (uses the workspace root).
	WorkingDir string    `json:"working_dir,omitempty"`
	Archived   bool      `json:"archived"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type ConversationTurn struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	// AgentID names the sub-agent that produced this turn. It is
	// contract-optional and legitimately empty: the built-in default agent is
	// selected by submitting a task with an empty agent_id (see
	// internal/server/http.go's handleListAgents, which for that reason does
	// not list it), so every serve/GUI default-agent turn carries "" here.
	// Empty means "the built-in default agent", not "the value went missing" —
	// consumers must not treat it as a broken record, and the event projection
	// in internal/storage/project_turns.go must not reject it.
	AgentID      string           `json:"agent_id"`
	ModelProfile string           `json:"model_profile"`
	Role         ConversationRole `json:"role"`
	Content      string           `json:"content"`
	CreatedAt    time.Time        `json:"created_at"`
	// Token counts are meaningful only on assistant turns; user turns carry 0
	// (a legitimate optional per the turn's role, not a fallback).
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// GeneratedFiles are workspace-relative paths of files this turn's task
	// produced via write_file; persisted as JSON. Empty for user turns / tasks
	// that wrote no files (a legitimate optional per role/task, not a fallback).
	GeneratedFiles []string `json:"generated_files,omitempty"`
}

type AgentMessageType string

const (
	AgentMessageTypeMessage AgentMessageType = "message"
	AgentMessageTypeResult  AgentMessageType = "result"
	AgentMessageTypeHandoff AgentMessageType = "handoff"
	AgentMessageTypeReview  AgentMessageType = "review"
)

type AgentMessageStatus string

const (
	AgentMessageUnread AgentMessageStatus = "unread"
	AgentMessageRead   AgentMessageStatus = "read"
)

type AgentMessage struct {
	ID            string             `json:"id"`
	CompanyID     string             `json:"company_id"`
	TaskID        string             `json:"task_id"`
	SourceEventID string             `json:"source_event_id,omitempty"`
	ThreadID      string             `json:"thread_id"`
	FromAgentID   string             `json:"from_agent_id"`
	ToAgentID     string             `json:"to_agent_id"`
	Type          AgentMessageType   `json:"type"`
	Status        AgentMessageStatus `json:"status"`
	Summary       string             `json:"summary"`
	Artifact      string             `json:"artifact,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	// ReadAt carries no omitempty: encoding/json ignores it on struct types, so
	// an unread message still marshals as "0001-01-01T00:00:00Z" rather than
	// dropping the key. Consumers must treat the zero time as "unread" — use
	// ReadAt.IsZero(), not the key's absence. Making the key genuinely optional
	// requires changing the field to *time.Time, which is a wire-contract change.
	ReadAt time.Time `json:"read_at"`
}

type AgentMessageQuery struct {
	CompanyID     string
	TaskID        string
	ThreadID      string
	FromAgentID   string
	ToAgentID     string
	Status        AgentMessageStatus
	SourceEventID string
	Limit         int
}

type AgentMessageTaskEventFields struct {
	CompanyID      string
	EventID        string
	TaskID         string
	EventType      string
	FromAgentID    string
	ToAgentID      string
	ActorAgentID   string
	Summary        string
	Artifact       string
	CreatedAt      time.Time
	IdempotencyKey string
}

func AgentMessageFromTaskEventFields(fields AgentMessageTaskEventFields) AgentMessage {
	messageID := strings.TrimSpace(fields.IdempotencyKey)
	if messageID == "" {
		messageID = strings.TrimSpace(fields.EventID)
	}
	fromAgentID := strings.TrimSpace(fields.FromAgentID)
	if fromAgentID == "" {
		fromAgentID = strings.TrimSpace(fields.ActorAgentID)
	}
	return AgentMessage{
		ID:            messageID,
		CompanyID:     strings.TrimSpace(fields.CompanyID),
		TaskID:        strings.TrimSpace(fields.TaskID),
		SourceEventID: strings.TrimSpace(fields.EventID),
		ThreadID:      strings.TrimSpace(fields.TaskID),
		FromAgentID:   fromAgentID,
		ToAgentID:     strings.TrimSpace(fields.ToAgentID),
		Type:          agentMessageTypeFromTaskEvent(fields.EventType),
		Status:        AgentMessageUnread,
		Summary:       strings.TrimSpace(fields.Summary),
		Artifact:      strings.TrimSpace(fields.Artifact),
		CreatedAt:     fields.CreatedAt,
	}
}

func agentMessageTypeFromTaskEvent(eventType string) AgentMessageType {
	switch strings.TrimSpace(eventType) {
	case "result.appended":
		return AgentMessageTypeResult
	case "handoff.appended":
		return AgentMessageTypeHandoff
	case "review.appended":
		return AgentMessageTypeReview
	default:
		return AgentMessageTypeMessage
	}
}
