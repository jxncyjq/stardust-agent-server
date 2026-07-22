package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/cognitive"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/evolution"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/sessionstate"
	"github.com/stardust/legion-agent/internal/tool"
)

var (
	ErrInterrupted     = errors.New("runtime interrupted")
	ErrMaasUnavailable = errors.New("maas inference client unavailable")
	// ErrSuspended is returned by RunTask when the ToolGate pauses execution at a
	// tool-round boundary. The runtime has already written a checkpoint; the
	// coordinator maps this to TaskSuspended (not TaskFailed) and the goroutine
	// is released. A later run (this process or after restart) auto-resumes.
	ErrSuspended = errors.New("runtime suspended pending decision")
	// ErrManualGateMissing is returned by RunTask when a Manual-mode task reaches a
	// runtime whose approval gate is not wired (nil toolGate or nil checkpoints).
	// Manual mode's entire safety guarantee is that sensitive tool calls suspend for
	// human approval; a nil gate never suspends and a nil checkpoint store cannot
	// persist a suspension, so either would let the task silently execute sensitive
	// tools and bypass approval. This is an invariant violation (a misconfigured
	// runtime path), not a task-content failure: we fail loud here rather than
	// degrade to Auto behaviour. See CLAUDE.md §0 fail-loud.
	ErrManualGateMissing = errors.New("manual mode requires an approval gate: runtime has nil toolGate or nil checkpoints")
)

const defaultMaxToolRounds = 4

type ContextBuilder interface {
	BuildContext(ctx context.Context, req cognitive.Request) (cognitive.BuiltContext, error)
}

// ToolGate decides, at each tool-round boundary, whether the runtime must
// suspend before executing the given pending tool calls (e.g. awaiting human
// approval in Manual mode). A nil gate never suspends — Auto behaviour. M1b ships
// only the seam; the approval-backed implementation lands in M2.
type ToolGate interface {
	// ShouldSuspend reports whether the runtime must suspend before executing this
	// round's calls. tools is the run's effective registry (for sensitivity lookup).
	ShouldSuspend(ctx context.Context, task domain.Task, calls []domain.ToolCall, tools *tool.Registry) (bool, error)
	// Resolve reports, at dispatch time for one call, whether it may execute.
	Resolve(ctx context.Context, task domain.Task, call domain.ToolCall, tools *tool.Registry) (allow bool, err error)
}

type Config struct {
	Maas           port.MaasInferenceClient
	Audit          port.AuditLog
	Events         port.EventBus
	ContextBuilder ContextBuilder
	ContextPrefix  string
	Tools          *tool.Registry
	MaxToolRounds  int
	// LazyTools selects the on-demand meta-tool protocol. When true the model is
	// offered only list_tools/call_tool and discovers/invokes real tools through
	// them, keeping simple no-tool chats cheap. When false the full native tool
	// schema is offered every round (legacy behaviour, safety rollback).
	LazyTools bool
	// MaxToolResultChars caps a single tool result before it is appended to the
	// prompt; MaxPromptChars caps the whole accumulated tool-loop prompt. Zero
	// falls back to safe defaults.
	MaxToolResultChars int
	MaxPromptChars     int
	ConversationTurns  []domain.ConversationTurn
	// Delegation controls. Role is "orchestrator" (may spawn sub-tasks) or "leaf"
	// (may not); an empty Role at the root (Depth 0) defaults to orchestrator, and
	// spawned children default to leaf. Depth is the current delegation depth (0
	// at the root). MaxSpawnDepth caps how deep orchestrators may nest; MaxConcurrent
	// bounds parallel sub-tasks in a batch. Zero MaxSpawnDepth/MaxConcurrent fall
	// back to safe defaults.
	Role          string
	Depth         int
	MaxSpawnDepth int
	MaxConcurrent int
	// Checkpoints persists suspended tool-loop state so a task can resume after
	// its goroutine is released (and after a process restart). Nil disables
	// suspend/resume (the loop runs straight through, legacy behaviour).
	Checkpoints *sessionstate.Store
	// ToolGate gates each tool round for suspension. Nil never suspends.
	ToolGate ToolGate
	// Logger records failures at the boundaries where there is no caller left to
	// return an error to: a failure-learning signal the event bus rejected, and
	// the audit fallback a delegated sub-task falls back to when its own event
	// publish already failed.
	//
	// Nil falls back to slog.Default() rather than to silence. A missing logger
	// is a wiring oversight, not a request to discard diagnostics — the same
	// mistake as the file logger that used to degrade to io.Discard.
	Logger *slog.Logger
	// SkillUsage records that a skill was actually loaded. The Curator ages
	// idle skills off this record, and leaves skills with no usage history
	// alone -- so a runtime that never touches it silently disables the sweep.
	SkillUsage SkillUsageRecorder
	// CapabilitySkills is the skill half of the capability catalog: the provider
	// that lists an agent's loadable skills and returns their bodies. The tool
	// half is built per task from the run's effective registry, so the catalog is
	// scoped to exactly what that task may load and dispatch. Nil means no skills
	// are catalogued (the catalog then lists only tools). It is consulted only
	// under the lazy protocol, the only protocol that offers load_capabilities.
	CapabilitySkills capability.Provider
}

