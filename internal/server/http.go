package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/security"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/workflow"
)

type TaskStore interface {
	Add(ctx context.Context, task domain.Task) error
	Get(ctx context.Context, taskID string) (domain.Task, bool, error)
	List(ctx context.Context) ([]domain.Task, error)
}

type WaitingWorkflowStore interface {
	ListWaitingWorkflowStates(ctx context.Context) ([]storage.WorkflowState, error)
}

type WorkflowStateStore interface {
	WaitingWorkflowStore
	SaveWorkflowState(ctx context.Context, def workflow.Definition, result workflow.Result) error
	GetWorkflowState(ctx context.Context, workflowID string) (storage.WorkflowState, bool, error)
}

type ReadinessChecker interface {
	Ping(ctx context.Context) error
}

type QualityEvalStore interface {
	ListQualityEvalRuns(ctx context.Context, query quality.TrendQuery) ([]quality.EvalRunRecord, error)
}

type SessionStore interface {
	ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error)
	ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error)
	GetAgentSession(ctx context.Context, sessionID string) (domain.AgentSession, bool, error)
	SaveAgentSession(ctx context.Context, session domain.AgentSession) error
	DeleteAgentSession(ctx context.Context, sessionID string) error
	// ReadFrom 返回 seq >= fromSeq 的会话事件，按 seq 升序。读路径只走它、绝不走
	// Load：Load 会替未收尾的日志补事件并落盘，而它的调用契约是「只对确定没有活跃
	// 写入者的会话调用」（spec §4.3.1 第 3 条）——这个端点在任务执行期间也会被前端
	// 拉，两者不相容。
	ReadFrom(ctx context.Context, sessionID string, fromSeq int64, limit int64) ([]domain.SessionEvent, error)
}

type MessageStore interface {
	SaveAgentMessage(ctx context.Context, message domain.AgentMessage) error
	ListAgentMessages(ctx context.Context, query domain.AgentMessageQuery) ([]domain.AgentMessage, error)
	MarkAgentMessageRead(ctx context.Context, messageID string, readAt time.Time) error
}

// SkillManager installs, updates, and uninstalls skills on behalf of the GUI's
// /skill commands. It mirrors skill.Manager so the HTTP layer can drive the same
// disk-backed implementation the TUI uses. It is optional: when nil, the skill
// endpoints report 503 rather than panicking.
type SkillManager interface {
	Install(ctx context.Context, source string) (skill.Skill, error)
	Update(ctx context.Context, name string) (skill.Skill, error)
	Uninstall(ctx context.Context, name string) error
}

// TaskInterrupter cancels a running task's tool-loop mid-flight. It is
// satisfied directly by *agentruntime.Coordinator (Interrupt(taskID string)
// error); the interface exists so the HTTP layer stays testable without
// pulling in the full coordinator. Interrupt returns an error when the task
// is not currently running (already finished / never started / unknown) so
// the handler can fail loud with 404 instead of pretending an interrupt
// happened.
type TaskInterrupter interface {
	Interrupt(taskID string) error
}

// ApprovalDecider records a human approve/deny decision on a Manual-mode tool
// approval ticket and returns the updated ticket. It is satisfied by
// manualgate.ApprovalCoordinator; the server package depends only on this
// narrow interface to stay decoupled from the manualgate implementation.
type ApprovalDecider interface {
	Decide(ctx context.Context, taskID, ticketID string, status approval.ApprovalStatus) (approval.ToolApproval, error)
}

type Config struct {
	Tasks          TaskStore
	Agents         AgentCatalog
	Workflows      WaitingWorkflowStore
	WorkflowStates WorkflowStateStore
	WorkflowEngine *workflow.Engine
	WorkflowEvents port.EventBus
	PlatformEvents *observability.EventBus
	// Browser is the per-session browser stream source backing the SSE endpoint
	// /v1/browser/sessions/{id}/stream. It is optional: when nil the endpoint
	// reports 503. Satisfied by *browser.Runtime (Subscribe + ReplaySince).
	Browser BrowserStreamer
	// Tokens 是当前有效凭证的持有者，也是长连接得知凭证被吊销的唯一途径。
	//
	// nil 表示这个部署不轮换凭证：鉴权退回 AdminToken 的静态比较，行为与这个字段
	// 出现之前完全一致。装配期（cli）在铸了 loopback token 或配了 AdminToken 时才
	// 建一个。
	Tokens       *TokenStore
	Audit        port.AuditLog
	QualityEvals QualityEvalStore
	Sessions     SessionStore
	Messages     MessageStore
	Skills       SkillManager
	// Plugins is the plugin-authorization surface backing GET /v1/plugins
	// (and, from Task 3 onward, the grant/deny endpoints on the same
	// interface). It is optional the same way Skills is: nil means this
	// process assembled no plugin loader ("plugins.manifest" not configured),
	// and the endpoint reports 404 naming that rather than an empty list --
	// see handleListPlugins.
	Plugins             PluginConsent
	ToolApprovals       ApprovalDecider
	ApprovalTickets     ApprovalLister
	TaskInterrupter     TaskInterrupter
	Readiness           ReadinessChecker
	AdminToken          string
	PublicHealthEnabled bool
	// RequireIdentity makes the X-Role and X-Company-ID request headers
	// mandatory for RBAC and tenant checks. It defaults to false, which keeps
	// the single-machine contract where a header-less request is treated as an
	// admin of every company (see security.Policy.RequireIdentity).
	//
	// Scope, so it is not mistaken for a blanket authentication switch: it
	// governs only the handlers that consult the policy — the single-resource
	// endpoints guarded by requireCompanyAccess and the two RBAC-guarded
	// governance endpoints. List endpoints (GET /v1/tasks, /v1/agents,
	// /v1/sessions, ...) have no tenant filtering today and stay reachable.
	// Both headers are caller-asserted, so they only form a boundary where a
	// trusted gateway injects them and strips the client's own values.
	RequireIdentity bool
	RequestIDHeader string
	// WorkspaceRoot is the base directory for a session's on-disk state when
	// the session carries no working_dir (sessionstate.SessionBase's
	// workspaceRoot argument). Session deletion joins it with the session key
	// to locate the directory to remove alongside the DB row (spec §4.0).
	WorkspaceRoot string
	// FileBaseURL is the public base URL (no trailing slash) that fileURL
	// prepends to generated-file links when the agent server is not on the
	// same origin as its caller (e.g. a deployed GUI). Empty means the
	// caller resolves the relative "/v1/files?..." path against this
	// server's own origin, which is the loopback/single-machine default.
	// Mirrors config.ServerConfig.FileBaseURL; bridged at the assembly site
	// in internal/cli/command.go since server.Config intentionally does not
	// import internal/config.
	FileBaseURL string
	Logger      *slog.Logger
	Metrics     *observability.MetricsRecorder
	Diagnostics *observability.Diagnostics
	Traces      *observability.TraceRecorder
}

