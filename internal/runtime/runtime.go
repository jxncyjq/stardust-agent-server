package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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
	"github.com/stardust/legion-agent/internal/taskgate"
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

// EpisodeRecorder captures a one-line-of-effort summary of a finished task into
// episodic memory. It is best-effort and MUST NOT block or fail the task: the
// implementation does its own async work + timeout + logging, so RecordEpisode
// returns immediately and never errors. A nil recorder disables episode
// recording entirely (valid, zero-behaviour-change default).
type EpisodeRecorder interface {
	RecordEpisode(agent domain.Agent, task domain.Task, outcome string, content string)
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
	// ToolRoot is the tool sandbox root for THIS run (task.WorkingDir, or the
	// context root fallback). When non-empty, an oversized tool result is cached
	// to ToolRoot/.stardust/tool_results/ so read_file can page it back; empty
	// means no sandbox (tests / no workspace) and results fall back to plain
	// self-describing truncation. Every production NewRuntime is built per-task,
	// so a fixed field is correct.
	ToolRoot          string
	ConversationTurns []domain.ConversationTurn
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
	// DisabledTools names tools this runtime's agent may not use (deny-list).
	// effectiveTools removes them from the registry that drives the offered
	// schema, the lazy capability catalog and dispatch at once. Meta-tools are
	// never in the registry, so they are unaffected. Empty disables nothing.
	DisabledTools []string
	// Debug enables the inference debug probe: runInference logs a per-message
	// breakdown of every outgoing prompt (see inferenceRequestDebug). Off by
	// default; intended to be driven by the config file's runtime.debug toggle.
	Debug bool
	// CompactTokenThreshold triggers conversation compaction when the tool
	// loop's accumulated prompt tokens exceed it. 0 disables compaction
	// (default), matching config.RuntimeConfig.CompactTokenThreshold.
	CompactTokenThreshold int
	// EpisodeRecorder is an optional hook invoked once per finished task (success
	// in finishRun, failure in recordLearningFailure) with a one-line-of-effort
	// summary for episodic memory. Nil disables it; the runtime never depends on
	// its outcome.
	EpisodeRecorder EpisodeRecorder
	// Gate is the task-boundary gate this runtime registers every RunTask on, so
	// a plugin change lands only between tasks and never underneath one. It is
	// REQUIRED: NewRuntime panics on a nil Gate rather than running ungated.
	//
	// The reason it is not optional is the shape of the mistake it would allow.
	// The gate is the whole of that guarantee, so a "nil means no gating"
	// default would make the guarantee vanish silently whenever a new
	// construction site forgot the field — the protection would be off and
	// nothing would say so. Every runtime that can carry a task in a process
	// where plugins are applied must share ONE gate with the loader; a gate of
	// its own would let its tasks run straight through another gate's boundary.
	Gate *taskgate.TaskGate
	// SessionEvents 是会话事件日志的落点（P1 的 port.SessionEventStore）。
	//
	// 允许为 nil，且 nil 是一种**契约声明的合法部署形态**（内存后端、绝大多数测试
	// 构造），不是兜底：那时整个记录是 no-op，三个屏障永远放行。它与「配了但写不进去」
	// 是两回事——后者由屏障 fail-closed 挡住。
	SessionEvents port.SessionEventStore
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
	maas                  port.MaasInferenceClient
	audit                 port.AuditLog
	events                port.EventBus
	contextBuilder        ContextBuilder
	contextPrefix         string
	tools                 *tool.Registry
	maxToolRounds         int
	maxToolResultChars    int
	maxPromptChars        int
	toolRoot              string
	lazyTools             bool
	conversationTurns     []domain.ConversationTurn
	interrupted           atomic.Bool
	role                  string
	depth                 int
	maxSpawnDepth         int
	maxConcurrent         int
	subTaskSeq            atomic.Uint64
	checkpoints           *sessionstate.Store
	toolGate              ToolGate
	logger                *slog.Logger
	skillUsage            SkillUsageRecorder
	capabilitySkills      capability.Provider
	disabledTools         []string
	debug                 bool
	compactTokenThreshold int
	episodeRecorder       EpisodeRecorder
	// gate is never nil: NewRuntime refuses a nil Config.Gate and newSubRuntime
	// carries the parent's over, so RunTask can register on it unconditionally.
	gate *taskgate.TaskGate
	// sessionEvents is the session event log's store (Config.SessionEvents).
	// Nil is a contract-optional deployment shape, not a wiring gap: RunTask
	// always builds an *eventRecorder from it (newTaskRecorder), and a nil
	// store makes that recorder a no-op (eventRecorder.enabled()) rather than
	// leaving the recorder field itself nil -- see eventRecorder's type doc on
	// why a literal nil recorder is refused, not tolerated.
	sessionEvents port.SessionEventStore
}

// loopState is the mutable state threaded through the tool-execution loop.
// runToolLoop advances it; a suspend serialises the relevant fields to a
// checkpoint and a resume rebuilds it from one.
type loopState struct {
	started    time.Time
	basePrompt string
	round      int
	// convo is the append-only multi-turn exchange sent to the model each round
	// (see messages.go). It replaced a single re-sent prompt string whose tool
	// results were deduplicated by (name, arguments), which hid the model's own
	// repeated calls from it.
	convo *conversation
	// repeatGuard counts non-consecutive repeats of each tool-call signature
	// across the whole task (see messages.go). One per RunTask.
	repeatGuard *repeatGuard
	// toolNameGuard counts calls per tool NAME (ignoring arguments) across the
	// task, backing the toolLoopCap runaway guard that the name+arguments
	// repeatGuard cannot see. toolFailGuard counts per-name FAILURES for the
	// same-tool-failure warning. Both one per RunTask, like repeatGuard.
	//
	// toolNameGuard is a sharedToolBudget rather than a bare repeatGuard because
	// the model is not its only writer: dispatchToolCall installs it on every
	// dispatched call's context as a tool.LoopBudget, so a plugin's call_tool
	// spends this same allowance instead of a counter of its own.
	toolNameGuard    *sharedToolBudget
	toolFailGuard    *repeatGuard
	loaded           []loadedEntry
	resp             port.InferenceResponse
	promptTokens     int
	completionTokens int
	cachedTokens     int
	totalTokens      int
	// compactions counts how many times this task's conversation has been
	// summarised by compactConversation, capped at maxCompactionsPerTask so a
	// pathological run cannot spend its whole budget re-summarising every round.
	compactions int
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
	// generatedFiles collects workspace-relative paths of files this task wrote
	// via write_file, in first-seen order with duplicates removed. Populated in
	// executeToolCalls's success branch, surfaced by finishRun onto both the
	// task_completed RuntimeEvent and the returned TaskRun.
	generatedFiles []string
	// events is this run's session event recorder (spec §5), built once in
	// RunTask (newTaskRecorder) and carried through the whole loop so
	// executeToolCalls/dispatchToolCall can record tool/call and tool/result
	// without their own signatures growing a recorder parameter. Never nil
	// (see eventRecorder's type doc): a deployment with no SessionEvents store
	// still gets a recorder, just one whose enabled() is false.
	events *eventRecorder
}