// SkillUsageRecorder is the usage sidecar skill.UsageStore satisfies.
type SkillUsageRecorder interface {
	Touch(id string, at time.Time)
}

// Context-accumulation bounds for the tool-execution loop. Tool outputs are
// appended to the prompt and re-sent on every round, so without caps a single
// large tool result (e.g. a big file read) re-enters context every round and the
// prompt grows unbounded across rounds.
const (
	defaultMaxToolResultChars = 4000  // per single tool result, before truncation
	defaultMaxPromptChars     = 16000 // whole accumulated tool-loop prompt (re-sent each round)
)

type Runtime struct {
	maas               port.MaasInferenceClient
	audit              port.AuditLog
	events             port.EventBus
	contextBuilder     ContextBuilder
	contextPrefix      string
	tools              *tool.Registry
	maxToolRounds      int
	maxToolResultChars int
	maxPromptChars     int
	lazyTools          bool
	conversationTurns  []domain.ConversationTurn
	interrupted        atomic.Bool
	role               string
	depth              int
	maxSpawnDepth      int
	maxConcurrent      int
	subTaskSeq         atomic.Uint64
	checkpoints        *sessionstate.Store
	toolGate           ToolGate
	logger             *slog.Logger
	skillUsage         SkillUsageRecorder
	capabilitySkills   capability.Provider
}

// loopState is the mutable state threaded through the tool-execution loop.
// runToolLoop advances it; a suspend serialises the relevant fields to a
// checkpoint and a resume rebuilds it from one.
type loopState struct {
	started          time.Time
	basePrompt       string
	round            int
	toolCtx          []toolEntry
	loaded           []loadedEntry
	resp             port.InferenceResponse
	promptTokens     int
	completionTokens int
	cachedTokens     int
	totalTokens      int
	// images is checkpoint-consistent: on resume it comes from the loaded
	// checkpoint (not the live task), so a resumed run keeps the images it
	// was suspended with even if the reconstructed task no longer carries them.
	images []string
	// tools is the per-call effective tool registry resolved once at RunTask
	// entry via effectiveTools. It must be used for both offering tools to the
	// model (inferenceTools) and dispatching them (dispatchToolCall), so a run
	// never dispatches against a broader set than it offered.
	tools *tool.Registry
	// catalog is the per-call capability catalog, built from the same effective
	// registry as tools (buildCatalog). It is what the prompt advertises and what
	// load_capabilities loads from, so both are scoped identically -- a Plan-mode
	// run cannot load a sensitive tool that was filtered out of tools. Nil under
	// the eager protocol, which offers native schemas and never load_capabilities.
	catalog *capability.Catalog
}

// effectiveTools returns the tool registry a run should use: in Plan mode only
// the non-sensitive (read-only) subset, so a planning run can research but never
// cause side effects; every other mode uses the full registry unchanged. It never
// mutates r.tools and returns a fresh per-call Registry, safe under concurrent
// tasks sharing this Runtime.
func (r *Runtime) effectiveTools(task domain.Task) *tool.Registry {
	if r.tools != nil && task.Mode == domain.ModePlan {
		return r.tools.Subset(r.tools.SafeToolNames()...)
	}
	return r.tools
}

// buildCatalog assembles the per-task capability catalog from the run's
// effective tool registry plus the assembly-provided skill provider. It is the
// single source both the prompt (what to advertise) and dispatch
// (load_capabilities) draw from, so they are always scoped to the same set: a
// Plan-mode run passes its read-only subset here, and so cannot advertise or
// load a sensitive tool it may not run.
//
// It returns nil under the eager protocol: eager offers full native schemas and
// never load_capabilities, so a catalog would be dead weight in the prompt. That
// is why a group-less registry only matters under the lazy protocol -- a tool
// with no catalog group fails loud in ToolProvider.Entries, but only a lazy run
// builds a catalog to hit it.
func (r *Runtime) buildCatalog(tools *tool.Registry) *capability.Catalog {
	if !r.lazyTools {
		return nil
	}
	providers := make([]capability.Provider, 0, 2)
	if tools != nil {
		providers = append(providers, capability.NewToolProvider(tools))
	}
	if r.capabilitySkills != nil {
		providers = append(providers, r.capabilitySkills)
	}
	return capability.NewCatalog(providers...)
}