type HTTPServer struct {
	tasks               TaskStore
	agents              AgentCatalog
	workflows           WaitingWorkflowStore
	workflowStates      WorkflowStateStore
	workflowEngine      *workflow.Engine
	workflowEvents      port.EventBus
	platformEvents      *observability.EventBus
	browser             BrowserStreamer
	audit               port.AuditLog
	qualityEvals        QualityEvalStore
	sessions            SessionStore
	messages            MessageStore
	skills              SkillManager
	plugins             PluginConsent
	toolApprovals       ApprovalDecider
	approvalTickets     ApprovalLister
	taskInterrupter     TaskInterrupter
	readiness           ReadinessChecker
	adminToken          string
	tokens              *TokenStore
	publicHealthEnabled bool
	policy              security.Policy
	requestIDHeader     string
	workspaceRoot       string
	fileBaseURL         string
	logger              *slog.Logger
	metrics             *observability.MetricsRecorder
	diagnostics         *observability.Diagnostics
	traces              *observability.TraceRecorder
}

type requestIDContextKey struct{}

func NewHTTPServer(cfg Config) *HTTPServer {
	requestIDHeader := cfg.RequestIDHeader
	if requestIDHeader == "" {
		requestIDHeader = "X-Request-ID"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	workflowStates := cfg.WorkflowStates
	if workflowStates == nil {
		if states, ok := cfg.Workflows.(WorkflowStateStore); ok {
			workflowStates = states
		}
	}
	if !cfg.RequireIdentity {
		// The permissive mode is a declared optional contract, not a silent
		// default: say so out loud at assembly time so an operator who exposes
		// this listener beyond localhost cannot miss it.
		observability.WithComponent(logger, "server").Info("identity verification disabled",
			"detail", "requests without X-Role and X-Company-ID headers receive full access to every company",
			"remedy", "set server.require_identity=true (LEGION_AGENT_REQUIRE_IDENTITY=true) to require caller identity",
		)
	}
	return &HTTPServer{
		tasks:               cfg.Tasks,
		agents:              cfg.Agents,
		workflows:           cfg.Workflows,
		workflowStates:      workflowStates,
		workflowEngine:      cfg.WorkflowEngine,
		workflowEvents:      cfg.WorkflowEvents,
		platformEvents:      cfg.PlatformEvents,
		browser:             cfg.Browser,
		audit:               cfg.Audit,
		qualityEvals:        cfg.QualityEvals,
		sessions:            cfg.Sessions,
		messages:            cfg.Messages,
		skills:              cfg.Skills,
		plugins:             cfg.Plugins,
		toolApprovals:       cfg.ToolApprovals,
		approvalTickets:     cfg.ApprovalTickets,
		taskInterrupter:     cfg.TaskInterrupter,
		readiness:           cfg.Readiness,
		adminToken:          cfg.AdminToken,
		tokens:              cfg.Tokens,
		publicHealthEnabled: cfg.PublicHealthEnabled,
		policy:              security.NewPolicy(cfg.RequireIdentity),
		requestIDHeader:     requestIDHeader,
		workspaceRoot:       cfg.WorkspaceRoot,
		fileBaseURL:         cfg.FileBaseURL,
		logger:              observability.WithComponent(logger, "server"),
		metrics:             cfg.Metrics,
		diagnostics:         cfg.Diagnostics,
		traces:              cfg.Traces,
	}
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := r.Header.Get(s.requestIDHeader)
	if requestID == "" {
		requestID = newRequestID()
	}
	w.Header().Set(s.requestIDHeader, requestID)
	r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if s.metrics != nil && (r.URL.Path != "/metrics" || rec.status != http.StatusOK) {
			s.metrics.IncHTTPStatus(rec.status)
		}
		observability.WithRequestID(s.logger, requestID).Info("http request handled",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	}()
	if !s.authorized(r) {
		writeError(rec, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		writeJSON(rec, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		s.handleReadyz(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		s.handleMetrics(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/debug/diagnostics":
		s.handleDiagnostics(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/debug/traces":
		s.handleTraces(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/openapi.json":
		writeJSON(rec, http.StatusOK, BuildOpenAPISpec())
	case r.Method == http.MethodGet && r.URL.Path == "/v1/events":
		s.handleEvents(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/stream"):
		s.handleBrowserStream(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/takeover"):
		s.handleBrowserTakeover(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/viewport"):
		s.handleBrowserViewport(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/browser/sessions":
		s.handleBrowserSessions(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/info"):
		s.handleBrowserSessionInfo(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/navigate"):
		s.handleBrowserNavigate(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/browser/sessions/") && strings.HasSuffix(r.URL.Path, "/input"):
		s.handleBrowserInput(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/approvals":
		s.handleListApprovals(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/audit-events":
		s.handleAuditEvents(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime-events":
		s.handleRuntimeEvents(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/quality/evals":
		s.handleQualityEvals(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/plugins":
		s.handleListPlugins(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/grant"):
		s.handleGrantPlugin(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/deny"):
		s.handleDenyPlugin(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/resolve"):
		s.handleResolvePlugin(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
		s.handleServeFile(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/rotate":
		s.handleRotateToken(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		s.handleCreateSession(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		s.handleListSessions(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/turns"):
		s.handleListSessionTurns(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/events"):
		s.handleSessionEvents(rec, r)
	// 会话本体的写：判据是「路径解出了一个会话 id」，而不是「路径不以某个已知子资源
	// 后缀结尾」。后者每加一个子资源就要补一条排除，漏补一次就意味着
	// DELETE /v1/sessions/{id}/events 落进会话删除分支——数据损坏级的错误。
	// sessionIDFromPath 只在 /v1/sessions/{id} 这一种形状上返回非空，于是子资源
	// 路径天然被排除，将来再加也不必回头改这里。
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && sessionIDFromPath(r.URL.Path) != "":
		s.handlePatchSession(rec, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && sessionIDFromPath(r.URL.Path) != "":
		s.handleDeleteSession(rec, r)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && strings.HasPrefix(r.URL.Path, "/v1/agents/") && strings.HasSuffix(r.URL.Path, "/messages"):
		s.handleAgentMessages(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
		s.handleListAgents(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
		s.handleCreateTask(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks":
		s.handleListTasks(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/result"):
		s.handleGetTaskResult(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.Contains(r.URL.Path, "/approvals/"):
		s.handleDecideApproval(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/interrupt"):
		s.handleInterruptTask(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		s.handleGetTask(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/workflows":
		s.handleSubmitWorkflow(rec, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/workflows/") && strings.HasSuffix(r.URL.Path, "/events"):
		s.handleResumeWorkflow(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/workflows/") && r.URL.Path != "/v1/workflows/waiting":
		s.handleGetWorkflow(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/workflows/waiting":
		s.handleWaitingWorkflows(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/install":
		s.handleInstallSkill(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/update":
		s.handleUpdateSkill(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/skills/uninstall":
		s.handleUninstallSkill(rec, r)
	default:
		writeError(rec, http.StatusNotFound, "not found")
	}
}

func (s *HTTPServer) authorized(r *http.Request) bool {
	if s.adminToken == "" {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" && s.publicHealthEnabled {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" && s.publicHealthEnabled {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/openapi.json" {
		return true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	// 有 TokenStore 就以它为准：静态的 adminToken 是这个部署**启动时**的凭证，
	// 轮换之后它已经不是当前那个了，继续拿它比较等于让被吊销的 token 一直有效。
	if s.tokens != nil {
		return s.tokens.Valid(presented)
	}
	return r.Header.Get("Authorization") == "Bearer "+s.adminToken
}

// handleRotateToken 让运维在不重启进程的前提下作废当前凭证。
//
// 重启也能换 token，但会把正在跑的任务一起打断——于是「我的 token 泄露了」这件事
// 的代价变成了「所有人的工作中断」，结果就是没人愿意轮换。
func (s *HTTPServer) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil || !s.tokens.RotateAllowed() {
		// 409 而不是 404/501：端点在，只是这个部署没有可轮换的凭证（本机开放
		// serve）。把它说成「没有这个端点」会让运维以为版本不对。
		writeError(w, http.StatusConflict, "this deployment has no bearer token to rotate")
		return
	}
	next := s.tokens.Rotate()
	writeJSON(w, http.StatusOK, map[string]any{"token": next})
}

// handleCreateSession creates a new agent session for two-level grouping. The
// session id is generated server-side; company/agent default to the standard
// single-tenant ids when omitted. A missing session store is reported as a 503
// rather than silently swallowed, per the fail-loud rule.
func (s *HTTPServer) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	var req createSessionRequest
	decoder := json.NewDecoder(r.Body)
	// Same contract as handlePatchSession: a field this handler cannot apply is
	// reported rather than dropped. id is the case that motivated it -- the
	// server always generates the session id, so answering 201 carrying a
	// different id than the caller sent hides the misunderstanding.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session request: %v", err))
		return
	}
	companyID := strings.TrimSpace(req.CompanyID)
	if companyID == "" {
		companyID = "default-company"
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		agentID = "default-agent"
	}
	mode, ok := domain.NormalizeMode(req.Mode)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid mode %q (want manual|plan|auto)", req.Mode))
		return
	}
	now := time.Now()
	session := domain.AgentSession{
		ID:         fmt.Sprintf("session-%d", now.UTC().UnixNano()),
		CompanyID:  companyID,
		AgentID:    agentID,
		Project:    strings.TrimSpace(req.Project),
		Title:      strings.TrimSpace(req.Title),
		Mode:       mode,
		WorkingDir: strings.TrimSpace(req.WorkingDir),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.sessions.SaveAgentSession(r.Context(), session); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("create session: %v", err))
		return
	}
	observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Info("session created",
		"session_id", session.ID, "project", session.Project, "agent_id", session.AgentID)
	writeJSON(w, http.StatusCreated, session)
}

func (s *HTTPServer) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeJSON(w, http.StatusOK, []domain.AgentSession{})
		return
	}
	sessions, err := s.sessions.ListAgentSessions(r.Context(), r.URL.Query().Get("company_id"), r.URL.Query().Get("agent_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *HTTPServer) handleListSessionTurns(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeJSON(w, http.StatusOK, []conversationTurnResponse{})
		return
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/turns")
	if strings.TrimSpace(sessionID) == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = parsed
	}
	turns, err := s.sessions.ListConversationTurns(r.Context(), sessionID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list session turns: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, s.conversationTurnResponses(turns))
}

// conversationTurnResponse mirrors domain.ConversationTurn field-for-field,
// except GeneratedFiles is the linked DTO form instead of bare relative
// paths, so the GUI's history reload can render download/open links without
// a second round trip. All other fields keep their existing json tags so
// consumers of the plain turn shape are unaffected.
type conversationTurnResponse struct {
	ID               string                  `json:"id"`
	SessionID        string                  `json:"session_id"`
	TaskID           string                  `json:"task_id"`
	AgentID          string                  `json:"agent_id"`
	ModelProfile     string                  `json:"model_profile"`
	Role             domain.ConversationRole `json:"role"`
	Content          string                  `json:"content"`
	CreatedAt        time.Time               `json:"created_at"`
	PromptTokens     int                     `json:"prompt_tokens,omitempty"`
	CompletionTokens int                     `json:"completion_tokens,omitempty"`
	CachedTokens     int                     `json:"cached_tokens,omitempty"`
	TotalTokens      int                     `json:"total_tokens,omitempty"`
	GeneratedFiles   []GeneratedFile         `json:"generated_files"`
}

// conversationTurnResponses maps persisted turns to their response shape,
// resolving each turn's generated files into links via fileURL.
func (s *HTTPServer) conversationTurnResponses(turns []domain.ConversationTurn) []conversationTurnResponse {
	out := make([]conversationTurnResponse, 0, len(turns))
	for _, turn := range turns {
		out = append(out, conversationTurnResponse{
			ID:               turn.ID,
			SessionID:        turn.SessionID,
			TaskID:           turn.TaskID,
			AgentID:          turn.AgentID,
			ModelProfile:     turn.ModelProfile,
			Role:             turn.Role,
			Content:          turn.Content,
			CreatedAt:        turn.CreatedAt,
			PromptTokens:     turn.PromptTokens,
			CompletionTokens: turn.CompletionTokens,
			CachedTokens:     turn.CachedTokens,
			TotalTokens:      turn.TotalTokens,
			GeneratedFiles:   s.generatedFilesDTO(turn.SessionID, turn.GeneratedFiles),
		})
	}
	return out
}

// sessionIDFromPath extracts the {id} segment from /v1/sessions/{id}. It returns
// "" when the trimmed remainder is empty or still contains a slash (a nested
// path that this handler does not own), so the caller can reject it as a bad
// request instead of acting on a malformed id.
//
// It is also the ROUTING predicate for PATCH and DELETE /v1/sessions/{id} (see
// the routing switch above): those two branches are selected by "this path
// resolves to a session id", precisely so that every sub-resource path is
// excluded without having to enumerate the suffixes. Loosening it — accepting a
// remainder that contains a slash — therefore does not merely admit a malformed
// id: it routes DELETE /v1/sessions/{id}/events into handleDeleteSession, i.e.
// deletes the session. Any change here must keep the "no slash" rule.
func sessionIDFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/v1/sessions/")
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return strings.TrimSpace(trimmed)
}

// handlePatchSession updates the mutable fields of a single session. The request
// body carries pointer fields so an omitted field is left untouched while an
// explicitly provided one (including an empty string or false) is applied; this
// lets a rename send only title and an archive send only archived without
// clobbering the rest. A missing session is a 404, surfaced rather than silently
// created, per the fail-loud rule. The updated session is returned.
func (s *HTTPServer) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	sessionID := sessionIDFromPath(r.URL.Path)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	var req patchSessionRequest
	decoder := json.NewDecoder(r.Body)
	// A field this handler cannot apply must be reported, not dropped: silently
	// accepting e.g. agent_id and answering 200 tells the caller a change landed
	// when nothing did. The decoder's own message names the offending field, so
	// it is passed through rather than replaced with a generic one.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid session patch request: %v", err))
		return
	}
	session, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
		return
	}
	if req.Title != nil {
		session.Title = strings.TrimSpace(*req.Title)
	}
	if req.Project != nil {
		session.Project = strings.TrimSpace(*req.Project)
	}
	if req.Archived != nil {
		session.Archived = *req.Archived
	}
	if req.Mode != nil {
		mode, ok := domain.NormalizeMode(*req.Mode)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid mode %q (want manual|plan|auto)", *req.Mode))
			return
		}
		session.Mode = mode
	}
	if req.WorkingDir != nil {
		newWorkingDir := strings.TrimSpace(*req.WorkingDir)
		currentWorkingDir := strings.TrimSpace(session.WorkingDir)
		// A session's on-disk state (checkpoints, approval tickets, plans) is
		// filed under sessionstate.SessionBase(workspaceRoot, working_dir), and
		// that base is derived from whatever working_dir the session carries at
		// the moment of the write -- there is no record of a session's *former*
		// bases. Recovery after a restart only enumerates the bases in current
		// use (distinctSessionBases in the cli package), so once a session has a
		// non-empty working_dir, silently repointing it to a different value
		// would strand any state already filed under the old base: it would
		// never again be scanned, and a pending checkpoint would be lost without
		// so much as a log line. Fail loud instead: reject the change outright.
		// Setting it for the first time (currentWorkingDir == "") is safe --
		// with no working_dir yet, state lives under workspaceRoot, which is
		// always in the base set -- and re-PATCHing the same value is a no-op.
		if currentWorkingDir != "" && newWorkingDir != currentWorkingDir {
			writeError(w, http.StatusBadRequest, "working_dir cannot be changed once set")
			return
		}
		session.WorkingDir = newWorkingDir
	}
	session.UpdatedAt = time.Now()
	if err := s.sessions.SaveAgentSession(r.Context(), session); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("update session: %v", err))
		return
	}
	observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Info("session updated",
		"session_id", session.ID, "project", session.Project, "archived", session.Archived)
	writeJSON(w, http.StatusOK, session)
}

// handleDeleteSession removes a session, its conversation turns, and the
// on-disk session directory (spec §4.0: DELETE cascades to the state a session
// left under sessionstate.SessionBase). A session id that does not exist maps
// to a 404 rather than being reported as a no-op success, so the client learns
// the delete had no target. Success returns 204 No Content.
func (s *HTTPServer) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	sessionID := sessionIDFromPath(r.URL.Path)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	// The session's working_dir determines where its directory lives
	// (sessionstate.SessionBase), and it is only readable before the DB row is
	// gone, so it must be fetched ahead of the delete.
	session, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
		return
	}
	if err := s.sessions.DeleteAgentSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, storage.ErrAgentSessionNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete session: %v", err))
		return
	}
	base := sessionstate.SessionBase(s.workspaceRoot, session.WorkingDir)
	if base == "" {
		// Only reachable when both s.workspaceRoot and the session's working_dir
		// are empty (an unconfigured production deployment always resolves
		// workspaceRoot to a non-empty absolute path via
		// sessionstate.ResolveWorkspaceRoot, so this is test/misconfiguration
		// territory, not a production path). SessionDir(base, id) would then
		// join onto "" and yield a bare "session/<id>" relative to the process
		// cwd -- os.RemoveAll on that is not the directory this delete promised
		// to remove, so skip it rather than risk deleting the wrong thing. This
		// is a defensive guard, not a silent skip: it is logged at Warn.
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Warn("delete session: skipping on-disk cleanup, empty session base",
			"session_id", sessionID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sessionDir := sessionstate.SessionDir(base, sessionID)
	if err := os.RemoveAll(sessionDir); err != nil {
		// Fail-loud: the DB row is already gone, but a directory the delete
		// promised to remove is still on disk. Do not report success — log
		// and return 500 rather than silently leaving orphaned state.
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Error("delete session directory failed",
			"session_id", sessionID, "dir", sessionDir, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete session directory %q: %v", sessionDir, err))
		return
	}
	observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Info("session deleted",
		"session_id", sessionID, "dir", sessionDir)
	w.WriteHeader(http.StatusNoContent)
}

// handleServeFile streams a generated file for read-only preview/download,
// confined to the requesting session's working directory. Auth is enforced
// centrally by HTTPServer.authorized (called for every route before the
// switch dispatches here), so this handler does not re-check the Authorization
// header.
func (s *HTTPServer) handleServeFile(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
		return
	}
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	rel := q.Get("path")
	if sessionID == "" || strings.TrimSpace(rel) == "" {
		writeError(w, http.StatusBadRequest, "session_id and path are required")
		return
	}
	session, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	root := strings.TrimSpace(session.WorkingDir)
	if root == "" {
		writeError(w, http.StatusNotFound, "session has no working directory")
		return
	}
	abs, err := resolveInWorkspace(root, rel)
	if err != nil {
		writeError(w, http.StatusForbidden, "path outside workspace")
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	ctype := mime.TypeByExtension(filepath.Ext(abs))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(abs)+"\"")
	}
	http.ServeContent(w, r, filepath.Base(abs), info.ModTime(), f)
}

// resolveInWorkspace joins rel onto root and refuses any path escaping root,
// so a caller cannot pass "../../etc/passwd" (or similar) to read outside the
// session's working directory.
func resolveInWorkspace(root, rel string) (string, error) {
	abs := filepath.Clean(filepath.Join(root, rel))
	rp, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("resolve %q against workspace root %q: %w", rel, root, err)
	}
	if rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q outside workspace root", rel)
	}
	return abs, nil
}

// fileURL builds a link for a generated file. Absolute when FileBaseURL is
// configured (deployment), else a relative "/v1/files?..." path the loopback
// frontend resolves against its own base URL. Never persisted -- callers
// build it fresh from the session id and relative path each time.
func (s *HTTPServer) fileURL(sessionID, relPath string, download bool) string {
	v := url.Values{}
	v.Set("session_id", sessionID)
	v.Set("path", relPath)
	if download {
		v.Set("download", "1")
	}
	rel := "/v1/files?" + v.Encode()
	if s.fileBaseURL != "" {
		return s.fileBaseURL + rel
	}
	return rel
}

func (s *HTTPServer) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	if s.messages == nil {
		writeError(w, http.StatusServiceUnavailable, "message store is unavailable")
		return
	}
	agentID := agentIDFromMessagesPath(r.URL.Path)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "agent id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleListAgentMessages(w, r, agentID)
	case http.MethodPost:
		s.handleSendAgentMessage(w, r, agentID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *HTTPServer) handleListAgentMessages(w http.ResponseWriter, r *http.Request, agentID string) {
	query := r.URL.Query()
	companyID := strings.TrimSpace(query.Get("company_id"))
	if !s.requireCompanyAccess(w, r, companyID, "agent_messages", agentID) {
		return
	}
	limit, err := parseNonNegativeInt(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
		return
	}
	messages, err := s.messages.ListAgentMessages(r.Context(), domain.AgentMessageQuery{
		CompanyID:     companyID,
		TaskID:        strings.TrimSpace(query.Get("task_id")),
		ThreadID:      strings.TrimSpace(query.Get("thread_id")),
		FromAgentID:   firstNonEmptyString(query.Get("from"), query.Get("from_agent_id")),
		ToAgentID:     agentID,
		Status:        domain.AgentMessageStatus(strings.TrimSpace(query.Get("status"))),
		SourceEventID: strings.TrimSpace(query.Get("source_event_id")),
		Limit:         limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list messages: %v", err))
		return
	}
	if parseBool(query.Get("mark_read")) {
		now := time.Now().UTC()
		for _, message := range messages {
			if message.Status == domain.AgentMessageUnread {
				if err := s.messages.MarkAgentMessageRead(r.Context(), message.ID, now); err != nil {
					writeError(w, http.StatusInternalServerError, fmt.Sprintf("mark message read: %v", err))
					return
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *HTTPServer) handleSendAgentMessage(w http.ResponseWriter, r *http.Request, agentID string) {
	var req sendAgentMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid message request")
		return
	}
	if strings.TrimSpace(req.Summary) == "" {
		writeError(w, http.StatusBadRequest, "summary is required")
		return
	}
	if !s.requireCompanyAccess(w, r, req.CompanyID, "agent_messages", agentID) {
		return
	}
	message := domain.AgentMessage{
		ID:            firstNonEmptyString(req.MessageID, newAgentMessageID()),
		CompanyID:     strings.TrimSpace(req.CompanyID),
		TaskID:        strings.TrimSpace(req.TaskID),
		SourceEventID: strings.TrimSpace(req.SourceEventID),
		ThreadID:      firstNonEmptyString(req.ThreadID, req.TaskID),
		FromAgentID:   firstNonEmptyString(req.From, req.FromAgentID, "agent"),
		ToAgentID:     agentID,
		Type:          parseAgentMessageType(req.Type),
		Status:        domain.AgentMessageUnread,
		Summary:       strings.TrimSpace(req.Summary),
		Artifact:      strings.TrimSpace(req.Artifact),
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.messages.SaveAgentMessage(r.Context(), message); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("send message: %v", err))
		return
	}
	writeJSON(w, http.StatusCreated, message)
}

// handleInstallSkill installs a skill from the given source URL/shorthand and
// returns the installed skill summary. A missing manager is a 503; a missing
// source is a 400; an install failure (bad source, parse/scan failure) is
// surfaced loudly as a 400 with the underlying reason rather than swallowed.
func (s *HTTPServer) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		writeError(w, http.StatusServiceUnavailable, "skill manager is unavailable")
		return
	}
	var req skillCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid skill request")
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	installed, err := s.skills.Install(r.Context(), source)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("install skill: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, skillResponseFromSkill(installed))
}

// handleUpdateSkill re-fetches a previously installed skill by name using its
// stored source. A missing name is a 400; an unknown skill or fetch failure is
// reported as a 400 with the reason.
func (s *HTTPServer) handleUpdateSkill(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		writeError(w, http.StatusServiceUnavailable, "skill manager is unavailable")
		return
	}
	var req skillCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid skill request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	updated, err := s.skills.Update(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("update skill: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, skillResponseFromSkill(updated))
}

// handleUninstallSkill removes an installed skill by name. A missing name is a
// 400; an unknown skill is reported as a 400 with the reason.
func (s *HTTPServer) handleUninstallSkill(w http.ResponseWriter, r *http.Request) {
	if s.skills == nil {
		writeError(w, http.StatusServiceUnavailable, "skill manager is unavailable")
		return
	}
	var req skillCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid skill request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.skills.Uninstall(r.Context(), name); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("uninstall skill: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "uninstalled", "name": name})
}

// skillResponseFromSkill projects the internal skill.Skill into the small JSON
// shape the GUI renders, avoiding leaking the full skill content.
func skillResponseFromSkill(sk skill.Skill) map[string]any {
	return map[string]any{
		"id":         sk.ID,
		"name":       sk.Name,
		"version":    sk.Version,
		"risk_level": string(sk.RiskLevel),
		"summary":    sk.Summary,
	}
}

// AgentCatalog lists the configured sub-agents a task may target. It is
// satisfied by *agentregistry.Registry; the server takes the interface so it
// does not depend on the registry package directly.
type AgentCatalog interface {
	Names() []string
}

// handleListAgents returns the names of the configured sub-agents so a client
// (the GUI agent picker) can offer them as conversation targets. The names are
// exactly the keys of the config's `agents` map; the built-in default agent is
// reached by submitting a task with an empty agent_id, so it is not listed here.
func (s *HTTPServer) handleListAgents(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusServiceUnavailable, "agent registry is unavailable")
		return
	}
	names := s.agents.Names()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": names})
}

func (s *HTTPServer) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task store is unavailable")
		return
	}
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid task request")
		return
	}
	if req.ID == "" || req.Input == "" {
		writeError(w, http.StatusBadRequest, "task id and input are required")
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	now := time.Now()
	// A task's mode is inherited from its owning session (or "auto" for a
	// one-off task with no session_id). The session is loaded once here — both
	// to resolve the mode and, further down, to record the user turn — rather
	// than queried twice.
	taskMode := domain.ModeAuto
	var session domain.AgentSession
	haveSession := false
	if sessionID != "" {
		if s.sessions == nil {
			writeError(w, http.StatusServiceUnavailable, "session store is unavailable")
			return
		}
		loaded, ok, err := s.sessions.GetAgentSession(r.Context(), sessionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", sessionID))
			return
		}
		resolved, ok := domain.NormalizeMode(loaded.Mode)
		if !ok {
			// An invalid mode stored on disk is corrupt state, not client input —
			// fail loud with a 500 rather than silently coercing to auto.
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("session %q has invalid stored mode %q", sessionID, loaded.Mode))
			return
		}
		taskMode = resolved
		session = loaded
		haveSession = true
	}
	// A session's working_dir is inherited onto every task it spawns (mirrors
	// the mode inheritance above). An empty working_dir is a legal "use the
	// workspace root" state, but a non-empty one that does not resolve to an
	// existing directory is corrupt session state — fail loud with a 400
	// rather than enqueuing a task whose tool calls would silently resolve to
	// the wrong base directory.
	taskWorkingDir := ""
	if haveSession {
		taskWorkingDir = session.WorkingDir
		if wd := strings.TrimSpace(taskWorkingDir); wd != "" {
			info, err := os.Stat(wd)
			if err != nil || !info.IsDir() {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("session %q working_dir %q is not an existing directory", sessionID, wd))
				return
			}
		}
	}
	task := domain.Task{
		ID:         req.ID,
		CompanyID:  req.CompanyID,
		AgentID:    req.AgentID,
		SessionID:  sessionID,
		Mode:       taskMode,
		WorkingDir: taskWorkingDir,
		Status:     domain.TaskPending,
		Input:      req.Input,
		CreatedAt:  now,
		Images:     req.Images,
	}
	// Float the session to the top of the session list before the task is
	// enqueued. The user prompt itself is no longer written here: the session
	// event log is the single source of truth for conversation content (spec §3
	// 取舍 A2) and the runtime records the user/message event when it runs the
	// task. session_id is an optional field (a one-off task may carry none), but
	// when it is present the session must exist — a missing session is a client
	// error, not a state we silently paper over.
	if haveSession {
		if err := s.touchSessionOnSubmit(r.Context(), w, task, session); err != nil {
			return
		}
	}
	if err := s.tasks.Add(r.Context(), task); err != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("create task: %v", err))
		return
	}
	if s.metrics != nil {
		s.metrics.IncTaskStatus("submitted")
	}
	observability.WithTaskID(observability.WithRequestID(s.logger, requestIDFromContext(r.Context())), task.ID).Info("task submitted")
	writeJSON(w, http.StatusCreated, task)
}

// touchSessionOnSubmit refreshes the owning session's updated_at when a task is
// submitted into it, so the session sorts to the top of the session list. The
// session must already be loaded by the caller (handleCreateTask resolves it
// once, to derive the task's mode, and passes it here rather than querying it a
// second time); the existing project/title/created_at are preserved by
// re-saving that loaded session. It writes a 5xx response and returns a non-nil
// error when the write fails, so the caller aborts loudly instead of enqueuing a
// task whose session bookkeeping was lost.
//
// It no longer writes the prompt anywhere: conversation content lives only in
// the session event log now (spec §3 取舍 A2), written by the runtime's
// recordUserMessage when the task actually runs. Touching on every submit —
// including a resubmission of the same task id — is unchanged behaviour.
func (s *HTTPServer) touchSessionOnSubmit(ctx context.Context, w http.ResponseWriter, task domain.Task, session domain.AgentSession) error {
	session.UpdatedAt = task.CreatedAt
	if err := s.sessions.SaveAgentSession(ctx, session); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("touch session: %v", err))
		return err
	}
	return nil
}

// handleListTasks returns every task currently tracked by the task store. The
// list reflects the in-session live scheduler (plus any persistent store) and is
// returned newest-last in creation order; the frontend renders the most recent
// entries. A nil store is a 503 rather than a silent empty list, per fail-loud.
func (s *HTTPServer) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task store is unavailable")
		return
	}
	tasks, err := s.tasks.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list tasks: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *HTTPServer) handleGetTask(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task store is unavailable")
		return
	}
	taskID := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, ok, err := s.tasks.Get(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get task: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if !s.requireCompanyAccess(w, r, task.CompanyID, "task", task.ID) {
		return
	}
	writeJSON(w, http.StatusOK, task)
}

type taskResultResponse struct {
	TaskID           string          `json:"task_id"`
	Status           string          `json:"status"`
	Result           string          `json:"result"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	CachedTokens     int             `json:"cached_tokens"`
	TotalTokens      int             `json:"total_tokens"`
	ElapsedMs        int64           `json:"elapsed_ms"`
	GeneratedFiles   []GeneratedFile `json:"generated_files"`
}

// GeneratedFile is the linked view of a workspace-relative path a task wrote
// via write_file. URL/DownloadURL are built fresh from fileURL on every
// response rather than persisted, so a later FileBaseURL change is reflected
// immediately without a data migration.
type GeneratedFile struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
	Name        string `json:"name"`
}

// taskUsage is the token/timing breakdown scanned from the task_completed
// runtime event. CachedTokens is the subset of PromptTokens served from the
// provider prompt cache; it stays zero when the provider does not report it.
type taskUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	TotalTokens      int
	ElapsedMs        int64
}

// handleGetTaskResult returns the current task status together with the answer
// text produced by the runtime. The answer is carried by the task_completed
// runtime event (its Message field holds the model response), which is the only
// place the result is exposed because TaskRun is not persisted.
func (s *HTTPServer) handleGetTaskResult(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "task store is unavailable")
		return
	}
	taskID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/result")
	if strings.TrimSpace(taskID) == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, ok, err := s.tasks.Get(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get task: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if !s.requireCompanyAccess(w, r, task.CompanyID, "task", task.ID) {
		return
	}
	result, usage, generatedFiles, err := s.taskResult(taskID)
	if err != nil {
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Error("read task result failed", "task_id", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("task result: %v", err))
		return
	}
	// Nothing is persisted here any more. The assistant answer, its token usage
	// and its generated files all reach the conversation through the session
	// event log, which the runtime writes as the task runs and
	// ListConversationTurns projects back out (spec §3 取舍 A2); a second write
	// from this read-only endpoint would be the second source of truth that
	// retiring conversation_turns exists to remove.
	writeJSON(w, http.StatusOK, taskResultResponse{
		TaskID:           taskID,
		Status:           string(task.Status),
		Result:           result,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CachedTokens:     usage.CachedTokens,
		TotalTokens:      usage.TotalTokens,
		ElapsedMs:        usage.ElapsedMs,
		GeneratedFiles:   s.generatedFilesDTO(task.SessionID, generatedFiles),
	})
}

// taskResult scans the runtime event bus for the task_completed event of the
// given task and returns its answer text, total token usage, elapsed time in
// milliseconds, and the workspace-relative paths of any files the task wrote.
// The task_completed event is the only place these values are exposed because
// TaskRun is not persisted. A failure to read the event bus is returned rather
// than reported as an empty result: an empty answer on a done task is
// indistinguishable from "the task produced nothing", which would let a
// backing-store outage surface to the GUI as a silently truncated answer.
func (s *HTTPServer) taskResult(taskID string) (result string, usage taskUsage, generatedFiles []string, err error) {
	if s.workflowEvents == nil {
		return "", taskUsage{}, nil, nil
	}
	events, err := s.workflowEvents.Events()
	if err != nil {
		return "", taskUsage{}, nil, fmt.Errorf("read runtime events for task %q: %w", taskID, err)
	}
	for _, event := range events {
		if event.TaskID == taskID && event.Type == "task_completed" {
			result = event.Message
			usage = taskUsage{
				PromptTokens:     event.PromptTokens,
				CompletionTokens: event.CompletionTokens,
				CachedTokens:     event.CachedTokens,
				TotalTokens:      event.TotalTokens,
				ElapsedMs:        event.ElapsedMs,
			}
			generatedFiles = event.GeneratedFiles
		}
	}
	return result, usage, generatedFiles, nil
}

// generatedFilesDTO maps workspace-relative paths to their linked DTO form,
// building URL/DownloadURL fresh via fileURL for the given session. It always
// returns a non-nil slice so the field serializes as [] rather than null when
// rels is empty.
func (s *HTTPServer) generatedFilesDTO(sessionID string, rels []string) []GeneratedFile {
	out := make([]GeneratedFile, 0, len(rels))
	for _, rel := range rels {
		out = append(out, GeneratedFile{
			Path:        rel,
			URL:         s.fileURL(sessionID, rel, false),
			DownloadURL: s.fileURL(sessionID, rel, true),
			Name:        filepath.Base(rel),
		})
	}
	return out
}

// runtimeEventsLimit caps how many of the most recent runtime events are
// returned by handleRuntimeEvents, keeping the status-panel payload bounded.
const runtimeEventsLimit = 200

// handleRuntimeEvents returns the most recent runtime events published on the
// workflow event bus (task_started, inference_completed, task_completed,
// learning, ...) in chronological order, capped at runtimeEventsLimit. The
// workflow event bus is an optional dependency: when it is absent the panel has
// nothing to show, so an empty list is the contractually correct response, not a
// silent error.
func (s *HTTPServer) handleRuntimeEvents(w http.ResponseWriter, r *http.Request) {
	if s.workflowEvents == nil {
		writeJSON(w, http.StatusOK, []domain.RuntimeEvent{})
		return
	}
	events, err := s.workflowEvents.Events()
	if err != nil {
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Error("list runtime events failed", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list runtime events: %v", err))
		return
	}
	if len(events) > runtimeEventsLimit {
		events = events[len(events)-runtimeEventsLimit:]
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *HTTPServer) handleWaitingWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.workflows == nil {
		writeJSON(w, http.StatusOK, []storage.WorkflowState{})
		return
	}
	states, err := s.workflows.ListWaitingWorkflowStates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list waiting workflows: %v", err))
		return
	}
	if s.metrics != nil {
		s.metrics.IncWorkflowRun("waiting")
	}
	observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Info("waiting workflows listed", "count", len(states))
	writeJSON(w, http.StatusOK, states)
}

func (s *HTTPServer) handleSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	if s.workflowEngine == nil || s.workflowStates == nil {
		writeError(w, http.StatusServiceUnavailable, "workflow service is unavailable")
		return
	}
	var def workflow.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow request")
		return
	}
	if def.ID == "" {
		writeError(w, http.StatusBadRequest, "workflow id is required")
		return
	}
	result, err := s.workflowEngine.Execute(r.Context(), def)
	if err != nil && result.Status != workflow.StatusWaitingApproval && result.Status != workflow.StatusWaitingEvent {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("execute workflow: %v", err))
		return
	}
	if err := s.workflowStates.SaveWorkflowState(r.Context(), def, result); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save workflow: %v", err))
		return
	}
	if s.metrics != nil {
		s.metrics.IncWorkflowRun(string(result.Status))
	}
	status := http.StatusCreated
	if result.Status == workflow.StatusWaitingApproval || result.Status == workflow.StatusWaitingEvent {
		status = http.StatusAccepted
	}
	writeJSON(w, status, storage.WorkflowState{Definition: def, Result: result, UpdatedAt: time.Now()})
}

func (s *HTTPServer) handleResumeWorkflow(w http.ResponseWriter, r *http.Request) {
	if s.workflowEngine == nil || s.workflowStates == nil || s.workflowEvents == nil {
		writeError(w, http.StatusServiceUnavailable, "workflow service is unavailable")
		return
	}
	workflowID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/workflows/"), "/events")
	if workflowID == "" {
		writeError(w, http.StatusBadRequest, "workflow id is required")
		return
	}
	state, ok, err := s.workflowStates.GetWorkflowState(r.Context(), workflowID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get workflow: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	var event domain.RuntimeEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow event")
		return
	}
	if event.Type == "" {
		writeError(w, http.StatusBadRequest, "event type is required")
		return
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if err := s.workflowEvents.Publish(r.Context(), event); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("publish workflow event: %v", err))
		return
	}
	result, err := s.workflowEngine.Execute(r.Context(), state.Definition)
	if err != nil && result.Status != workflow.StatusWaitingApproval && result.Status != workflow.StatusWaitingEvent {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("resume workflow: %v", err))
		return
	}
	if err := s.workflowStates.SaveWorkflowState(r.Context(), state.Definition, result); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save workflow: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, storage.WorkflowState{Definition: state.Definition, Result: result, UpdatedAt: time.Now()})
}

func (s *HTTPServer) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	if s.workflowStates == nil {
		writeError(w, http.StatusServiceUnavailable, "workflow service is unavailable")
		return
	}
	workflowID := strings.TrimPrefix(r.URL.Path, "/v1/workflows/")
	if workflowID == "" {
		writeError(w, http.StatusBadRequest, "workflow id is required")
		return
	}
	state, ok, err := s.workflowStates.GetWorkflowState(r.Context(), workflowID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get workflow: %v", err))
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if !s.requireCompanyAccess(w, r, state.Definition.CompanyID, "workflow", workflowID) {
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *HTTPServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		snapshot := observability.NewMetricsRecorder(nil).Snapshot()
		if r.URL.Query().Get("format") == "prometheus" {
			writePrometheus(w, snapshot)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	s.metrics.IncHTTPStatus(http.StatusOK)
	snapshot := s.metrics.Snapshot()
	if r.URL.Query().Get("format") == "prometheus" {
		writePrometheus(w, snapshot)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writePrometheus(w http.ResponseWriter, snapshot observability.MetricsSnapshot) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	// Same reasoning as writeJSON: unrepairable, but not to be silent. A scrape
	// that quietly returns half a metrics page produces gaps no one can explain.
	if _, err := w.Write([]byte(observability.PrometheusText(snapshot))); err != nil {
		slog.Default().Warn("write prometheus response", "component", "server", "error", err)
	}
}

type readinessResponse struct {
	Status string            `json:"status"`
	Reason string            `json:"reason,omitempty"`
	Checks map[string]string `json:"checks"`
}

func (s *HTTPServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	resp := readinessResponse{
		Status: "ok",
		Checks: map[string]string{"storage": "ok"},
	}
	if s.readiness != nil {
		if err := s.readiness.Ping(r.Context()); err != nil {
			resp.Status = "unavailable"
			resp.Reason = "storage_unavailable"
			resp.Checks["storage"] = "unavailable"
			writeJSON(w, http.StatusServiceUnavailable, resp)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *HTTPServer) handleDiagnostics(w http.ResponseWriter, _ *http.Request) {
	if s.diagnostics == nil {
		s.diagnostics = observability.NewDiagnostics(observability.DiagnosticsConfig{
			Metrics: s.metrics,
		})
	}
	writeJSON(w, http.StatusOK, s.diagnostics.Snapshot())
}

func (s *HTTPServer) handleTraces(w http.ResponseWriter, _ *http.Request) {
	if s.traces == nil {
		writeJSON(w, http.StatusOK, observability.TraceSnapshot{Spans: []observability.Span{}})
		return
	}
	writeJSON(w, http.StatusOK, s.traces.Snapshot())
}

type createSessionRequest struct {
	Project    string `json:"project"`
	CompanyID  string `json:"company_id"`
	AgentID    string `json:"agent_id"`
	Title      string `json:"title"`
	Mode       string `json:"mode"`
	WorkingDir string `json:"working_dir"`
}

// patchSessionRequest carries the optional, mutable fields of a session update.
// Each is a pointer so the handler can tell "field omitted" (nil, leave as-is)
// apart from "field set to the zero value" (non-nil pointing at "" or false),
// which is what lets a rename touch only the title and an archive touch only the
// archived flag.
type patchSessionRequest struct {
	Title      *string `json:"title"`
	Project    *string `json:"project"`
	Archived   *bool   `json:"archived"`
	Mode       *string `json:"mode"`
	WorkingDir *string `json:"working_dir"`
}

type createTaskRequest struct {
	ID        string   `json:"id"`
	CompanyID string   `json:"company_id"`
	AgentID   string   `json:"agent_id"`
	SessionID string   `json:"session_id"`
	Input     string   `json:"input"`
	Images    []string `json:"images"`
}

type sendAgentMessageRequest struct {
	MessageID     string `json:"message_id"`
	CompanyID     string `json:"company_id"`
	TaskID        string `json:"task_id"`
	SourceEventID string `json:"source_event_id"`
	ThreadID      string `json:"thread_id"`
	From          string `json:"from"`
	FromAgentID   string `json:"from_agent_id"`
	Type          string `json:"type"`
	Summary       string `json:"summary"`
	Artifact      string `json:"artifact"`
}

// skillCommandRequest is the body for the /v1/skills/* endpoints. Install reads
// Source; Update and Uninstall read Name. The unused field for a given endpoint
// is simply ignored.
type skillCommandRequest struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

func agentIDFromMessagesPath(path string) string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/agents/"), "/messages")
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return strings.TrimSpace(trimmed)
}

func parseNonNegativeInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid non-negative integer")
	}
	return value, nil
}

func parseBool(raw string) bool {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func parseAgentMessageType(raw string) domain.AgentMessageType {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "result":
		return domain.AgentMessageTypeResult
	case "handoff":
		return domain.AgentMessageTypeHandoff
	case "review":
		return domain.AgentMessageTypeReview
	default:
		return domain.AgentMessageTypeMessage
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newAgentMessageID() string {
	return "http-msg-" + time.Now().UTC().Format("20060102-150405.000000000")
}

// handleInterruptTask cancels a running task's tool-loop mid-flight so the
// caller (the GUI's stop button) can stop token spend without waiting for the
// model to finish. Path: POST /v1/tasks/{taskID}/interrupt. Responds 204 when
// the interrupt was delivered, 404 when the task is not currently running
// (already finished / unknown) -- the fail-loud contract means a not-running
// task must not be reported as a successful interrupt.
func (s *HTTPServer) handleInterruptTask(w http.ResponseWriter, r *http.Request) {
	if s.taskInterrupter == nil {
		writeError(w, http.StatusServiceUnavailable, "task interrupter is unavailable")
		return
	}
	taskID := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/interrupt"), "/")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	if err := s.taskInterrupter.Interrupt(taskID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDecideApproval records a human approve/deny on a Manual-mode tool
// approval ticket and lets the coordinator resume the task. Path:
// POST /v1/tasks/{taskID}/approvals/{ticketID}, body {"decision":"approve"|"deny"}.
func (s *HTTPServer) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	if s.toolApprovals == nil {
		writeError(w, http.StatusServiceUnavailable, "approval store is unavailable")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	parts := strings.SplitN(rest, "/approvals/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusBadRequest, "malformed approval path")
		return
	}
	taskID, ticketID := parts[0], parts[1]
	var req struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid approval request")
		return
	}
	var status approval.ApprovalStatus
	switch req.Decision {
	case "approve":
		status = approval.ApprovalApproved
	case "deny":
		status = approval.ApprovalDenied
	default:
		writeError(w, http.StatusBadRequest, "decision must be approve or deny")
		return
	}
	decided, err := s.toolApprovals.Decide(r.Context(), taskID, ticketID, status)
	if err != nil {
		if errors.Is(err, approval.ErrTicketNotFound) {
			writeError(w, http.StatusNotFound, "approval ticket not found")
			return
		}
		if errors.Is(err, approval.ErrTicketAlreadyDecided) {
			writeError(w, http.StatusConflict, "approval ticket already decided")
			return
		}
		writeError(w, http.StatusInternalServerError, "decide approval failed")
		return
	}
	writeJSON(w, http.StatusOK, decided)
}

// writeJSON encodes value as the response body.
//
// It keeps its package-function signature — there are ~40 call sites, and
// threading a logger through all of them would be a large change for a failure
// that is diagnostic only — and falls back to the process logger. Silence was
// the actual problem, not the missing plumbing.
func writeJSON(w http.ResponseWriter, status int, value any) {
	writeJSONLogging(slog.Default(), w, status, value)
}

// writeJSONLogging is writeJSON with the logger supplied, so the failure path is
// testable.
//
// The write cannot be repaired: status and headers are already on the wire by
// the time encoding fails, so the client gets 200 OK with an empty or truncated
// body no matter what. Previously the server also recorded nothing, and in the
// GUI that reads as "the call succeeded but the data is empty" — which sends the
// reader looking at the wrong layer entirely.
func writeJSONLogging(logger *slog.Logger, w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		logger.Warn("write json response",
			"component", "server",
			"status", status,
			"error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func newRequestID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(data[:])
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	return r.ResponseWriter.Write(data)
}

// Flush forwards to the wrapped ResponseWriter's Flush when it implements
// http.Flusher. statusRecorder embeds http.ResponseWriter as an interface
// field, so Go's method promotion only exposes the interface's own methods
// (Header/Write/WriteHeader) -- Flush is not part of that interface and is
// therefore never promoted, even though the concrete ResponseWriter beneath
// it (e.g. the stdlib http.response) implements it. Without this passthrough,
// streaming handlers (SSE endpoints) that type-assert w.(http.Flusher) after
// statusRecorder wraps w would always fail the assertion and could never
// push partial writes to the client before the handler returns.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
