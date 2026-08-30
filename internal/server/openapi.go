package server

type OpenAPISpec struct {
	OpenAPI    string                     `json:"openapi"`
	Info       OpenAPIInfo                `json:"info"`
	Paths      map[string]OpenAPIPathItem `json:"paths"`
	Components OpenAPIComponents          `json:"components"`
}

type OpenAPIInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type OpenAPIPathItem struct {
	Get    *OpenAPIOperation `json:"get,omitempty"`
	Post   *OpenAPIOperation `json:"post,omitempty"`
	Patch  *OpenAPIOperation `json:"patch,omitempty"`
	Delete *OpenAPIOperation `json:"delete,omitempty"`
}

type OpenAPIOperation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary"`
	Responses   map[string]any        `json:"responses"`
	Security    []map[string][]string `json:"security,omitempty"`
}

type OpenAPIComponents struct {
	Schemas         map[string]any `json:"schemas"`
	SecuritySchemes map[string]any `json:"securitySchemes"`
}

func BuildOpenAPISpec() OpenAPISpec {
	return OpenAPISpec{
		OpenAPI: "3.1.0",
		Info: OpenAPIInfo{
			Title:   "Legion Agent API",
			Version: "0.1.0",
		},
		Paths: map[string]OpenAPIPathItem{
			"/healthz":                            {Get: openAPIOperation("getHealthz", "Health check", false)},
			"/readyz":                             {Get: openAPIOperation("getReadyz", "Readiness check", false)},
			"/metrics":                            {Get: openAPIOperation("getMetrics", "Metrics snapshot", true)},
			"/debug/diagnostics":                  {Get: openAPIOperation("getDiagnostics", "Diagnostics snapshot", true)},
			"/debug/traces":                       {Get: openAPIOperation("getTraces", "Trace snapshot", true)},
			"/openapi.json":                       {Get: openAPIOperation("getOpenAPI", "OpenAPI contract", false)},
			"/v1/approvals":                       {Get: openAPIOperation("listApprovals", "List pending Manual-mode approval tickets", true)},
			"/v1/audit-events":                    {Get: openAPIOperation("listAuditEvents", "List audit events", true)},
			"/v1/runtime-events":                  {Get: openAPIOperation("listRuntimeEvents", "List recent runtime events", true)},
			"/v1/quality/evals":                   {Get: openAPIOperation("listQualityEvals", "List quality evaluation runs", true)},
			"/v1/sessions":                        {Get: openAPIOperation("listSessions", "List agent sessions", true), Post: openAPIOperation("createSession", "Create agent session", true)},
			"/v1/sessions/{id}":                   {Patch: openAPIOperation("patchSession", "Update session mode or working directory", true), Delete: openAPIOperation("deleteSession", "Delete agent session", true)},
			"/v1/sessions/{id}/turns":             {Get: openAPIOperation("listSessionTurns", "List session conversation turns", true)},
			"/v1/agents":                          {Get: openAPIOperation("listAgents", "List configured sub-agents", true)},
			"/v1/agents/{id}/messages":            {Get: openAPIOperation("listAgentMessages", "List agent messages", true), Post: openAPIOperation("sendAgentMessage", "Send agent message", true)},
			"/v1/tasks":                           {Get: openAPIOperation("listTasks", "List tasks", true), Post: openAPIOperation("submitTask", "Submit task", true)},
			"/v1/tasks/{id}":                      {Get: openAPIOperation("getTask", "Get task status", true)},
			"/v1/tasks/{id}/result":               {Get: openAPIOperation("getTaskResult", "Get task status and answer text", true)},
			"/v1/tasks/{id}/approvals/{ticketID}": {Post: openAPIOperation("decideApproval", "Approve or deny a Manual-mode approval ticket", true)},
			"/v1/workflows":                       {Post: openAPIOperation("submitWorkflow", "Submit workflow", true)},
			"/v1/workflows/{id}":                  {Get: openAPIOperation("getWorkflow", "Get workflow state", true)},
			"/v1/workflows/{id}/events":           {Post: openAPIOperation("resumeWorkflowEvent", "Resume workflow event", true)},
			"/v1/workflows/waiting":               {Get: openAPIOperation("listWaitingWorkflows", "List waiting workflows", true)},
			"/v1/skills/install":                  {Post: openAPIOperation("installSkill", "Install skill", true)},
			"/v1/skills/update":                   {Post: openAPIOperation("updateSkill", "Update skill", true)},
			"/v1/skills/uninstall":                {Post: openAPIOperation("uninstallSkill", "Uninstall skill", true)},
			"/v1/plugins":                         {Get: openAPIOperation("listPlugins", "List deployment plugins with their declared and granted capabilities", true)},
			"/v1/plugins/{name}/grant":            {Post: openAPIOperation("grantPlugin", "Authorize a plugin's declared capabilities, hosts and paths", true)},
			"/v1/plugins/{name}/deny":             {Post: openAPIOperation("denyPlugin", "Revoke a plugin's authorization to run", true)},
			// 422 is declared explicitly here and nowhere else: it is the one
			// status a client is REQUIRED to branch on rather than treat
			// generically (an untrusted package can never become trusted by
			// retrying, so a client must not offer a retry), and the spec is
			// the only place a non-GUI client would learn it exists. See
			// ErrPluginUntrusted and pluginConsentStatus.
			"/v1/plugins/{name}/resolve": {Post: openAPIOperation("resolvePlugin", "Fetch and verify a plugin's package to see what it declares, without authorizing it", true, "422")},
			"/v1/events":                 {Get: openAPIOperation("subscribeEvents", "Subscribe platform events", true)},
			"/v1/files":                  {Get: openAPIOperation("getFile", "Stream a generated file from a session's working directory", true)},
			"/v1/tasks/{id}/interrupt":   {Post: openAPIOperation("interruptTask", "Interrupt a running task", true)},
			// 凭证轮换：409 是这个端点特有的一条，声明出来才让客户端知道它存在——
			// 本机开放部署没有可轮换的凭证，那不是错误也不是重试能解决的事。
			"/v1/auth/rotate": {Post: openAPIOperation("rotateToken", "Revoke the current bearer token and mint a replacement", true, "409")},
			// 浏览器的六个端点此前一条都不在契约里：GUI 走 Wails 绑定不受影响，而按
			// OpenAPI 生成客户端的人拿不到它们，只能手写。
			"/v1/browser/sessions/{id}/stream": {Get: openAPIOperation("streamBrowserSession",
				"Subscribe a browser session's screencast frames and status events (SSE)", true)},
			"/v1/browser/sessions/{id}/info": {Get: openAPIOperation("getBrowserSession",
				"Where the browser is, whether a human has taken over, and whether the page still exists", true)},
			"/v1/browser/sessions/{id}/takeover": {Post: openAPIOperation("setBrowserTakeover",
				"Hand the session to a human, or give it back to the agent", true)},
			"/v1/browser/sessions/{id}/input": {Post: openAPIOperation("injectBrowserInput",
				"Inject mouse/keyboard events into a session under human takeover", true, "409")},
			"/v1/browser/sessions/{id}/navigate": {Post: openAPIOperation("navigateBrowserSession",
				"Navigate by hand (address, back, forward, reload) in a session under takeover", true, "409")},
			"/v1/browser/sessions/{id}/viewport": {Post: openAPIOperation("setBrowserViewport",
				"Set the session viewport so screencast frames match the viewer's aspect", true)},
		},
		Components: OpenAPIComponents{
			Schemas: map[string]any{
				"TaskSubmitRequest":     objectSchema(),
				"Task":                  objectSchema(),
				"WorkflowSubmitRequest": objectSchema(),
				"WorkflowState":         objectSchema(),
				"DiagnosticsSnapshot":   objectSchema(),
				"MetricsSnapshot":       objectSchema(),
				"EventEnvelope":         objectSchema(),
				"AuditEvent":            objectSchema(),
				"QualityEvalRun":        objectSchema(),
				"AgentSession":          objectSchema(),
				"ConversationTurn":      objectSchema(),
				"AgentMessage":          objectSchema(),
				"AgentMessageRequest":   objectSchema(),
				"SessionCreateRequest":  objectSchema(),
				"SessionPatchRequest":   objectSchema(),
				"ApprovalTicket":        objectSchema(),
				"ApprovalDecision":      objectSchema(),
				"SkillCommandRequest":   objectSchema(),
				"PluginView":            objectSchema(),
				"PluginGrantRequest":    objectSchema(),
				"PluginConsentResponse": objectSchema(),
				"TraceSnapshot":         objectSchema(),
				"ErrorResponse":         errorResponseSchema(),
			},
			SecuritySchemes: map[string]any{
				"AdminToken": map[string]any{
					"type": "apiKey",
					"in":   "header",
					"name": "Authorization",
				},
			},
		},
	}
}