// planInstruction is appended to the base prompt in Plan mode, directing the
// model to research and produce a structured plan instead of taking any
// side-effecting action; it pairs with effectiveTools restricting the actually
// offered/dispatched tools to the read-only subset.
const planInstruction = "\n\n[系统] 当前为 Plan 模式：只做调研与分析，产出一份结构化的执行计划（步骤、涉及文件、验证方式），不要执行任何有副作用的操作。只可使用只读工具。"

func NewRuntime(cfg Config) *Runtime {
	audit := cfg.Audit
	if audit == nil {
		audit = noopAuditLog{}
	}
	events := cfg.Events
	if events == nil {
		events = noopEventBus{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	role := cfg.Role
	if role == "" {
		if cfg.Depth == 0 {
			role = roleOrchestrator
		} else {
			role = roleLeaf
		}
	}
	return &Runtime{
		maas:               cfg.Maas,
		audit:              audit,
		events:             events,
		contextBuilder:     cfg.ContextBuilder,
		contextPrefix:      strings.TrimSpace(cfg.ContextPrefix),
		tools:              cfg.Tools,
		maxToolRounds:      normalizeMaxToolRounds(cfg.MaxToolRounds),
		maxToolResultChars: normalizePositive(cfg.MaxToolResultChars, defaultMaxToolResultChars),
		maxPromptChars:     normalizePositive(cfg.MaxPromptChars, defaultMaxPromptChars),
		lazyTools:          cfg.LazyTools,
		conversationTurns:  append([]domain.ConversationTurn(nil), cfg.ConversationTurns...),
		role:               role,
		depth:              cfg.Depth,
		maxSpawnDepth:      normalizePositive(cfg.MaxSpawnDepth, defaultMaxSpawnDepth),
		maxConcurrent:      normalizePositive(cfg.MaxConcurrent, defaultMaxConcurrent),
		checkpoints:        cfg.Checkpoints,
		toolGate:           cfg.ToolGate,
		logger:             logger,
		skillUsage:         cfg.SkillUsage,
		capabilitySkills:   cfg.CapabilitySkills,
	}
}

func normalizeMaxToolRounds(rounds int) int {
	if rounds <= 0 {
		return defaultMaxToolRounds
	}
	return rounds
}

func normalizePositive(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func (r *Runtime) Interrupt() {
	r.interrupted.Store(true)
}

func (r *Runtime) RunTask(ctx context.Context, agent domain.Agent, task domain.Task) (domain.TaskRun, error) {
	started := time.Now()
	requestID := task.ID + ":run"
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:      "task_started",
		TaskID:    task.ID,
		Message:   "runtime started",
		CreatedAt: started,
	}); err != nil {
		return domain.TaskRun{}, fmt.Errorf("publish task started event: %w", err)
	}
	if r.interrupted.Load() {
		r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInterrupted)
		return domain.TaskRun{}, ErrInterrupted
	}
	if r.maas == nil {
		r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
		return domain.TaskRun{}, ErrMaasUnavailable
	}

	// Manual-mode invariant: the approval gate must be wired before we run a single
	// round. Today every runtime that can carry a Manual task (default + resolver-built
	// per-agent) wires both, and delegated children are always Auto — but that safety is
	// implicit. Assert it loudly so any future path (a child inheriting Mode=manual, a new
	// runtime constructor) that reaches here without a gate fails fast instead of silently
	// executing sensitive tools and bypassing human approval.
	if task.Mode == domain.ModeManual && (r.toolGate == nil || r.checkpoints == nil) {
		return domain.TaskRun{}, fmt.Errorf("run task %s: %w", task.ID, ErrManualGateMissing)
	}

	// Resume path: a persisted checkpoint means this task previously suspended.
	// Rebuild loop state from disk and re-enter the loop with the pending calls,
	// skipping the initial prompt build + generate.
	if r.checkpoints != nil {
		cp, ok, err := r.checkpoints.Load(sessionKeyForTask(task), task.WorkingDir)
		if err != nil {
			return domain.TaskRun{}, fmt.Errorf("load checkpoint for task %s: %w", task.ID, err)
		}
		if ok {
			// The checkpoint is authoritative for the resumed run's mode: a caller
			// (coordinator resume) may hand us a task rebuilt from the scheduler; the
			// mode captured at suspend time must win so gating stays consistent.
			task.Mode = cp.Mode
			effTools := r.effectiveTools(task)
			st := loopState{
				started:          started,
				basePrompt:       cp.BasePrompt,
				round:            cp.Round,
				toolCtx:          restoreToolEntries(cp.ToolEntries),
				loaded:           restoreLoaded(cp.Loaded),
				resp:             port.InferenceResponse{ToolCalls: cp.PendingCalls},
				promptTokens:     cp.PromptTokens,
				completionTokens: cp.CompletionTokens,
				cachedTokens:     cp.CachedTokens,
				totalTokens:      cp.TotalTokens,
				images:           cp.Images,
				tools:            effTools,
				// The resumed prompt's catalog is already baked into cp.BasePrompt
				// from the first run; this rebuilds the dispatch-side catalog so a
				// load_capabilities issued in a resumed round still resolves, scoped
				// to the same effective registry.
				catalog: r.buildCatalog(effTools),
			}
			return r.runToolLoop(ctx, requestID, agent, task, st)
		}
	}

	effTools := r.effectiveTools(task)
	catalog := r.buildCatalog(effTools)
	prompt, err := r.buildPrompt(ctx, agent, task, catalog)
	if err != nil {
		return domain.TaskRun{}, err
	}
	if task.Mode == domain.ModePlan {
		prompt += planInstruction
	}
	// basePrompt is the fixed task framing (system + task). It is reused verbatim
	// as the head of every tool-round prompt, so it is the stable prefix that
	// drives the provider prompt-cache breakpoint (InferenceRequest.StablePrefixLen).
	basePrompt := prompt
	resp, err := r.generate(ctx, requestID, prompt, task.Images, len([]rune(basePrompt)), effTools)
	if err != nil {
		r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
		return domain.TaskRun{}, fmt.Errorf("generate inference: %w", err)
	}
	st := loopState{
		started:          started,
		basePrompt:       basePrompt,
		round:            0,
		resp:             resp,
		promptTokens:     resp.PromptTokens,
		completionTokens: resp.CompletionTokens,
		cachedTokens:     resp.CachedTokens,
		totalTokens:      resp.TotalTokens,
		images:           task.Images,
		tools:            effTools,
		catalog:          catalog,
	}
	return r.runToolLoop(ctx, requestID, agent, task, st)
}

