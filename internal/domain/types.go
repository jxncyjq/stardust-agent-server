package domain

import (
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
}

type ToolCall struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
	RiskLevel string            `json:"risk_level"`
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
	CachedTokens int       `json:"cached_tokens,omitempty"`
	TotalTokens  int       `json:"total_tokens,omitempty"`
	ElapsedMs    int64     `json:"elapsed_ms,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

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
	ID           string           `json:"id"`
	SessionID    string           `json:"session_id"`
	TaskID       string           `json:"task_id"`
	AgentID      string           `json:"agent_id"`
	ModelProfile string           `json:"model_profile"`
	Role         ConversationRole `json:"role"`
	Content      string           `json:"content"`
	CreatedAt    time.Time        `json:"created_at"`
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
	ReadAt        time.Time          `json:"read_at,omitempty"`
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