// effectiveTools returns the tool registry a run should use: in Plan mode only
// the non-sensitive (read-only) subset, so a planning run can research but never
// cause side effects; every other mode uses the full registry unchanged. It never
// mutates r.tools and returns a fresh per-call Registry, safe under concurrent
// tasks sharing this Runtime.
func (r *Runtime) effectiveTools(task domain.Task) *tool.Registry {
	tools := r.tools
	if tools != nil && task.Mode == domain.ModePlan {
		tools = tools.Subset(tools.SafeToolNames()...)
	}
	if tools != nil && len(r.disabledTools) > 0 {
		tools = tools.Without(r.disabledTools...)
	}
	return tools
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
	// A nil gate is a wiring error, not a request to run ungated — see
	// Config.Gate. NewRuntime has no error return and every caller is assembly
	// code, so the loud failure is a panic naming the field.
	if cfg.Gate == nil {
		panic("runtime: NewRuntime: Config.Gate is nil; a runtime without a task-boundary gate " +
			"would let a plugin change land in the middle of a running task")
	}
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
		maas:                  cfg.Maas,
		audit:                 audit,
		events:                events,
		contextBuilder:        cfg.ContextBuilder,
		contextPrefix:         strings.TrimSpace(cfg.ContextPrefix),
		tools:                 cfg.Tools,
		maxToolRounds:         normalizeMaxToolRounds(cfg.MaxToolRounds),
		maxToolResultChars:    normalizePositive(cfg.MaxToolResultChars, defaultMaxToolResultChars),
		maxPromptChars:        normalizePositive(cfg.MaxPromptChars, defaultMaxPromptChars),
		toolRoot:              cfg.ToolRoot,
		lazyTools:             cfg.LazyTools,
		conversationTurns:     append([]domain.ConversationTurn(nil), cfg.ConversationTurns...),
		role:                  role,
		depth:                 cfg.Depth,
		maxSpawnDepth:         normalizePositive(cfg.MaxSpawnDepth, defaultMaxSpawnDepth),
		maxConcurrent:         normalizePositive(cfg.MaxConcurrent, defaultMaxConcurrent),
		checkpoints:           cfg.Checkpoints,
		toolGate:              cfg.ToolGate,
		logger:                logger,
		debug:                 cfg.Debug,
		skillUsage:            cfg.SkillUsage,
		capabilitySkills:      cfg.CapabilitySkills,
		disabledTools:         cfg.DisabledTools,
		compactTokenThreshold: cfg.CompactTokenThreshold,
		episodeRecorder:       cfg.EpisodeRecorder,
		gate:                  cfg.Gate,
		sessionEvents:         cfg.SessionEvents,
	}
}

// recordEpisode is the nil-safe entry point into the optional EpisodeRecorder
// hook. A nil recorder (the default) makes this a no-op, so callers never need
// to guard the call themselves.
func (r *Runtime) recordEpisode(agent domain.Agent, task domain.Task, outcome string, content string) {
	if r.episodeRecorder != nil {
		r.episodeRecorder.RecordEpisode(agent, task, outcome, content)
	}
}

// normalizeMaxToolRounds is the runtime-layer fallback for a directly
// constructed Runtime: an unset (<=0) MaxToolRounds falls back to
// defaultMaxToolRounds.
//
// This intentionally differs from config.normalizeMaxToolRounds, which maps
// <=0 to config.UnlimitedToolRoundsCap as the user-facing "0 = no limit"
// opt-in. Production always runs config.Load first, so the value reaching
// NewRuntime is already positive and never takes this <=0 branch; this fallback
// only covers direct construction (tests, and app.RunDemo's bare Config).
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

// newTaskRecorder builds this run's session event recorder (spec §5) and
// resolves the turn number it opens with.
//
// Turn numbers are monotonic PER SESSION (spec §4.1): the resolved value is
// one past the highest turn already recorded among this session's existing
// events, or 0 when it has none yet. That covers r.sessionEvents == nil too —
// the contract-optional deployment shape (see Config.SessionEvents) — under
// which newEventRecorder itself returns a disabled (store == nil) recorder
// and the turn number is never observed by anything: enabled() is false, so
// every record* call becomes a no-op. Skipping the ReadFrom in that case is
// not an optimisation shortcut, it is required — there is no store to read.
//
// Two known costs, both deliberate and both registered for Task 5 rather than
// papered over here:
//
//   - This ReadFrom scans the whole session log, and the recorder's first flush
//     scans it again to align its seq cursor: two O(n) reads per turn on a log
//     that grows with the session. Seeding the recorder's cursor from THIS read
//     would remove the second one, but it would also move the cursor's snapshot
//     from "the moment we first write" to "the moment the task started",
//     widening the window in which another writer on the same session can land
//     an event and turn our whole batch into a hard Append failure. A slow read
//     is a better trade than a task that fails because someone else wrote
//     first; making it cheap needs a store-side next-turn query, which is P1's
//     surface, not this function's.
//   - Two tasks running concurrently on ONE session resolve the same turn
//     number here (sessionKeyForTask does not serialise them). Nothing in P2
//     prevents it; whether the scheduler can produce it is a Task 5 question.
//     The consequence is two turns sharing a number, not lost or interleaved
//     events — the store still assigns seq under its own per-session lock.
func (r *Runtime) newTaskRecorder(ctx context.Context, task domain.Task) (*eventRecorder, int, error) {
	rec := newEventRecorder(r.sessionEvents, task)
	if r.sessionEvents == nil {
		return rec, 0, nil
	}
	existing, err := r.sessionEvents.ReadFrom(ctx, rec.sessionID(), 0)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve turn number for session %q: %w", rec.sessionID(), err)
	}
	turn := 0
	for _, e := range existing {
		var payload struct {
			Turn int `json:"turn"`
		}
		if err := json.Unmarshal(e.Data, &payload); err != nil {
			return nil, 0, fmt.Errorf("decode existing session event seq %d for %q: %w", e.Seq, rec.sessionID(), err)
		}
		if payload.Turn >= turn {
			turn = payload.Turn + 1
		}
	}
	return rec, turn, nil
}