// runToolLoop advances the tool-execution loop from st until the model stops
// requesting tools (or the round budget is exhausted), then finalises the run.
// Before executing each round's tool calls it consults the ToolGate: if the gate
// says suspend, it writes a checkpoint and returns ErrSuspended, releasing the
// goroutine. A successfully completed run deletes any checkpoint.
func (r *Runtime) runToolLoop(ctx context.Context, requestID string, agent domain.Agent, task domain.Task, st loopState) (domain.TaskRun, error) {
	// toolCtx accumulates tool results deduplicated by (tool, arguments), so
	// re-reading the same file/URL keeps only the latest copy in context instead
	// of stacking duplicates each round.
	for st.round < r.maxToolRounds && len(st.resp.ToolCalls) > 0 {
		suspend, err := r.checkSuspend(ctx, task, st)
		if err != nil {
			return domain.TaskRun{}, err
		}
		if suspend {
			return domain.TaskRun{}, ErrSuspended
		}
		results, err := r.executeToolCalls(ctx, agent, task, &st)
		if err != nil {
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonToolError)
			return domain.TaskRun{}, fmt.Errorf("execute model tool calls: %w", err)
		}
		st.toolCtx = mergeToolResults(st.toolCtx, st.resp.ToolCalls, results, r.maxToolResultChars)
		prompt := composePrompt(st.basePrompt, st.loaded, st.toolCtx, r.maxPromptChars)
		st.resp, err = r.generate(ctx, requestID, prompt, st.images, stablePrefixRunes(prompt, st.basePrompt), st.tools)
		if err != nil {
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
			return domain.TaskRun{}, fmt.Errorf("generate inference after tools: %w", err)
		}
		st.promptTokens += st.resp.PromptTokens
		st.completionTokens += st.resp.CompletionTokens
		st.cachedTokens += st.resp.CachedTokens
		st.totalTokens += st.resp.TotalTokens
		st.round++
	}
	if len(st.resp.ToolCalls) > 0 {
		// Tool-round budget exhausted but the model still wants tools. Rather
		// than hard-failing the whole task (which discards every tool result
		// gathered so far and surfaces as "任务执行失败" to the user), make a
		// final inference with no tools offered, and explicitly instruct the
		// model to answer rather than narrate another tool call — otherwise it
		// tends to emit text like "list_files 参数: {...}" instead of a real
		// answer when it is cut off mid-exploration.
		prompt := composePrompt(st.basePrompt, st.loaded, st.toolCtx, r.maxPromptChars)
		finalPrompt := prompt + "\n\n[系统] 工具调用已达上限。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		final, err := r.generateNoTools(ctx, requestID, finalPrompt, st.images, stablePrefixRunes(finalPrompt, st.basePrompt))
		if err != nil {
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
			return domain.TaskRun{}, fmt.Errorf("generate final answer after tool budget exhausted: %w", err)
		}
		st.promptTokens += final.PromptTokens
		st.completionTokens += final.CompletionTokens
		st.cachedTokens += final.CachedTokens
		st.totalTokens += final.TotalTokens
		st.resp = final
	}
	return r.finishRun(ctx, requestID, agent, task, st)
}

