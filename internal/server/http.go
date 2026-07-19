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
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/observability"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
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
	AppendConversationTurnIfAbsent(ctx context.Context, turn domain.ConversationTurn) (bool, error)
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

// ApprovalDecider records a human approve/deny decision on a Manual-mode tool
// approval ticket and returns the updated ticket. It is satisfied by
// manualgate.ApprovalCoordinator; the server package depends only on this
// narrow interface to stay decoupled from the manualgate implementation.
type ApprovalDecider interface {
	Decide(ctx context.Context, taskID, ticketID string, status approval.ApprovalStatus) (approval.ToolApproval, error)
}

type Config struct {
	Tasks               TaskStore
	Agents              AgentCatalog
	Workflows           WaitingWorkflowStore
	WorkflowStates      WorkflowStateStore
	WorkflowEngine      *workflow.Engine
	WorkflowEvents      port.EventBus
	PlatformEvents      *observability.EventBus
	Audit               port.AuditLog
	QualityEvals        QualityEvalStore
	Sessions            SessionStore
	Messages            MessageStore
	Skills              SkillManager
	ToolApprovals       ApprovalDecider
	ApprovalTickets     ApprovalLister
	Readiness           ReadinessChecker
	AdminToken          string
	PublicHealthEnabled bool
	RequestIDHeader     string
	// WorkspaceRoot is the base directory for a session's on-disk state when
	// the session carries no working_dir (sessionstate.SessionBase's
	// workspaceRoot argument). Session deletion joins it with the session key
	// to locate the directory to remove alongside the DB row (spec §4.0).
	WorkspaceRoot string
	Logger        *slog.Logger
	Metrics       *observability.MetricsRecorder
	Diagnostics   *observability.Diagnostics
	Traces        *observability.TraceRecorder
}