// closingTurnReason maps a RunTask-ending error to the turn/end reason that
// best describes it: a caller-cancelled context closes as "cancelled",
// anything else as "failed". Never "interrupted" -- that reason is reserved
// for crash recovery reconstructing a turn nobody closed, which is a
// different code path entirely (not exercised by P2); a controlled error
// return here always gets to write its own closing event.
func closingTurnReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.TurnEndReasonCancelled
	}
	return domain.TurnEndReasonFailed
}

// closingStepReason is closingTurnReason's step/end counterpart, used wherever
// a step is closed on the same error that is about to close its turn, so the
// two closing events agree on why (both "cancelled" for a caller-cancelled
// context, both "failed" otherwise).
func closingStepReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.StepEndReasonCancelled
	}
	return domain.StepEndReasonFailed
}

// closeTurnOnError records this run's closing turn/end (see closingTurnReason)
// and makes a best-effort attempt to flush it, for use on an error-return path
// where the caller already has the real error to report. A flush failure here
// is logged rather than returned: it must never replace or mask the primary
// error, and the buffered events survive a failed flush (eventRecorder.flush)
// so nothing is lost, only left for a later attempt that this run will not make.
func (r *Runtime) closeTurnOnError(ctx context.Context, task domain.Task, rec *eventRecorder, cause error) {
	rec.recordTurnEnd(closingTurnReason(cause))
	if err := rec.flush(ctx); err != nil {
		r.logger.Warn("session event flush failed while closing a failed run",
			"task_id", task.ID, "cause", cause, "flush_error", err)
	}
}

// RunTask runs one task from prompt to result, including the whole tool loop,
// and returns what it produced.
//
// It holds the runtime's task-boundary gate for the entire run, so a plugin
// change waits for this task to finish instead of changing the capability
// catalog underneath it. When a plugin change is already waiting for a boundary
// a TOP-LEVEL task does not start at all and the returned error wraps
// ErrApplyPending — a caller that matches it with errors.Is can retry shortly,
// which is a different response from the one a task that genuinely failed
// deserves. A delegated sub-task (Depth above 0) is never refused this way: it
// continues a task that is already in flight, which the pending apply is
// already waiting for, so refusing it would break the very task the gate is
// holding the apply for.
//
// The guarantee is one catalog per CONTINUOUS execution, not per task id. A
// manual-mode task that suspends returns ErrSuspended, which retires the gate
// like any other ending, so an apply may land while it waits for its human and
// the resumed leg (a fresh RunTask over the checkpointed conversation) may see
// a changed catalog. That is accepted deliberately: a suspended task can sit
// for hours, and counting it as in flight would let one unanswered approval
// block every plugin reload indefinitely — a worse failure than one
// prompt-cache miss on resume. See TaskGate's doc for the full reasoning.
func (r *Runtime) RunTask(ctx context.Context, agent domain.Agent, task domain.Task) (domain.TaskRun, error) {
	// The task is registered with the task-boundary gate before anything else
	// happens, and retired when it is over however it ends. That is both halves
	// of the contract in one place: while this task runs a plugin change waits
	// for it instead of landing underneath it, and a task that would start into
	// a plugin set that is mid-change does not start at all.
	//
	// Depth is what tells the two apart. A runtime at depth 0 is where a task
	// ARRIVES, so it is the one that can be turned away; every deeper runtime is
	// built by newSubRuntime for a child of a task whose Begin is still held, so
	// it registers with BeginChild and is admitted unconditionally. (Depth above
	// 0 is only ever reached through that delegation path — no production
	// construction site sets Config.Depth.)
	//
	// The refusal is wrapped, not flattened: it carries ErrApplyPending, so a
	// caller can tell "the plugin set is switching, retry in a moment" from
	// "this task failed". Returning here costs nothing to unwind — no event has
	// been published, no inference issued — and it is deliberately NOT recorded
	// as a learning failure: the task did not fail, it did not run.
	var end func()
	if r.depth > 0 {
		end = r.gate.BeginChild()
	} else {
		gateEnd, err := r.gate.Begin()
		if err != nil {
			return domain.TaskRun{}, fmt.Errorf("run task %s: %w", task.ID, err)
		}
		end = gateEnd
	}
	defer end()

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

	// One RunTask execution is one turn (spec §4.1): the recorder and the turn
	// number are resolved before anything that could produce an event, and
	// EVERY control-flow path out of this function from here on -- resume,
	// fresh run, any of their failure branches -- shares this one rec so the
	// turn it opens here gets exactly one closing turn/end. Building it any
	// earlier would record turns for tasks that never actually ran (refused by
	// the gate, interrupted, no maas configured, missing manual gate) -- those
	// returns above cost nothing to unwind and are deliberately not turns.
	rec, turn, err := r.newTaskRecorder(ctx, task)
	if err != nil {
		return domain.TaskRun{}, fmt.Errorf("run task %s: %w", task.ID, err)
	}
	rec.recordTurnStart(turn)
	rec.recordUserMessage(task.Input)

	// Resume path: a persisted checkpoint means this task previously suspended.
	// Rebuild loop state from disk and re-enter the loop with the pending calls,
	// skipping the initial prompt build + generate.
	if r.checkpoints != nil {
		cp, ok, err := r.checkpoints.Load(sessionKeyForTask(task), task.WorkingDir)
		if err != nil {
			r.closeTurnOnError(ctx, task, rec, err)
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
				convo:            restoreConversation(cp.Messages),
				loaded:           restoreLoaded(cp.Loaded),
				resp:             port.InferenceResponse{ToolCalls: cp.PendingCalls},
				promptTokens:     cp.PromptTokens,
				completionTokens: cp.CompletionTokens,
				cachedTokens:     cp.CachedTokens,
				totalTokens:      cp.TotalTokens,
				images:           cp.Images,
				tools:            effTools,
				repeatGuard:      newRepeatGuard(),
				toolNameGuard:    newSharedToolBudget(),
				toolFailGuard:    newRepeatGuard(),
				// The resumed prompt's catalog is already baked into cp.BasePrompt
				// from the first run; this rebuilds the dispatch-side catalog so a
				// load_capabilities issued in a resumed round still resolves, scoped
				// to the same effective registry.
				catalog: r.buildCatalog(effTools),
				events:  rec,
			}
			// The carried-over pending calls are this NEW turn's opening step:
			// there is no fresh model request to wrap (the response came from
			// the checkpoint, not a generate call), so this step is recorded
			// directly rather than through generateStep. It still needs a
			// barrier before runToolLoop may dispatch any of these calls again
			// -- same "record before acting" guarantee barrier 2 gives a fresh
			// call, just applied to calls resumed from disk instead of ones
			// just generated.
			//
			// The response is therefore recorded twice across the log: once in
			// the turn that suspended (which closed as "cancelled", see the
			// suspend branch in runToolLoop) and once here, opening the turn
			// that acts on it. That is deliberate and not a duplicate-write
			// bug: each turn must be readable on its own, and a turn whose
			// first act is dispatching tool calls with no assistant message
			// above them would be unreadable. The user/message recorded at the
			// top of RunTask repeats for the same reason -- the resumed turn is
			// still answering it.
			rec.recordStepStart()
			rec.recordAssistantMessage(st.resp.Text, st.resp.ToolCalls, eventUsage{
				Prompt: st.promptTokens, Completion: st.completionTokens,
				Cached: st.cachedTokens, Total: st.totalTokens,
			}, "")
			if err := rec.barrier(ctx, "before resuming pending tool calls"); err != nil {
				r.closeTurnOnError(ctx, task, rec, err)
				return domain.TaskRun{}, fmt.Errorf("run task %s: %w", task.ID, err)
			}
			return r.runToolLoop(ctx, requestID, agent, task, st)
		}
	}

	effTools := r.effectiveTools(task)
	catalog := r.buildCatalog(effTools)
	prompt, stablePrefixLen, err := r.buildPrompt(ctx, agent, task, catalog)
	if err != nil {
		r.closeTurnOnError(ctx, task, rec, err)
		return domain.TaskRun{}, err
	}
	if task.Mode == domain.ModePlan {
		prompt += planInstruction
	}
	// basePrompt is the fixed task framing (system + task). It is reused verbatim
	// as the head of every tool-round prompt (message[0]); its leading stable
	// section drives the provider prompt-cache breakpoint via
	// pinCachePrefix -> message[0].StablePrefixLen (see stablePrefixLen above).
	basePrompt := prompt
	convo := newConversation(basePrompt, task.Images)
	convo.pinCachePrefix(stablePrefixLen)
	resp, err := r.generateStep(ctx, rec, requestID, task.ID, convo, effTools)
	if err != nil {
		r.closeTurnOnError(ctx, task, rec, err)
		r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
		return domain.TaskRun{}, fmt.Errorf("generate inference: %w", err)
	}
	st := loopState{
		started:          started,
		basePrompt:       basePrompt,
		convo:            convo,
		round:            0,
		resp:             resp,
		promptTokens:     resp.PromptTokens,
		completionTokens: resp.CompletionTokens,
		cachedTokens:     resp.CachedTokens,
		totalTokens:      resp.TotalTokens,
		images:           task.Images,
		tools:            effTools,
		repeatGuard:      newRepeatGuard(),
		toolNameGuard:    newSharedToolBudget(),
		toolFailGuard:    newRepeatGuard(),
		catalog:          catalog,
		events:           rec,
	}
	return r.runToolLoop(ctx, requestID, agent, task, st)
}