// checkSuspend consults the ToolGate for the current round's pending calls and,
// when the gate says pause, persists a checkpoint so the run can resume later.
// It returns true only after the checkpoint is safely on disk (fail-loud on
// write error — never suspend with lost state). Nil gate or nil store → false.
func (r *Runtime) checkSuspend(ctx context.Context, task domain.Task, st loopState) (bool, error) {
	if r.toolGate == nil || r.checkpoints == nil {
		return false, nil
	}
	suspend, err := r.toolGate.ShouldSuspend(ctx, task, st.resp.ToolCalls, st.tools)
	if err != nil {
		return false, fmt.Errorf("tool gate decision for task %s: %w", task.ID, err)
	}
	if !suspend {
		return false, nil
	}
	cp := sessionstate.Checkpoint{
		SchemaVersion:    sessionstate.CheckpointSchemaVersion,
		TaskID:           task.ID,
		AgentID:          task.AgentID,
		SessionKey:       sessionKeyForTask(task),
		Mode:             task.Mode,
		BasePrompt:       st.basePrompt,
		Round:            st.round,
		ToolEntries:      snapshotToolEntries(st.toolCtx),
		Loaded:           snapshotLoaded(st.loaded),
		PendingCalls:     st.resp.ToolCalls,
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		CachedTokens:     st.cachedTokens,
		TotalTokens:      st.totalTokens,
		Images:           st.images,
		CreatedAt:        time.Now(),
		WorkingDir:       task.WorkingDir,
	}
	if err := r.checkpoints.Save(cp); err != nil {
		return false, fmt.Errorf("save checkpoint for task %s: %w", task.ID, err)
	}
	return true, nil
}

// finishRun emits completion events/audit, deletes any checkpoint (the task is
// done, not suspended), and returns the assembled TaskRun.
func (r *Runtime) finishRun(ctx context.Context, requestID string, agent domain.Agent, task domain.Task, st loopState) (domain.TaskRun, error) {
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:      "inference_completed",
		TaskID:    task.ID,
		Message:   "model inference completed",
		CreatedAt: time.Now(),
	}); err != nil {
		return domain.TaskRun{}, fmt.Errorf("publish inference completed event: %w", err)
	}
	if err := r.audit.Append(ctx, domain.AuditEvent{
		ID:          task.ID + ":model-audit-1",
		RequestID:   requestID,
		SubjectType: "model",
		SubjectID:   task.ID,
		Action:      "model_inference_completed",
		Hash:        "memory",
		CreatedAt:   time.Now(),
	}); err != nil {
		return domain.TaskRun{}, fmt.Errorf("append model audit event: %w", err)
	}
	ended := time.Now()
	run := domain.TaskRun{
		ID:               task.ID + ":run-1",
		TaskID:           task.ID,
		AgentID:          agent.ID,
		StartedAt:        st.started,
		EndedAt:          ended,
		Result:           st.resp.Text,
		ReasoningSummary: st.resp.ReasoningSummary,
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		CachedTokens:     st.cachedTokens,
		TotalTokens:      st.totalTokens,
	}
	if err := r.audit.Append(ctx, domain.AuditEvent{
		ID:          task.ID + ":audit-1",
		RequestID:   requestID,
		SubjectType: "task",
		SubjectID:   task.ID,
		Action:      "task_completed",
		Hash:        "memory",
		CreatedAt:   time.Now(),
	}); err != nil {
		return domain.TaskRun{}, fmt.Errorf("append audit event: %w", err)
	}
	if err := r.events.Publish(ctx, domain.RuntimeEvent{
		Type:             "task_completed",
		TaskID:           task.ID,
		Message:          st.resp.Text,
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		CachedTokens:     st.cachedTokens,
		TotalTokens:      st.totalTokens,
		ElapsedMs:        ended.Sub(st.started).Milliseconds(),
		CreatedAt:        time.Now(),
	}); err != nil {
		return domain.TaskRun{}, fmt.Errorf("publish task completed event: %w", err)
	}
	if task.Mode == domain.ModePlan && r.checkpoints != nil {
		if err := r.writePlanArtifact(task, st.resp.Text); err != nil {
			return domain.TaskRun{}, fmt.Errorf("write plan artifact for task %s: %w", task.ID, err)
		}
	}
	if r.checkpoints != nil {
		if err := r.checkpoints.Delete(sessionKeyForTask(task), task.WorkingDir); err != nil {
			return domain.TaskRun{}, fmt.Errorf("delete checkpoint after completion for task %s: %w", task.ID, err)
		}
	}
	if err := r.publishLearning(ctx, agent, task, evolution.SignalSuccess, "", false); err != nil {
		return domain.TaskRun{}, fmt.Errorf("publish learning success event: %w", err)
	}
	return run, nil
}