type HTTPServer struct {
	tasks               TaskStore
	agents              AgentCatalog
	workflows           WaitingWorkflowStore
	workflowStates      WorkflowStateStore
	workflowEngine      *workflow.Engine
	workflowEvents      port.EventBus
	platformEvents      *observability.EventBus
	audit               port.AuditLog
	qualityEvals        QualityEvalStore
	sessions            SessionStore
	messages            MessageStore
	skills              SkillManager
	toolApprovals       ApprovalDecider
	approvalTickets     ApprovalLister
	readiness           ReadinessChecker
	adminToken          string
	publicHealthEnabled bool
	requestIDHeader     string
	workspaceRoot       string
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
	return &HTTPServer{
		tasks:               cfg.Tasks,
		agents:              cfg.Agents,
		workflows:           cfg.Workflows,
		workflowStates:      workflowStates,
		workflowEngine:      cfg.WorkflowEngine,
		workflowEvents:      cfg.WorkflowEvents,
		platformEvents:      cfg.PlatformEvents,
		audit:               cfg.Audit,
		qualityEvals:        cfg.QualityEvals,
		sessions:            cfg.Sessions,
		messages:            cfg.Messages,
		skills:              cfg.Skills,
		toolApprovals:       cfg.ToolApprovals,
		approvalTickets:     cfg.ApprovalTickets,
		readiness:           cfg.Readiness,
		adminToken:          cfg.AdminToken,
		publicHealthEnabled: cfg.PublicHealthEnabled,
		requestIDHeader:     requestIDHeader,
		workspaceRoot:       cfg.WorkspaceRoot,
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
	case r.Method == http.MethodGet && r.URL.Path == "/v1/approvals":
		s.handleListApprovals(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/audit-events":
		s.handleAuditEvents(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime-events":
		s.handleRuntimeEvents(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/quality/evals":
		s.handleQualityEvals(rec, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		s.handleCreateSession(rec, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		s.handleListSessions(rec, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/turns"):
		s.handleListSessionTurns(rec, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && !strings.HasSuffix(r.URL.Path, "/turns"):
		s.handlePatchSession(rec, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/") && !strings.HasSuffix(r.URL.Path, "/turns"):
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
	return r.Header.Get("Authorization") == "Bearer "+s.adminToken
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session request")
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
		writeJSON(w, http.StatusOK, []domain.ConversationTurn{})
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
	writeJSON(w, http.StatusOK, turns)
}

// sessionIDFromPath extracts the {id} segment from /v1/sessions/{id}. It returns
// "" when the trimmed remainder is empty or still contains a slash (a nested
// path that this handler does not own), so the caller can reject it as a bad
// request instead of acting on a malformed id.
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid session patch request")
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
	// Record the user turn before the task is enqueued so the conversation
	// history exists even if the runtime never produces an answer. session_id is
	// an optional field (a one-off task may carry none), but when it is present
	// the session must exist — a missing session is a client error, not a state
	// we silently paper over.
	if haveSession {
		if err := s.recordUserTurn(r.Context(), w, task, session); err != nil {
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

// userTurnContent renders the persisted text for a user turn. Base64 image data
// is deliberately not stored in conversation_turns (it would bloat sqlite);
// instead, when the task carried images, a "[附图 N 张]" marker is appended so the
// replayed history shows that images were attached without re-embedding them.
func userTurnContent(input string, imageCount int) string {
	if imageCount <= 0 {
		return input
	}
	marker := fmt.Sprintf("[附图 %d 张]", imageCount)
	if strings.TrimSpace(input) == "" {
		return marker
	}
	return input + "\n" + marker
}

// recordUserTurn persists the user prompt as a conversation turn and refreshes
// the owning session's updated_at. The session must already be loaded by the
// caller (handleCreateTask resolves it once, to derive the task's mode, and
// passes it here rather than querying it a second time). It writes a 5xx
// response and returns a non-nil error when the write fails, so the caller
// aborts loudly instead of enqueuing a task whose history was lost. The turn
// id is deterministic ("<taskID>:user") so a retried submission cannot
// duplicate it.
func (s *HTTPServer) recordUserTurn(ctx context.Context, w http.ResponseWriter, task domain.Task, session domain.AgentSession) error {
	turn := domain.ConversationTurn{
		ID:        task.ID + ":user",
		SessionID: task.SessionID,
		TaskID:    task.ID,
		AgentID:   task.AgentID,
		Role:      domain.ConversationRoleUser,
		Content:   userTurnContent(task.Input, len(task.Images)),
		CreatedAt: task.CreatedAt,
	}
	if _, err := s.sessions.AppendConversationTurnIfAbsent(ctx, turn); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("record user turn: %v", err))
		return err
	}
	// Refresh the session's updated_at even when the turn already existed (a
	// resubmission), so the session sorts to the top of the list. Preserve the
	// existing project/title/created_at by re-saving the loaded session.
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
	TaskID           string `json:"task_id"`
	Status           string `json:"status"`
	Result           string `json:"result"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	ElapsedMs        int64  `json:"elapsed_ms"`
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
	result, usage, err := s.taskResult(taskID)
	if err != nil {
		observability.WithRequestID(s.logger, requestIDFromContext(r.Context())).Error("read task result failed", "task_id", taskID, "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("task result: %v", err))
		return
	}
	// On a completed task tied to a session, persist the assistant answer as a
	// conversation turn so the GUI can reload the full history. The turn id is
	// deterministic ("<taskID>:assistant"), and the insert is exactly-once, so
	// repeated polling of this endpoint yields a single assistant turn.
	if task.Status == domain.TaskDone && strings.TrimSpace(task.SessionID) != "" && strings.TrimSpace(result) != "" {
		if err := s.recordAssistantTurn(r.Context(), task, result); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("record assistant turn: %v", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, taskResultResponse{
		TaskID:           taskID,
		Status:           string(task.Status),
		Result:           result,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CachedTokens:     usage.CachedTokens,
		TotalTokens:      usage.TotalTokens,
		ElapsedMs:        usage.ElapsedMs,
	})
}

// recordAssistantTurn persists the model answer for a completed task exactly
// once, keyed by "<taskID>:assistant". A nil session store while a task carries a
// session id is an inconsistent state and is surfaced as an error rather than
// ignored.
func (s *HTTPServer) recordAssistantTurn(ctx context.Context, task domain.Task, result string) error {
	if s.sessions == nil {
		return fmt.Errorf("session store is unavailable")
	}
	turn := domain.ConversationTurn{
		ID:        task.ID + ":assistant",
		SessionID: task.SessionID,
		TaskID:    task.ID,
		AgentID:   task.AgentID,
		Role:      domain.ConversationRoleAssistant,
		Content:   result,
		CreatedAt: time.Now(),
	}
	if _, err := s.sessions.AppendConversationTurnIfAbsent(ctx, turn); err != nil {
		return err
	}
	return nil
}

// taskResult scans the runtime event bus for the task_completed event of the
// given task and returns its answer text, total token usage, and elapsed time
// in milliseconds. The task_completed event is the only place these values are
// exposed because TaskRun is not persisted. A failure to read the event bus is
// returned rather than reported as an empty result: an empty answer on a done
// task is indistinguishable from "the task produced nothing", which would let a
// backing-store outage surface to the GUI as a silently truncated answer.
func (s *HTTPServer) taskResult(taskID string) (result string, usage taskUsage, err error) {
	if s.workflowEvents == nil {
		return "", taskUsage{}, nil
	}
	events, err := s.workflowEvents.Events()
	if err != nil {
		return "", taskUsage{}, fmt.Errorf("read runtime events for task %q: %w", taskID, err)
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
		}
	}
	return result, usage, nil
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
	_, _ = w.Write([]byte(observability.PrometheusText(snapshot)))
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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