// runToolLoop advances the tool-execution loop from st until the model stops
// requesting tools (or the round budget is exhausted), then finalises the run.
// Before executing each round's tool calls it consults the ToolGate: if the gate
// says suspend, it writes a checkpoint and returns ErrSuspended, releasing the
// goroutine. A successfully completed run deletes any checkpoint.
func (r *Runtime) runToolLoop(ctx context.Context, requestID string, agent domain.Agent, task domain.Task, st loopState) (domain.TaskRun, error) {
	rec := st.events
	// Each round appends the model's own turn and one tool turn per executed
	// call, so the exchange the model sees grows monotonically and its repeated
	// calls stay visible to it.
	//
	// loopCut records that the loop ended because the model kept repeating one
	// call rather than because it ran out of rounds; the two need different
	// closing instructions.
	loopCut := false
	// stepOpen tracks whether a step is currently open -- a step/start with no
	// step/end yet. On entry one always is: whoever produced st.resp (RunTask's
	// initial generateStep, the resumed step, or an earlier iteration's
	// trailing generateStep) opened it and left the closing to this function.
	// The loop body clears it when it closes the round's step and each
	// successful generate*Step sets it again, so the closing points below can
	// tell "there is still an open step to close" from "the body already closed
	// it".
	//
	// Without it the two breaks below (per-tool cap, repeat-abort) fall into
	// the budget-exhausted branch with st.resp.ToolCalls still non-empty and
	// emit a second step/end for a step that was closed one line earlier: a
	// step/end with no matching step/start, and -- since recordStepEnd advances
	// the step counter -- the next step's number stolen too. Both guards are
	// live production paths (they are the two brakes added after the 152-round
	// run of 2026-07-23), so that imbalance is not a corner case.
	//
	// Paths that return immediately do not bother updating it; it only has to
	// be right where control flows onward.
	stepOpen := true
	for st.round < r.maxToolRounds && len(st.resp.ToolCalls) > 0 {
		suspend, err := r.checkSuspend(ctx, task, st)
		if err != nil {
			// st.resp's step was opened (step/start + assistant/message) by
			// whoever produced it -- RunTask's initial generateStep, the resumed
			// step, or the previous iteration's trailing generateStep -- and
			// never got the chance to execute its tool calls. Close it here
			// rather than leaving it dangling: this is a hard failure, not a
			// suspend, so nothing will resume this turn later.
			rec.recordStepEnd(closingStepReason(err))
			r.closeTurnOnError(ctx, task, rec, err)
			return domain.TaskRun{}, err
		}
		if suspend {
			// Suspension is a normal pause, not a failure: the checkpoint (just
			// written by checkSuspend) holds everything needed to resume.
			//
			// The step and the turn nevertheless close HERE, because nobody
			// else ever will: resuming opens a FRESH turn (newTaskRecorder
			// resolves the next turn number from what is already on disk) and
			// re-records the response it resumes from, so this turn is finished
			// as far as the log is concerned. Leaving it open would leave a
			// turn that no crash ever happened to, which a later Load would
			// reconstruct as "interrupted" -- the reason reserved for a process
			// that really did die mid-turn, and precisely the marker P3 is
			// promised never to see from a normal run.
			//
			// "cancelled" rather than "completed" or "failed": this turn
			// neither finished its work nor failed at it, it was stopped part
			// way pending a human decision. Of the four reasons spec §4.1
			// defines that is the only honest one.
			rec.recordStepEnd(domain.StepEndReasonCancelled)
			rec.recordTurnEnd(domain.TurnEndReasonCancelled)
			// Flushing is mandatory: everything buffered by this turn (its
			// step/start, assistant/message and the two closing events above)
			// dies with this *eventRecorder when RunTask returns. Fail loud --
			// an un-flushed turn silently vanishes forever.
			if err := rec.flush(ctx); err != nil {
				return domain.TaskRun{}, fmt.Errorf("flush session events before suspend for task %s: %w", task.ID, err)
			}
			return domain.TaskRun{}, ErrSuspended
		}
		calls := st.resp.ToolCalls
		// Counted before this round is recorded, so the streak is "how many
		// consecutive rounds have asked for exactly this, including now".
		streak := repeatedCallStreak(st.convo.messages, calls)
		// repeatCount counts every occurrence of this call signature in the task,
		// consecutive or not. Because repeatAbortCount (6) < repeatAbortStreak (8),
		// a purely consecutive repeat now trips this guard at 6 rather than the
		// streak guard at 8 — intended: six identical calls is enough to stop,
		// whether or not they were interleaved. The streak guard still owns the
		// earlier consecutive *warning* (repeatWarnStreak=3 < repeatWarnCount=4).
		repeatCount := st.repeatGuard.record(callsKey(calls))
		// P2: per-tool-name loop cap. repeatGuard/streak key on callsKey
		// (name+arguments) and so miss "same tool, different args" runaways;
		// this counts by tool NAME only. Recorded before executing this round so
		// the count reflects every call the model has made, including now.
		// Count the tool the model actually reached, not the call_tool wrapper
		// the lazy protocol reaches it through: see domain.GuardedToolName.
		// The count comes back from the SHARED budget (sharedToolBudget), so calls a
		// plugin made through call_tool during earlier rounds are already in it: the
		// model's remaining allowance is reduced by whatever a contributor spent.
		capHit := ""
		for _, c := range calls {
			guarded := domain.GuardedToolName(c)
			if count, limit := st.toolNameGuard.Record(guarded); count >= limit {
				capHit = guarded
			}
		}
		st.convo.appendAssistant(st.resp.Text, calls)
		results, err := r.executeToolCalls(ctx, agent, task, &st)
		if err != nil {
			// executeToolCalls records tool/call + tool/result around every
			// dispatch it reaches (including the one whose error aborted the
			// loop, via its own barrier/dispatch-error handling); what it never
			// gets to is closing THIS step, since the step only finishes once
			// every one of the round's calls has been accounted for.
			rec.recordStepEnd(closingStepReason(err))
			r.closeTurnOnError(ctx, task, rec, err)
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonToolError)
			return domain.TaskRun{}, fmt.Errorf("execute model tool calls: %w", err)
		}
		st.convo.appendToolResults(calls, results, r.maxToolResultChars, r.toolRoot, sessionCacheDir(task), r.logger)
		// P2: same-tool failure warning. Count failures by tool NAME (not
		// callsKey) so "same tool, different args" failing repeatedly is caught.
		// Warn only — the loop cap is the hard stop.
		nameByID := make(map[string]string, len(calls))
		for _, c := range calls {
			nameByID[c.ID] = domain.GuardedToolName(c)
		}
		for _, res := range results {
			if res.Success {
				continue
			}
			if st.toolFailGuard.record(nameByID[res.CallID]) == toolSameFailWarn {
				st.convo.appendUser(fmt.Sprintf(
					"[系统] 工具 %s 已累计失败 %d 次。不要再用不同参数反复重试它：检查最近的错误信息、验证假设，改用其他工具，或基于已有信息直接作答。",
					nameByID[res.CallID], toolSameFailWarn))
			}
		}
		// A load_capabilities in this round changed the pinned block; surface the
		// new definitions as their own turn rather than re-sending the whole
		// block every round.
		st.convo.syncLoaded(renderLoaded(st.loaded))

		// This round's step is done: its response was recorded when it was
		// generated (RunTask's initial generateStep for round 0, the manually
		// recorded resumed step, or the previous iteration's trailing
		// generateStep call below), and every one of its tool calls, if any,
		// was just dispatched above with its own tool/call+tool/result pair.
		// Close it now, before deciding whether the loop continues, breaks
		// (loopCut/capHit), or falls through to the budget-exhausted branch --
		// all three paths share this one closing point so the step is never
		// left open regardless of which one the guards below choose.
		rec.recordStepEnd(domain.StepEndReasonCompleted)
		stepOpen = false
		if err := rec.barrier(ctx, "before next step"); err != nil {
			r.closeTurnOnError(ctx, task, rec, err)
			return domain.TaskRun{}, err
		}
		if capHit != "" {
			if err := r.events.Publish(ctx, domain.RuntimeEvent{
				Type:      "tool_loop_broken",
				TaskID:    task.ID,
				Message:   fmt.Sprintf("工具 %s 调用次数达上限(%d)，已停止工具循环", capHit, toolLoopCap),
				CreatedAt: time.Now(),
			}); err != nil {
				// Like every other error return in this function: the turn was
				// opened here and nothing else will ever close it, so it closes
				// here. Leaving it open would hand P3 a turn that a later Load
				// reconstructs as "interrupted" -- the one reason reserved for a
				// process that really did die mid-turn.
				r.closeTurnOnError(ctx, task, rec, err)
				return domain.TaskRun{}, fmt.Errorf("publish tool loop cap event: %w", err)
			}
			r.logger.Warn("tool loop broken: per-tool call cap reached",
				"task_id", task.ID, "tool", capHit, "cap", toolLoopCap)
			loopCut = true
			break
		}
		if streak >= repeatAbortStreak || repeatCount >= repeatAbortCount {
			// The model is not making progress: it has asked for exactly the same
			// calls this many rounds running and the results are already in its
			// context. Cutting the loop here is the difference between a finished
			// task and the 152-round, 554s run of 2026-07-23. Say so loudly —
			// silently stopping would look to the user like the model simply chose
			// to answer.
			if err := r.events.Publish(ctx, domain.RuntimeEvent{
				Type:      "tool_loop_broken",
				TaskID:    task.ID,
				Message:   "同一工具调用重复过多，已停止工具循环",
				CreatedAt: time.Now(),
			}); err != nil {
				// Same as the cap branch above: close the turn this function
				// opened rather than leaving it to be reconstructed as
				// "interrupted".
				r.closeTurnOnError(ctx, task, rec, err)
				return domain.TaskRun{}, fmt.Errorf("publish tool loop broken event: %w", err)
			}
			r.logger.Warn("tool loop broken: identical call repeated",
				"task_id", task.ID, "streak", streak, "repeat_count", repeatCount, "calls", callsKey(calls))
			loopCut = true
			break
		}
		if streak >= repeatWarnStreak || repeatCount >= repeatWarnCount {
			st.convo.appendUser(fmt.Sprintf(
				"[系统] 你已多次以完全相同的参数调用同一工具，结果没有变化。不要再重复该调用：改用其他工具，或基于已获取的信息直接给出最终回答。（连续%d次/累计%d次）", streak, repeatCount))
		}
		st.resp, err = r.generateStep(ctx, rec, requestID, task.ID, st.convo, st.tools)
		if err != nil {
			// generateStep closes the step it opened on its own error path, so
			// there is nothing open left here -- only the turn to close.
			r.closeTurnOnError(ctx, task, rec, err)
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
			return domain.TaskRun{}, fmt.Errorf("generate inference after tools: %w", err)
		}
		stepOpen = true
		st.promptTokens += st.resp.PromptTokens
		st.completionTokens += st.resp.CompletionTokens
		st.cachedTokens += st.resp.CachedTokens
		st.totalTokens += st.resp.TotalTokens
		st.round++
		if r.compactTokenThreshold > 0 && st.promptTokens > r.compactTokenThreshold && st.compactions < maxCompactionsPerTask {
			compacted, err := r.compactConversation(ctx, st.convo)
			if err != nil {
				// Fail-loud but non-fatal: keep the un-compacted history and press on;
				// a failed summary must never abort a task or drop context.
				r.logger.Warn("conversation compaction failed",
					"task_id", task.ID, "err", err)
			} else if compacted {
				st.compactions++
			}
		}
	}
	if len(st.resp.ToolCalls) > 0 {
		// Two different situations reach here with pending calls, and only one
		// of them has a step to close.
		//
		// Round budget exhausted: st.resp came from the loop's last trailing
		// generateStep and its step is still open (stepOpen), but the loop
		// exited before any iteration could execute those calls. They are
		// deliberately abandoned -- never dispatched, so no orphaned tool/call:
		// spec §4.3.1 rule 1 only promises an answer for calls actually
		// recorded. That step closes as "cancelled" rather than "completed":
		// the tool loop stopped it, it did not finish.
		//
		// loopCut/capHit break: st.resp's calls WERE dispatched (they are the
		// round the body just executed, tool/call+tool/result and all) and the
		// body already closed that step as "completed". Nothing is open, so
		// nothing is closed here -- emitting a step/end anyway is exactly the
		// unmatched end this branch used to produce.
		if stepOpen {
			rec.recordStepEnd(domain.StepEndReasonCancelled)
			stepOpen = false
			if err := rec.barrier(ctx, "before next step"); err != nil {
				r.closeTurnOnError(ctx, task, rec, err)
				return domain.TaskRun{}, err
			}
		}
		// Rather than hard-failing the whole task (which discards every tool
		// result gathered so far and surfaces as "任务执行失败" to the user),
		// make a final inference with no tools offered, and explicitly instruct
		// the model to answer rather than narrate another tool call —
		// otherwise it tends to emit text like "list_files 参数: {...}" instead
		// of a real answer when it is cut off mid-exploration.
		closing := "[系统] 工具调用已达上限。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		if loopCut {
			closing = "[系统] 检测到你在重复同一个工具调用，已停止工具循环。请勿再调用、规划或描述任何工具调用，直接基于以上已获取的信息，用自然语言给出对用户问题的最终回答。"
		}
		st.convo.appendUser(closing)
		final, err := r.generateFinalStep(ctx, rec, requestID, task.ID, st.convo)
		if err != nil {
			// Like generateStep, it closes its own step on the error path.
			r.closeTurnOnError(ctx, task, rec, err)
			r.recordLearningFailure(ctx, agent, task, evolution.FailureReasonInferenceError)
			return domain.TaskRun{}, fmt.Errorf("generate final answer after tool budget exhausted: %w", err)
		}
		stepOpen = true
		st.promptTokens += final.PromptTokens
		st.completionTokens += final.CompletionTokens
		st.cachedTokens += final.CachedTokens
		st.totalTokens += final.TotalTokens
		st.resp = final
	}
	// Whatever step is currently open at this point -- the loop's normal-exit
	// trailing response (no more tool calls requested), the step this function
	// was entered with when the model asked for no tools at all, or the closing
	// generateFinalStep answer just above -- is this turn's last step. Close
	// it before the turn itself closes. One is always open here: every one of
	// those three paths sets stepOpen, and the only path that clears it without
	// opening another (the loop-cut break) is routed through generateFinalStep
	// by the branch above, which opens one again.
	rec.recordStepEnd(domain.StepEndReasonCompleted)
	if err := rec.barrier(ctx, "before next step"); err != nil {
		r.closeTurnOnError(ctx, task, rec, err)
		return domain.TaskRun{}, err
	}
	run, err := r.finishRun(ctx, requestID, agent, task, st)
	if err != nil {
		r.closeTurnOnError(ctx, task, rec, err)
		return domain.TaskRun{}, err
	}
	rec.recordTurnEnd(domain.TurnEndReasonCompleted)
	if err := rec.flush(ctx); err != nil {
		return domain.TaskRun{}, fmt.Errorf("flush final session events for task %s: %w", task.ID, err)
	}
	return run, nil
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
		Messages:         snapshotMessages(st.convo),
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
		ID:               task.ID + ":model-audit-1",
		RequestID:        requestID,
		SubjectType:      "model",
		SubjectID:        task.ID,
		Action:           "model_inference_completed",
		Hash:             "memory",
		PromptTokens:     st.promptTokens,
		CompletionTokens: st.completionTokens,
		CachedTokens:     st.cachedTokens,
		TotalTokens:      st.totalTokens,
		CreatedAt:        time.Now(),
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
		GeneratedFiles:   st.generatedFiles,
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
		GeneratedFiles:   st.generatedFiles,
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
	r.recordEpisode(agent, task, "success", st.resp.Text)
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