// writePlanArtifact frames the model's plan result as OKF markdown (YAML
// frontmatter with type: Plan plus title/description/tags/timestamp, then the
// body) and writes it to the session's plans/ directory. Design §4.2.
func (r *Runtime) writePlanArtifact(task domain.Task, result string) error {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	title := firstNonEmptyLine(result)
	if title == "" {
		title = "Plan for task " + task.ID
	}
	content := fmt.Sprintf(`---
type: Plan
title: %q
description: "Plan produced in Plan mode for task %s"
tags: [plan, agent]
timestamp: %q
resource: %q
---

%s
`, title, task.ID, ts, task.ID, result)
	filename := fmt.Sprintf("plan-%d.md", now.UnixNano())
	if _, err := r.checkpoints.WritePlan(sessionKeyForTask(task), task.WorkingDir, filename, content); err != nil {
		return err
	}
	return nil
}

// firstNonEmptyLine returns the first non-empty (trimmed) line of s, used as a
// readable plan title.
func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func (r *Runtime) generate(ctx context.Context, requestID string, prompt string, images []string, stablePrefixLen int, tools *tool.Registry) (port.InferenceResponse, error) {
	return r.maas.Generate(ctx, port.InferenceRequest{
		RequestID:       requestID,
		Prompt:          prompt,
		Tools:           r.inferenceTools(tools),
		Images:          images,
		StablePrefixLen: stablePrefixLen,
	})
}

// generateNoTools runs a final inference with no tools offered, so the model is
// forced to produce a textual answer instead of requesting more tool calls. It
// is used to gracefully finish a task that has exhausted its tool-round budget.
func (r *Runtime) generateNoTools(ctx context.Context, requestID string, prompt string, images []string, stablePrefixLen int) (port.InferenceResponse, error) {
	return r.maas.Generate(ctx, port.InferenceRequest{
		RequestID:       requestID,
		Prompt:          prompt,
		Tools:           nil,
		Images:          images,
		StablePrefixLen: stablePrefixLen,
	})
}

// stablePrefixRunes reports the rune length of base when base is a verbatim
// prefix of sent, and 0 otherwise. Tool-round bounding (boundPrompt) can trim
// the head once the accumulated prompt exceeds the char budget; when that
// happens base is no longer a stable prefix and callers must not claim a cache
// breakpoint (contract: 0 means "no known stable prefix").
func stablePrefixRunes(sent, base string) int {
	if strings.HasPrefix(sent, base) {
		return len([]rune(base))
	}
	return 0
}

func (r *Runtime) inferenceTools(tools *tool.Registry) []port.InferenceTool {
	if tools == nil {
		return nil
	}
	// Lazy (on-demand) protocol: offer only the two meta tools so the model pays
	// a tiny fixed schema cost per inference. It loads a real tool's schema via
	// load_capabilities and invokes it via call_tool, both handled in-runtime.
	if r.lazyTools {
		return metaInferenceTools()
	}
	descriptors := tools.Descriptors()
	out := make([]port.InferenceTool, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, port.InferenceTool{
			Name:        descriptor.Name,
			Description: descriptor.Description,
			InputSchema: descriptor.InputSchema,
		})
	}
	return out
}

