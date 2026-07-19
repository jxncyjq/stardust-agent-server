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
			"/v1/events":                          {Get: openAPIOperation("subscribeEvents", "Subscribe platform events", true)},
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

func openAPIOperation(id string, summary string, secured bool) *OpenAPIOperation {
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
	return op
}

func objectSchema() map[string]any {
	return map[string]any{"type": "object"}
}

func errorResponse(status string) map[string]any {
	descriptions := map[string]string{
		"400": "Bad request",
		"401": "Unauthorized",
		"403": "Forbidden",
		"500": "Internal server error",
	}
	return map[string]any{
		"description": descriptions[status],
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