// generate sends the accumulated exchange and offers the run's effective tools.
// The request carries Messages (never Prompt): the model has to see the calls it
// already made. message[0] carries StablePrefixLen (set via pinCachePrefix), so
// the adapter places a prompt-cache breakpoint at the stable head of the task
// framing; the rest of the append-only exchange stays a stable prefix for
// providers that also cache automatically.
func (r *Runtime) generate(ctx context.Context, requestID string, taskID string, convo *conversation, tools *tool.Registry) (port.InferenceResponse, error) {
	req := port.InferenceRequest{
		RequestID: requestID,
		Messages:  convo.render(r.maxPromptChars),
		Tools:     r.inferenceTools(tools),
	}
	return r.runInference(ctx, taskID, req)
}

// generateNoTools runs a final inference with no tools offered, so the model is
// forced to produce a textual answer instead of requesting more tool calls. It
// finishes a task that exhausted its tool-round budget or whose loop was cut
// for repeating itself.
func (r *Runtime) generateNoTools(ctx context.Context, requestID string, taskID string, convo *conversation) (port.InferenceResponse, error) {
	req := port.InferenceRequest{
		RequestID: requestID,
		Messages:  convo.render(r.maxPromptChars),
		Tools:     nil,
	}
	return r.runInference(ctx, taskID, req)
}