// openAPIOperation builds one operation with the response set every secured
// endpoint in this API shares (200 plus 400/401/403/500).
//
// extraStatuses adds statuses beyond that shared set, for an operation whose
// contract a client cannot handle generically. It is variadic rather than a
// required argument on purpose: every existing caller keeps the identical
// shared shape, so a status appearing on one operation and not its siblings
// is a deliberate, visible choice at that one call site rather than a
// silent per-endpoint divergence. Each status must have a description in
// errorResponse, which panics on one it does not know rather than emitting a
// response object with an empty description.
func openAPIOperation(id string, summary string, secured bool, extraStatuses ...string) *OpenAPIOperation {
	op := &OpenAPIOperation{
		OperationID: id,
		Summary:     summary,
		Responses: map[string]any{
			"200": map[string]any{"description": "OK"},
		},
	}
	if secured {
		op.Security = []map[string][]string{{"AdminToken": {}}}
		for _, status := range []string{"400", "401", "403", "500"} {
			op.Responses[status] = errorResponse(status)
		}
	}
	for _, status := range extraStatuses {
		op.Responses[status] = errorResponse(status)
	}
	return op
}

func objectSchema() map[string]any {
	return map[string]any{"type": "object"}
}

// errorResponse builds the response object for one error status. An unknown
// status panics: emitting a response with an empty description would publish
// a contract that says less than the code does, which is worse than failing
// the build that introduced it.
func errorResponse(status string) map[string]any {
	descriptions := map[string]string{
		"400": "Bad request",
		"401": "Unauthorized",
		"403": "Forbidden",
		"404": "Not found",
		// 409 是「东西在，但现在的状态做不了这件事」：会话没有进入接管、没有可轮换
		// 的凭证、页面已被回收。与 400 分开是因为补救动作不同——改状态再来，而不是
		// 改请求。
		"409": "Conflict: the resource exists but is in the wrong state for this call",
		"422": "Unprocessable entity",
		"500": "Internal server error",
	}
	description, ok := descriptions[status]
	if !ok {
		panic("server: openapi: no description for error status " + status)
	}
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{
					"$ref": "#/components/schemas/ErrorResponse",
				},
			},
		},
	}
}

func errorResponseSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"error"},
		"properties": map[string]any{
			"error": map[string]any{
				"type": "string",
			},
		},
	}
}