// executeToolCalls runs the current round's tool calls and returns their
// results. It takes the mutable *loopState so a dispatched load_capabilities can
// pin definitions into st.loaded and the caller sees the write when it composes
// the next round's prompt; it reads the pending calls, effective registry and
// catalog off st for the same reason.
func (r *Runtime) executeToolCalls(ctx context.Context, agent domain.Agent, task domain.Task, st *loopState) ([]domain.ToolResult, error) {
	if st.tools == nil {
		return nil, fmt.Errorf("tool registry unavailable")
	}
	calls := st.resp.ToolCalls
	results := make([]domain.ToolResult, 0, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			call.ID = task.ID + ":" + call.Name
		}
		if err := r.events.Publish(ctx, domain.RuntimeEvent{
			Type:      "tool_call_requested",
			TaskID:    task.ID,
			Message:   call.Name,
			CreatedAt: time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("publish tool request event: %w", err)
		}
		result, err := r.dispatchToolCall(ctx, agent, task, call, st)
		if err != nil {
			if pubErr := r.events.Publish(ctx, domain.RuntimeEvent{
				Type:      "tool_failed",
				TaskID:    task.ID,
				Message:   call.Name,
				CreatedAt: time.Now(),
			}); pubErr != nil {
				return nil, fmt.Errorf("publish tool failed event: %w", pubErr)
			}
			// Feed the tool error back to the model instead of failing the task.
			// The error is already surfaced via the tool_failed event and is
			// rendered into the next prompt by promptWithToolResults, so the
			// model can recover or answer directly on the following round.
			results = append(results, domain.ToolResult{CallID: call.ID, Success: false, Error: err.Error()})
			continue
		}
		results = append(results, result)
		if err := r.events.Publish(ctx, domain.RuntimeEvent{
			Type:      "tool_result",
			TaskID:    task.ID,
			Message:   result.Output,
			CreatedAt: time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("publish tool result event: %w", err)
		}
		if err := r.events.Publish(ctx, domain.RuntimeEvent{
			Type:      "tool_executed",
			TaskID:    task.ID,
			Message:   call.Name,
			CreatedAt: time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("publish tool executed event: %w", err)
		}
	}
	return results, nil
}

// toolEntry is one tool result kept in the accumulated tool context, tagged with
// a dedup key so repeated calls collapse to a single most-recent copy.
type toolEntry struct {
	key  string
	text string
}

// dedupKey identifies a tool call by its name and arguments, so two reads of the
// same file (or two fetches of the same URL) share a key and deduplicate.
func dedupKey(call domain.ToolCall) string {
	keys := make([]string, 0, len(call.Arguments))
	for k := range call.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(call.Name)
	for _, k := range keys {
		b.WriteString("|")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(call.Arguments[k])
	}
	return b.String()
}

func renderToolResult(result domain.ToolResult, maxResultChars int) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(result.CallID)
	if result.Success {
		b.WriteString(" success: ")
		b.WriteString(truncateText(result.Output, maxResultChars))
	} else {
		b.WriteString(" failed: ")
		b.WriteString(truncateText(result.Error, maxResultChars))
	}
	return b.String()
}

// mergeToolResults folds this round's results into the accumulated tool context,
// deduplicated by (tool, arguments): a repeated call replaces its earlier entry
// and moves it to the end (most-recent-wins), so duplicate large outputs do not
// pile up across rounds.
func mergeToolResults(entries []toolEntry, calls []domain.ToolCall, results []domain.ToolResult, maxResultChars int) []toolEntry {
	byID := make(map[string]domain.ToolResult, len(results))
	for _, res := range results {
		byID[res.CallID] = res
	}
	for _, call := range calls {
		res, ok := byID[call.ID]
		if !ok {
			continue
		}
		key := dedupKey(call)
		kept := make([]toolEntry, 0, len(entries)+1)
		for _, e := range entries {
			if e.key != key {
				kept = append(kept, e)
			}
		}
		entries = append(kept, toolEntry{key: key, text: renderToolResult(res, maxResultChars)})
	}
	return entries
}

func renderToolEntries(entries []toolEntry) string {
	var b strings.Builder
	b.WriteString("\n\nTool results:\n")
	for _, e := range entries {
		b.WriteString(e.text)
		b.WriteString("\n")
	}
	b.WriteString("\nUse the tool results above to answer the original user request directly.")
	return b.String()
}

// truncateText caps a single piece of text to maxChars runes, appending a marker
// noting how much was dropped. maxChars <= 0 disables truncation.
func truncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + fmt.Sprintf("\n…[truncated %d chars]", len(runes)-maxChars)
}