// generateStep opens one step of the event log (spec §5: step/start, then
// barrier 1 "before model request"), issues req via generate, and records the
// resulting assistant/message. It does NOT close the step -- the caller
// closes it once it knows the fate of any tool calls this response requested
// (see runToolLoop), because a step is not finished until they have been
// dispatched or explicitly abandoned.
//
// On its OWN failure (the barrier or the generate call itself) it closes the
// step here, before returning: nothing downstream will ever get the chance
// to, since the caller has nothing to execute a fate for. errors.Is(err,
// context.Canceled) is read back through to give the closing reason
// "cancelled" instead of "failed", matching closingTurnReason's mapping for
// the turn this step lives in.
func (r *Runtime) generateStep(ctx context.Context, rec *eventRecorder, requestID, taskID string, convo *conversation, tools *tool.Registry) (resp port.InferenceResponse, err error) {
	rec.recordStepStart()
	defer func() {
		if err != nil {
			rec.recordStepEnd(closingStepReason(err))
		}
	}()
	if err = rec.barrier(ctx, "before model request"); err != nil {
		return port.InferenceResponse{}, err
	}
	resp, err = r.generate(ctx, requestID, taskID, convo, tools)
	if err != nil {
		return port.InferenceResponse{}, err
	}
	rec.recordAssistantMessage(resp.Text, resp.ToolCalls, eventUsage{
		Prompt: resp.PromptTokens, Completion: resp.CompletionTokens,
		Cached: resp.CachedTokens, Total: resp.TotalTokens,
	}, "")
	return resp, nil
}

// generateFinalStep is generateStep's counterpart for the tool-budget-exhausted
// / loop-cut closing answer (generateNoTools): same step lifecycle (step/start
// + barrier 1, record the response, leave closing to the caller), no tools
// offered so the model is forced to answer in text.
func (r *Runtime) generateFinalStep(ctx context.Context, rec *eventRecorder, requestID, taskID string, convo *conversation) (resp port.InferenceResponse, err error) {
	rec.recordStepStart()
	defer func() {
		if err != nil {
			rec.recordStepEnd(closingStepReason(err))
		}
	}()
	if err = rec.barrier(ctx, "before model request"); err != nil {
		return port.InferenceResponse{}, err
	}
	resp, err = r.generateNoTools(ctx, requestID, taskID, convo)
	if err != nil {
		return port.InferenceResponse{}, err
	}
	rec.recordAssistantMessage(resp.Text, resp.ToolCalls, eventUsage{
		Prompt: resp.PromptTokens, Completion: resp.CompletionTokens,
		Cached: resp.CachedTokens, Total: resp.TotalTokens,
	}, "")
	return resp, nil
}

// runInference sends req, streaming token deltas as RuntimeEvent{Type:"token"}
// when the maas client supports streaming, otherwise going through the
// synchronous path. A token publish failure is logged (Warn) but never aborts
// inference: token events are a best-effort display channel, the authoritative
// result is the returned InferenceResponse (and GetTaskResult on the GUI side).
func (r *Runtime) runInference(ctx context.Context, taskID string, req port.InferenceRequest) (port.InferenceResponse, error) {
	if r.debug {
		r.logInferenceRequest(taskID, req)
	}
	if s, ok := r.maas.(port.MaasStreamingClient); ok {
		return s.GenerateStream(ctx, req, func(delta string) {
			if err := r.events.Publish(ctx, domain.RuntimeEvent{
				Type: "token", TaskID: taskID, Message: delta, CreatedAt: time.Now(),
			}); err != nil {
				r.logger.Warn("publish token delta failed", "task_id", taskID, "err", err)
			}
		})
	}
	return r.maas.Generate(ctx, req)
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

// disambiguateCallIDs resolves the call_id each of this round's calls is
// recorded, dispatched and answered under, one per call in order.
//
// Two things make the model's own ids unusable as-is. A provider may omit tool
// call ids entirely, in which case the adapter fills the id in with the
// FUNCTION NAME (see adapter.openAIToolCalls) -- so a round that calls
// read_file twice in parallel arrives as two calls both carrying "read_file".
// A provider that does send ids only promises they are unique within one
// response. Either way spec §4.3.1 rule 4 is violated the moment the second
// tool/call is recorded while the first is still unanswered: the two are then
// indistinguishable to anything pairing results to calls by id.
//
// Only ids that actually collide within this round are rewritten, and only from
// their second occurrence onward. When the provider supplies real ids -- the
// common case -- every id passes through untouched, so the id semantics this
// repo shares with its providers are unchanged; the rewrite is confined to the
// rounds that would otherwise be ambiguous.
func disambiguateCallIDs(calls []domain.ToolCall, taskID string) []string {
	base := make([]string, len(calls))
	// Every id this round arrived with, including the ones not yet assigned: a
	// suffixed id must not land on a later call's own id either.
	arrived := make(map[string]bool, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			// The provider gave nothing at all, not even a function name to
			// degrade to. Name it after the task and tool so the event still
			// says what was called; collisions from this are handled below like
			// any other.
			id = taskID + ":" + call.Name
		}
		base[i] = id
		arrived[id] = true
	}
	ids := make([]string, len(calls))
	used := make(map[string]bool, len(calls))
	for i, id := range base {
		if used[id] {
			// At most len(calls) ids are used and len(calls) arrived, so one of
			// the first 2*len(calls)+1 suffixes is always free. Exhausting them
			// is arithmetically impossible, not a runtime condition to recover
			// from.
			free := false
			for n := 2; n <= 2*len(calls)+2; n++ {
				candidate := fmt.Sprintf("%s#%d", id, n)
				if !used[candidate] && !arrived[candidate] {
					id, free = candidate, true
					break
				}
			}
			if !free {
				panic(fmt.Sprintf("runtime: no free call id for %q among %d calls", id, len(calls)))
			}
		}
		used[id] = true
		ids[i] = id
	}
	return ids
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
	// spec §4.3.1 rule 4: within one step, two unanswered tool/call events must
	// not share a call_id -- the projection pairs a result to its call by that
	// id alone (rule 2), so two live calls under one id make the pair
	// ambiguous. The ids the model hands us do not guarantee it (see
	// disambiguateCallIDs), so this round settles them once, here, before the
	// first of them is recorded or dispatched.
	ids := disambiguateCallIDs(calls, task.ID)
	results := make([]domain.ToolResult, 0, len(calls))
	for i := range calls {
		// Written back into st.resp rather than kept in a local: everything
		// downstream identifies this round's calls by this id and all of them
		// must agree with the tool/call recorded below. The assistant message
		// already appended to the exchange holds THIS slice (appendAssistant
		// stores it by reference, so the provider sees the same ids), and
		// appendToolResults pairs results to calls by it.
		calls[i].ID = ids[i]
		call := calls[i]
		if err := r.events.Publish(ctx, domain.RuntimeEvent{
			Type:      "tool_call_requested",
			TaskID:    task.ID,
			Message:   call.Name,
			CreatedAt: time.Now(),
		}); err != nil {
			return nil, fmt.Errorf("publish tool request event: %w", err)
		}
		// spec §5 barrier 2: tool/call must be durably on disk BEFORE dispatch,
		// not after. Otherwise a crash inside the tool body -- the one place a
		// call has external side effects -- leaves a call that really happened
		// with no record of it ever being made, and recovery cannot reconstruct
		// a result for a call it never saw. A flush failure here means the call
		// is NOT dispatched at all (fail-closed): st.events.barrier returning an
		// error aborts this whole executeToolCalls call before dispatchToolCall
		// is ever reached.
		st.events.recordToolCall(call)
		if err := st.events.barrier(ctx, "before tool dispatch"); err != nil {
			// Fail-closed means this call is NOT dispatched, so the tool/call
			// just buffered describes something that never happened. flush
			// deliberately keeps its buffer when Append fails (so a retry loses
			// nothing), and this run flushes once more on its way out while
			// closing the turn -- a TRANSIENT failure (a full disk that got
			// freed, a lock timeout) would therefore persist this call after
			// all: orphaned forever, since no result can follow a dispatch that
			// never ran, and recovery would synthesize a result for a call that
			// never happened. Recording more than what happened is worse than
			// recording less (spec §4.3.1 rule 1 guards the opposite direction),
			// so take it back before returning.
			st.events.dropBufferedToolCall(call.ID)
			return nil, fmt.Errorf("session event barrier before dispatching call %s for task %s: %w", call.ID, task.ID, err)
		}
		dispatchStart := time.Now()
		result, err := r.dispatchToolCall(ctx, agent, task, call, st)
		dispatchDur := time.Since(dispatchStart)
		if err != nil {
			// spec §4.3.1 rule 1: every recorded tool/call gets a tool/result,
			// failure/denial/cancellation included -- a dispatch-level Go error
			// is exactly that case (the call never produced a domain.ToolResult
			// at all), so it is recorded here rather than silently only living
			// on as the synthesized ToolResult fed back to the model below.
			st.events.recordToolResult(call.ID, err.Error(), true, dispatchDur)
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
		// One id for this call, everywhere: the tool/call above was recorded
		// under call.ID, so the tool/result below and the answer the model is
		// shown must carry the same one or the by-call_id pairing (spec §4.3.1
		// rule 2) silently comes apart -- and after disambiguateCallIDs the
		// handler's own copy is not necessarily the id this round settled on.
		// Every handler today does write call.ID into its result (dispatchCallTool
		// even re-tags on their behalf), but that is convention; this line makes
		// it mechanism, so one forgetful new handler cannot break the pairing.
		result.CallID = call.ID
		results = append(results, result)
		// A successful dispatch still may have Success=false (a tool that ran
		// and reported its own failure, or the ToolGate's "denied by human
		// approver" result) -- that is answered here too, is_error tracking
		// result.Success rather than the nil dispatch error.
		preview := result.Output
		if !result.Success {
			preview = result.Error
		}
		st.events.recordToolResult(call.ID, preview, !result.Success, dispatchDur)
		if call.Name == "write_file" {
			raw, ok := call.Arguments["path"]
			if !ok || strings.TrimSpace(raw) == "" {
				// write_file's contract always carries a path; a reported success
				// without one is an invariant violation, not an optional field.
				return nil, fmt.Errorf("write_file for task %s reported success without a path argument", task.ID)
			}
			rel, err := workspaceRelPath(task.WorkingDir, raw)
			if err != nil {
				return nil, fmt.Errorf("normalize generated file %q for task %s: %w", raw, task.ID, err)
			}
			st.generatedFiles = appendUniqueStr(st.generatedFiles, rel)
		}
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

// workspaceRelPath normalizes a write_file path (relative, or absolute within
// root) to a slash path relative to root. An empty root (unbound session)
// yields the cleaned slash path as-is. It errors if the result escapes root.
func workspaceRelPath(root, p string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return filepath.ToSlash(filepath.Clean(p)), nil
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", p)
	}
	return filepath.ToSlash(rel), nil
}

// appendUniqueStr appends v to xs unless it is already present, preserving
// first-seen order. Used to dedupe write_file paths into loopState.generatedFiles.
func appendUniqueStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
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

// truncateText caps text at maxChars runes. When it truncates, it appends a
// self-describing footer stating this is a HARD truncation (a context-budget
// limit, not a data or parameter problem) so the model does not misread a cut
// result as "wrong arguments" and retry with different parameters — the failure
// mode that ran one task to 60 tool calls / 831.7k input (session-…955100). The
// footer names the shown and total rune counts so the model knows how much it is
// missing. maxChars<=0 disables truncation.
func truncateText(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	total := len(runes)
	if total <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + fmt.Sprintf(
		"\n\n──────── [输出被硬截断 / OUTPUT HARD-TRUNCATED] ────────\n"+
			"这是硬截断（上下文预算限制），非数据或参数问题——换参数或换工具重试不会有帮助。\n"+
			"This is a hard truncation (a context-budget limit), not a data/parameter problem; retrying with different arguments or tools will not help.\n"+
			"显示 %d / 共 %d 字符（rune）。\n",
		maxChars, total)
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

func (r *Runtime) buildPrompt(ctx context.Context, agent domain.Agent, task domain.Task, catalog *capability.Catalog) (string, int, error) {
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
			return "", 0, fmt.Errorf("build cognitive context: %w", err)
		}
		if r.debug {
			r.logContextBlocks(task.ID, built.Blocks)
		}
		// The hint is only needed on the Core path: Core renders a "Tools:" line
		// that, when empty under the lazy protocol, can mislead the model into
		// believing no tools exist. The plain paths below carry no such line.
		return built.Prompt + r.lazyToolHint(toolNames), built.StablePrefixLen, nil
	}
	if r.contextPrefix != "" {
		return r.contextPrefix + "\n\nTask input:\n" + task.Input, 0, nil
	}
	return task.Input, 0, nil
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
	r.recordEpisode(agent, task, "failure:"+reason, task.Input)
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