// boundPrompt caps the whole accumulated tool-loop prompt to maxChars by keeping
// the head (original task framing) and the most recent tail (latest tool
// results), collapsing the older middle. This prevents the prompt from growing
// unbounded across tool rounds without an extra LLM summarization call in the
// hot loop. maxChars <= 0 disables bounding.
func boundPrompt(prompt string, maxChars int) string {
	if maxChars <= 0 {
		return prompt
	}
	runes := []rune(prompt)
	if len(runes) <= maxChars {
		return prompt
	}
	headLen := maxChars / 3
	tailLen := maxChars - headLen
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	dropped := len(runes) - headLen - tailLen
	return head + fmt.Sprintf("\n\n…[older tool context trimmed: %d chars]…\n\n", dropped) + tail
}

type noopEventBus struct{}

func (noopEventBus) Publish(ctx context.Context, _ domain.RuntimeEvent) error {
	return ctx.Err()
}

func (noopEventBus) Events() ([]domain.RuntimeEvent, error) {
	return nil, nil
}

type noopAuditLog struct{}

func (noopAuditLog) Append(ctx context.Context, _ domain.AuditEvent) error {
	return ctx.Err()
}

func (noopAuditLog) Events() ([]domain.AuditEvent, error) {
	return nil, nil
}

func (r *Runtime) buildPrompt(ctx context.Context, agent domain.Agent, task domain.Task, catalog *capability.Catalog) (string, error) {
	toolNames := r.toolNames()
	if r.contextBuilder != nil {
		built, err := r.contextBuilder.BuildContext(ctx, cognitive.Request{
			Agent:             agent,
			Task:              task,
			ConversationTurns: append([]domain.ConversationTurn(nil), r.conversationTurns...),
			Tools:             toolNames,
			// Per-task, effective-registry-scoped catalog; nil under the eager
			// protocol so the Core renders no <available_capabilities> block.
			Catalog: catalog,
		})
		if err != nil {
			return "", fmt.Errorf("build cognitive context: %w", err)
		}
		// The hint is only needed on the Core path: Core renders a "Tools:" line
		// that, when empty under the lazy protocol, can mislead the model into
		// believing no tools exist. The plain paths below carry no such line.
		return built.Prompt + r.lazyToolHint(toolNames), nil
	}
	if r.contextPrefix != "" {
		return r.contextPrefix + "\n\nTask input:\n" + task.Input, nil
	}
	return task.Input, nil
}

// toolNames lists the registered real tool names (excluding the lazy-protocol
// meta tools), so the prompt can tell the model which tools exist even when the
// full schemas are not offered up front.
func (r *Runtime) toolNames() []string {
	if r.tools == nil {
		return nil
	}
	var names []string
	for _, descriptor := range r.tools.Descriptors() {
		if isMetaTool(descriptor.Name) {
			continue
		}
		names = append(names, descriptor.Name)
	}
	return names
}

// lazyToolHint returns a short instruction, only under the lazy protocol, telling
// the model that the named tools are available on demand via call_tool. Without
// it the model can see an empty native tool list and wrongly conclude no tools
// exist instead of discovering them.
func (r *Runtime) lazyToolHint(names []string) string {
	if !r.lazyTools || len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"\n\nAvailable tools (provided on demand, NOT empty): %s.\n"+
			"To use any tool, call call_tool with its tool_name and an arguments_json string; "+
			"call load_capabilities first if you need a tool's exact parameters. "+
			"Never claim no tools are available — they are listed above and loaded on demand via load_capabilities.\n",
		strings.Join(names, ", "),
	)
}

// recordLearningFailure publishes a failure learning signal and reports a
// publish failure instead of dropping it.
//
// The callers are all on their way out with a more important error already in
// hand, so this cannot return: the signal is a side record, not the result. But
// losing it silently is not free either — these signals feed the evolution
// pipeline and the trust score, so a bus that quietly rejects them leaves a
// persistently failing agent's score intact and TrustGate still admitting it.
// Warn is the level: the task's own failure is reported through its own channel;
// what is degraded here is the learning record.
func (r *Runtime) recordLearningFailure(ctx context.Context, agent domain.Agent, task domain.Task, reason string) {
	if err := r.publishLearning(ctx, agent, task, evolution.SignalFailure, reason, true); err != nil {
		r.logger.WarnContext(ctx, "publish failure learning event",
			"component", "runtime",
			"task_id", task.ID,
			"agent_id", agent.ID,
			"reason", reason,
			"error", err)
	}
}

func (r *Runtime) publishLearning(ctx context.Context, agent domain.Agent, task domain.Task, signal evolution.SignalKind, reason string, lightweight bool) error {
	return r.events.Publish(ctx, evolution.NewLearningRuntimeEvent(evolution.LearningEvent{
		AgentID:       agent.ID,
		TaskID:        task.ID,
		Signal:        signal,
		Reason:        reason,
		IsLightweight: lightweight,
		PublishedAt:   time.Now(),
	}))
}
