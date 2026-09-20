package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/domain"
)

const (
	// roleOrchestrator may spawn sub-tasks; roleLeaf may not. Children default to
	// leaf so delegation does not recurse without an explicit orchestrator role.
	roleOrchestrator = "orchestrator"
	roleLeaf         = "leaf"

	defaultMaxSpawnDepth = 2
	defaultMaxConcurrent = 3
)

// SubTaskSpec describes one delegated unit of work. Goal is required; Context is
// optional supporting detail. Role selects the child's delegation capability
// ("orchestrator" to allow further nesting, otherwise "leaf"). AgentID names a
// configured agent to run the child as, so the child runs with that agent's own
// tool-permission role, tool authorisation, model profile and workspace; empty
// runs a clone of the delegating runtime under a derived id.
//
// A non-empty AgentID is mutually exclusive with Role "orchestrator": a named
// agent's own runtime never registers delegate_task (see ResolveDelegate), so
// it could never act on that role, and the combination is refused rather than
// one side being silently ignored. validateSubTaskSpec refuses it, which is
// what puts the refusal inside RunSubTasks' whole-batch pre-flight;
// ResolveDelegate refuses it again as a last line of defence for callers that
// reach it directly. A non-empty Toolsets is NOT refused
// together with AgentID -- see the Toolsets field doc below and
// DelegationContext.Toolsets for how the two combine.
type SubTaskSpec struct {
	ParentTaskID string
	AgentID      string
	Goal         string
	Context      string
	Role         string
	// Toolsets, when non-empty, narrows the child runtime to only these tool
	// names. Empty inherits the full parent tool set. This is the
	// token-optimization knob: a focused sub-agent is offered only the tools
	// its goal needs. validateSubTaskSpec always checks these names against
	// THIS runtime's own registry (the delegating side), regardless of
	// AgentID. Combined with a non-empty AgentID, ResolveDelegate then maps
	// the same names onto the named agent's own registry, layered on top of
	// (not replacing) that agent's own DisabledTools deny-list -- see
	// DelegationContext.Toolsets.
	Toolsets []string
}

// SubTaskResult is what a delegated sub-task returns to its parent: only the
// final summary text, not the child's full working context. This is the whole
// point of delegation — the child burns its own context and hands back a digest.
type SubTaskResult struct {
	TaskID  string
	Summary string
	Err     string
	// StopReason is why the child's tool loop ended. Without it a Summary is
	// just text: a child that answered and a child that was cut off mid-work
	// read the same.
	StopReason domain.StopReason
}

// SubTaskHandle 指向一条后台子任务。TaskID 是它的 UUID，外部可以拿它查这条子任务
// 的运行记录——包括进程重启之后（那时它的状态是 interrupted）。
//
// 完成仍然经运行时事件（Type "subtask_completed"）送达。句柄本身不等待、不轮询。
type SubTaskHandle struct {
	TaskID string
}

// canDelegate reports whether this runtime may spawn sub-tasks: it must be an
// orchestrator (the root is one by default) and must not already be at the spawn
// depth limit.
func (r *Runtime) canDelegate() bool {
	if r.depth >= r.maxSpawnDepth {
		return false
	}
	return r.role == roleOrchestrator
}

// validateSubTaskSpec decides whether one delegation request may be admitted,
// without creating anything: a request rejected here starts no child and has
// no side effect.
//
// It requires spec.Goal to be non-empty, then checks against state this
// runtime already holds: the two role constants declared at the top of this
// file, the depth a spawned child would land at (this runtime's own depth
// plus one) against maxSpawnDepth, and -- for each requested toolset name --
// whether this runtime's tool registry exposes that name. A nil registry
// exposes nothing, so it rejects every requested name.
//
// Exposure is asked of tool.Registry.Descriptors(), which resolves along the
// parent chain and applies each view's filter. tool.Registry.HasTool answers a
// different question -- it counts only a registry's OWN registrations -- and
// the two disagree exactly where delegation lives: a plugin's tool reaches a
// task registry by inheritance, and a child narrowed by Toolsets runs on a
// Subset view that registers nothing of its own. Under HasTool both of those
// genuinely reachable tools would be refused, and the refusal would say the
// agent does not have a tool it can in fact execute.
//
// A non-empty spec.AgentID is checked last. First against spec.Role alone --
// a named agent combined with roleOrchestrator is refused here rather than
// only in ResolveDelegate, so RunSubTasks' whole-batch pre-flight can refuse
// the batch before any entry starts -- then against r.delegationAgents, where
// a nil resolver or an unrecognised name are both refused rather than silently
// falling back to a plain clone of this runtime under someone else's label.
func (r *Runtime) validateSubTaskSpec(spec SubTaskSpec) error {
	if strings.TrimSpace(spec.Goal) == "" {
		return fmt.Errorf("validate sub task: goal is required")
	}
	switch spec.Role {
	case "", roleLeaf, roleOrchestrator:
	default:
		return fmt.Errorf("validate sub task: role %q is not %q or %q", spec.Role, roleOrchestrator, roleLeaf)
	}
	if r.depth+1 > r.maxSpawnDepth {
		return fmt.Errorf("validate sub task: delegation depth %d exceeds max spawn depth %d", r.depth+1, r.maxSpawnDepth)
	}
	// Subset NARROWS, so a name it does not recognise is dropped in silence and
	// the child ends up with fewer tools than the caller asked for -- possibly
	// none. (Without, which widens, may ignore unknown names: removing a tool an
	// agent never had is a real no-op.)
	if len(spec.Toolsets) > 0 {
		exposed := make(map[string]bool)
		if r.tools != nil {
			for _, descriptor := range r.tools.Descriptors() {
				exposed[descriptor.Name] = true
			}
		}
		for _, name := range spec.Toolsets {
			if !exposed[name] {
				return fmt.Errorf("validate sub task: toolset name %q is not a tool this agent exposes", name)
			}
		}
	}
	// Naming an agent is a request to run as THAT agent's configuration. A
	// deployment with no agent directory cannot honour it, and an unknown name
	// cannot either; both refuse rather than quietly running the parent's clone
	// under someone else's label.
	if spec.AgentID != "" {
		// This combination is decidable from the spec alone, so it is decided
		// here, where RunSubTasks' whole-batch pre-flight can reach it. Deciding
		// it only in ResolveDelegate would let the pre-flight admit a batch it
		// must refuse, and entry 0 would already be running (with side effects
		// of its own) by the time entry 1 was rejected -- a partial execution,
		// which is precisely what that pre-flight exists to prevent.
		//
		// Exactly two places refuse the combination, and that is deliberate:
		// ResolveDelegate keeps its own copy as a last line of defence, because
		// it is an exported DelegationAgents method whose contract has to hold
		// on its own rather than on this function having run first. Same
		// reasoning as newSubRuntime, which re-checks the depth ceiling this
		// function has already checked.
		if spec.Role == roleOrchestrator {
			return fmt.Errorf("validate sub task: role %q cannot be combined with agent_id %q; a named agent's runtime never registers delegate_task, so it could never act as an orchestrator; delegate by name as a leaf, or delegate by role without a name",
				roleOrchestrator, spec.AgentID)
		}
		if r.delegationAgents == nil {
			return fmt.Errorf("validate sub task: agent_id %q was requested but this deployment has no configured agents", spec.AgentID)
		}
		if !r.delegationAgents.HasAgent(spec.AgentID) {
			return fmt.Errorf("validate sub task: agent_id %q is not a configured agent; configured agents are %v",
				spec.AgentID, r.delegationAgents.AgentNames())
		}
	}
	return nil
}

// newSubRuntime clones this runtime for a child at depth+1, sharing the inference
// client, tools, context builder, and audit/event sinks but starting with an
// empty conversation history so the child gets an independent context. It fails
// loud when the new depth would exceed the spawn limit.
func (r *Runtime) newSubRuntime(role string, toolsets []string) (*Runtime, error) {
	depth := r.depth + 1
	if depth > r.maxSpawnDepth {
		return nil, fmt.Errorf("delegation depth %d exceeds max spawn depth %d", depth, r.maxSpawnDepth)
	}
	if role == "" {
		role = roleLeaf
	}
	if role != roleOrchestrator && role != roleLeaf {
		return nil, fmt.Errorf("delegation role %q is not %q or %q", role, roleOrchestrator, roleLeaf)
	}
	tools := r.tools
	if len(toolsets) > 0 && tools != nil {
		tools = tools.Subset(toolsets...)
	}
	child := &Runtime{
		maas:           r.maas,
		audit:          r.audit,
		events:         r.events,
		contextBuilder: r.contextBuilder,
		contextPrefix:  r.contextPrefix,
		tools:          tools,
		// The deny-list must survive delegation: a child built by hand here
		// would otherwise regain a tool its parent was denied. Carry it over
		// explicitly, like tools/logger above.
		disabledTools:      r.disabledTools,
		maxToolRounds:      r.maxToolRounds,
		maxToolResultChars: r.maxToolResultChars,
		maxPromptChars:     r.maxPromptChars,
		lazyTools:          r.lazyTools,
		role:               role,
		depth:              depth,
		maxSpawnDepth:      r.maxSpawnDepth,
		maxConcurrent:      r.maxConcurrent,
		// Carried like tools and the deny-list: a child that lost it could not
		// resolve an agent name its parent could.
		delegationAgents: r.delegationAgents,
		// The child is built as a struct literal, bypassing NewRuntime and its
		// nil-logger fallback, so the parent's logger must be carried over
		// explicitly: a child left with a nil logger would panic the first time
		// one of its failure paths tried to record anything.
		logger: r.logger,
		// The child builds its own per-task catalog in RunTask (from its own
		// effective registry, which may be a narrowed subset); it only needs the
		// skill provider carried over so its catalog can list skills too. Its
		// loaded block still starts empty -- the child gets an independent context.
		//
		// This is structural, not incidental: "loaded" lives on loopState, which
		// RunTask constructs fresh for every run (including a child's own
		// RunTask call below), not on Runtime. Runtime carries no loaded-capability
		// field at all, so there is nothing here for newSubRuntime to copy from
		// the parent even if it wanted to -- a parent's in-flight loaded block
		// simply has no path into a spawned child's loopState.
		capabilitySkills: r.capabilitySkills,
		// The child runs its own RunTask, so it needs a gate — and it must be
		// the PARENT'S gate, not one of its own. A sub-task runs inside its
		// parent's task, which already holds that gate open; a private gate
		// would make the child invisible to the apply that is waiting for the
		// parent's boundary, which is the one thing the gate exists to prevent.
		gate: r.gate,
		// The store, not a recorder: each RunTask call builds its own
		// *eventRecorder (one per execution, see eventRecorder's type doc), and
		// the child's own RunTask does exactly that. Without this the child
		// silently drops every event it would otherwise write — the exact
		// "the seam exists but nothing reaches it" failure shape this repo has
		// hit twice before with per-agent tool/approval wiring.
		sessionEvents: r.sessionEvents,
		// 子运行时必须继承运行记录的落点：没有它，后台子任务的 RunTask 拿到的是
		// nil，落盘在子任务这条路上整条消失——而那正是本设计的目标场景。
		taskRuns: r.taskRuns,
		// The child runs on the parent's inference client, so it runs under the
		// parent's model profile; without this its own session log would record
		// an empty model_profile on every assistant/message (spec §4.1).
		modelProfile: r.modelProfile,
	}
	return child, nil
}

// RunSubTask delegates one sub-task to a fresh child runtime and returns only the
// child's final summary. Scheduling failures (delegation not permitted, depth
// exceeded, missing goal) are returned loud; a child that runs but fails is also
// returned as an error so the caller decides how to surface it.
func (r *Runtime) RunSubTask(ctx context.Context, spec SubTaskSpec) (SubTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return SubTaskResult{}, err
	}
	if err := r.validateSubTaskSpec(spec); err != nil {
		return SubTaskResult{}, err
	}
	if !r.canDelegate() {
		return SubTaskResult{}, fmt.Errorf("run sub task: delegation not permitted for role %q at depth %d", r.role, r.depth)
	}
	subTaskID := r.nextSubTaskID()
	agent, child, err := r.childFor(ctx, spec, subTaskID)
	if err != nil {
		return SubTaskResult{}, err
	}
	return r.runChild(ctx, agent, child, subTaskID, spec, false, "")
}

// childFor builds the runtime that will run one sub-task: the named agent's own
// runtime when spec names one, otherwise a clone of this runtime. It returns the
// domain.Agent to run as alongside it, because a named child runs as that
// agent's configured role rather than the fixed one an unnamed child uses.
//
// A resolution failure is wrapped and returned. It is never turned into a clone
// of this runtime: the caller asked for a specific agent, and a clone wearing
// that agent's name would run with this runtime's tools and model instead of
// the ones the name selects.
func (r *Runtime) childFor(ctx context.Context, spec SubTaskSpec, subTaskID string) (domain.Agent, *Runtime, error) {
	if spec.AgentID == "" {
		child, err := r.newSubRuntime(spec.Role, spec.Toolsets)
		if err != nil {
			return domain.Agent{}, nil, err
		}
		return domain.Agent{ID: subTaskID, Role: "developer"}, child, nil
	}
	agent, child, err := r.delegationAgents.ResolveDelegate(ctx, spec.AgentID, DelegationContext{
		Depth:         r.depth + 1,
		MaxSpawnDepth: r.maxSpawnDepth,
		Role:          spec.Role,
		Toolsets:      spec.Toolsets,
	})
	if err != nil {
		return domain.Agent{}, nil, fmt.Errorf("resolve delegate agent %q: %w", spec.AgentID, err)
	}
	// A named delegation is the one channel through which the choice of which
	// configured agent runs a sub-task is made by the MODEL rather than an
	// operator (spec §4.2): a restricted agent can name an unrestricted one and
	// inherit its role, tool authorisation, model profile and workspace. That
	// channel is intentional and is exactly as wide as the deployment's agent
	// directory -- the audit event below does not gate it, it only makes the
	// choice answerable after the fact: which task asked, which agent it named,
	// and what for. It is appended only here, after ResolveDelegate has already
	// succeeded: a resolution failure is returned above and starts no child, so
	// there is nothing yet worth recording.
	//
	// RequestID 直接写 spec.ParentTaskID，回答的就是「哪条任务发起了这次委派」——见
	// domain.AuditEvent.RequestID 的字段契约。spec.ParentTaskID 的唯一来源是
	// handleDelegateTask 从 ctx 读到的、当时真正在跑的那条任务的 id（拿不到就拒绝派
	// 发），所以这句话在生产上也成立，而不只是在喂字面量的用例里成立。
	//
	// 它以前写的是 subTaskID，靠 ParentTaskIDForSubTask 从 "<父>:sub-N" 里再解析回来；
	// 子任务 id 现在是 UUID，那条解析路不存在了，而父子关系本来就该存成字段、不该编
	// 码进 id。子任务自己的 id 仍在这条事件的 ID 里（subTaskID +
	// ":delegated-to-agent"），两头都没丢。
	//
	// This runtime carries no agent identity of its own -- Config has no such
	// field, because a Runtime is generic and only learns which domain.Agent it is
	// running as through RunTask's own parameter, which this function never
	// receives -- so the parent TASK id is the most specific caller-side identity
	// actually available at this call site, and Hash is free to carry the one
	// piece of content that has no field of its own: the goal the parent asked the
	// named agent to do.
	if auditErr := r.audit.Append(ctx, domain.AuditEvent{
		ID:          subTaskID + ":delegated-to-agent",
		RequestID:   spec.ParentTaskID,
		SubjectType: "agent",
		SubjectID:   spec.AgentID,
		Action:      "subtask_delegated_to_agent",
		Hash:        spec.Goal,
		CreatedAt:   time.Now(),
		// Without this the row would be stored with a blank origin, and
		// internal/storage's auditOrigin normalises a blank to
		// domain.OriginAgent ("agent"). The one event written specifically to
		// answer "who handed this work to whom" would then read back as the
		// agent's own work -- the audit actively misleading, not merely
		// silent. The depth is the CHILD's (r.depth+1, the same value handed
		// to ResolveDelegate above), because the delegated sub-run is what
		// this event is about; lazytools.go attributes a runtime's own tool
		// calls with its own r.depth for the same reason.
		Origin: domain.DelegateOrigin(r.depth + 1),
	}); auditErr != nil {
		// Fail-loud does not mean fail-the-caller here: the target agent has
		// already been resolved and is about to do real work on the parent's
		// behalf, and refusing to run it because the audit store had a hiccup
		// would trade a forensic record for the very delegation that record was
		// meant to describe. So the failure is not swallowed -- it is logged,
		// structured, at Warn -- and the delegation proceeds regardless.
		r.logger.WarnContext(ctx, "record named delegation audit event failed",
			"component", "runtime",
			"sub_task_id", subTaskID,
			"parent_task_id", spec.ParentTaskID,
			"agent_id", spec.AgentID,
			"error", auditErr)
	}
	return agent, child, nil
}

// runChild executes a prepared child runtime against spec and maps its run to a
// SubTaskResult, running it as agent. A child run error is wrapped and returned.
//
// background 报告这条子任务是不是后台派发的；openingRunID 是派发方已经插好的那一行
// 的 run id（同步委派两者分别是 false 与空串，开始那一行由 RunTask 自己插）。两者
// 必须成对给：只给 background，RunTask 会自己 mint 一个 id 再插一行，派发方那一行
// 永远停在 running；只给 openingRunID，这条运行记录会被标成一条直连任务。
func (r *Runtime) runChild(ctx context.Context, agent domain.Agent, child *Runtime, subTaskID string, spec SubTaskSpec, background bool, openingRunID string) (SubTaskResult, error) {
	agentID := spec.AgentID
	if agentID == "" {
		agentID = subTaskID
	}
	// SessionID is deliberately left unset. Task 1's decision D-A already falls
	// the session event recorder back to task.ID when SessionID is empty
	// (newEventRecorder), so a sub-task with no session identity of its own
	// gets its own short session log keyed by subTaskID instead of writing
	// into its parent's -- the parent's log keeps only the one tool/call +
	// tool/result pair for RunSubTask itself, exactly the shape spec F1 wants.
	// No extra code is needed here; this comment is the whole of the decision.
	// ParentTaskID 与 Goal 必须填：parent_task_id 空串在那个字段的契约里是「顶层
	// 任务」，不是「不知道」，漏填会让每一条子任务的运行记录都伪装成一条直连任务，
	// 父子树整棵断掉且不报错。Goal 则是让一条中断记录读起来知道它在干什么。
	task := domain.Task{
		ID:           subTaskID,
		AgentID:      agentID,
		Input:        composeSubTaskInput(spec),
		CreatedAt:    time.Now(),
		ParentTaskID: spec.ParentTaskID,
		Background:   background,
		Goal:         spec.Goal,
		RunID:        openingRunID,
	}
	run, err := child.RunTask(ctx, agent, task)
	if err != nil {
		return SubTaskResult{}, fmt.Errorf("run sub task %q: %w", subTaskID, err)
	}
	return SubTaskResult{TaskID: subTaskID, Summary: run.Result, StopReason: run.StopReason}, nil
}

// RunSubTasks delegates a batch concurrently, bounded by maxConcurrent. Results
// preserve input order. A single sub-task that fails during execution does not
// abort the batch: its error is reported in that entry's Err field so the model
// sees it, matching the "report each result, swallow nothing" contract.
//
// Several conditions short-circuit that and fail the whole call loud instead,
// with no per-entry results at all: an already-cancelled context, an empty
// batch, delegation not permitted for this runtime (a scheduling-level
// failure), and any entry failing validateSubTaskSpec during the batch
// pre-flight below.
func (r *Runtime) RunSubTasks(ctx context.Context, specs []SubTaskSpec) ([]SubTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("run sub tasks: no sub-tasks provided")
	}
	if !r.canDelegate() {
		return nil, fmt.Errorf("run sub tasks: delegation not permitted for role %q at depth %d", r.role, r.depth)
	}
	// Pre-flight the whole batch before starting any of it. A child has side
	// effects of its own, so discovering entry 4 is malformed after entries 0-3
	// are already running is not a refusal, it is a partial execution. (Entries
	// are numbered from 0 here, matching the "entry %d" below and the index i
	// ranges over.)
	for i, spec := range specs {
		if err := r.validateSubTaskSpec(spec); err != nil {
			return nil, fmt.Errorf("run sub tasks: entry %d: %w", i, err)
		}
	}
	results := make([]SubTaskResult, len(specs))
	sem := make(chan struct{}, r.maxConcurrent)
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec SubTaskSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := r.RunSubTask(ctx, spec)
			if err != nil {
				results[i] = SubTaskResult{TaskID: spec.ParentTaskID, Err: err.Error()}
				return
			}
			results[i] = res
		}(i, spec)
	}
	wg.Wait()
	return results, nil
}

// RunSubTaskAsync starts a sub-task in the background and returns a handle
// immediately. Completion (or failure) is published as a "subtask_completed"
// runtime event. The work runs on a detached context so it survives the tool call
// that launched it, but it is process-local: a parent exit loses it.
//
// Because it survives the tool call, it can also outlive the parent TASK. It
// therefore takes a task-boundary token of its own before it starts, so a
// plugin change waits for the background work as well and never lands on top of
// it — see the comment on that token below.
func (r *Runtime) RunSubTaskAsync(ctx context.Context, spec SubTaskSpec) (SubTaskHandle, error) {
	if err := ctx.Err(); err != nil {
		return SubTaskHandle{}, err
	}
	if err := r.validateSubTaskSpec(spec); err != nil {
		return SubTaskHandle{}, err
	}
	if !r.canDelegate() {
		return SubTaskHandle{}, fmt.Errorf("run sub task async: delegation not permitted for role %q at depth %d", r.role, r.depth)
	}
	subTaskID := r.nextSubTaskID()
	agent, child, err := r.childFor(ctx, spec, subTaskID)
	if err != nil {
		return SubTaskHandle{}, err
	}

	// 开始那一行由派发方写、终态由子运行时写，所以两边必须是同一个 store。不是的话
	// 那一行永远停在 running：下一次启动的 SweepRunning 会把一条正常跑完的子任务扫成
	// interrupted——比不落盘更坏，因为它给出的是一个确信的错误答案。
	//
	// 这不是假想：克隆路径（newSubRuntime）显式抄了 taskRuns，具名路径
	// （DelegationAgents.ResolveDelegate）组 Config 时根本没有 TaskRuns 这一项。校验放
	// 在这里，是为了让「不一致」只能表现为一次响亮的拒绝，而不是一条孤儿行。把
	// TaskRuns 接进 resolver 是另一件事；接上之后这条校验自然通过，不必删。
	//
	// 只在派发方自己要插行时才校验：r.taskRuns 为 nil 时下面什么都不写、RunID 传空串，
	// 子运行时自己开自己的行，两边各写各的没有分歧。
	if r.taskRuns != nil && child.taskRuns != r.taskRuns {
		return SubTaskHandle{}, fmt.Errorf(
			"delegate background sub-task %s: the child runtime records its runs in a different store, so the opening row this dispatcher writes would never be finished",
			subTaskID)
	}

	// The background sub-task keeps running after the tool call that started it
	// returns, and so possibly after the parent task itself has ended and
	// retired its Begin. Depth alone would not cover that: the child's own
	// BeginChild inside RunTask lasts only as long as its RunTask, and between
	// the parent retiring and the goroutine reaching that call — and again
	// between that call returning and this goroutine publishing its outcome —
	// nothing would be counted, so an apply could land on top of live work.
	//
	// So take a token HERE, on the caller's goroutine, while the parent is
	// demonstrably still in flight: RunSubTaskAsync is reached from a tool call
	// inside the parent's RunTask, which holds the gate. ApplyAtBoundary then
	// waits for the background sub-task too.
	//
	// Ownership passes to the goroutine and to nothing else: endBackground is
	// captured by exactly one closure and retired by its first deferred call, so
	// every way out — the child failing, the publish failing, a panic unwinding
	// — releases it exactly once. 这一行与 go 语句之间只有一条会失败的路（下面那次
	// 落盘），它自己在返回之前把票还掉；除此之外没有任何路径拿了票又把它丢掉。
	endBackground := r.gate.BeginChild()

	// 运行记录与边界票在同一处取得，理由相同：这一刻父任务确凿还在飞。放进下面的
	// goroutine 里写，父进程紧接着退出就什么都没留下——而那正是这条记录要覆盖的场景。
	// 插不进去就不起这个子任务，边界票也要还回去：票不还，插件变更会永远等一条从来
	// 没跑起来的子任务。
	//
	// run id 只在真的要插行时铸、并经 task.RunID 带给 RunTask：谁写下了开始那一行，谁
	// 就说了算。RunTask 以 task.RunID 非空判断「已经有人插过行了」，于是它不再插第二
	// 次，收尾写回的正是这一行。反过来，不铸就是空串，RunTask 自己开自己的行。
	//
	// 不变量：task.RunID 非空 ⟺ 确实有人插过那一行。无条件铸一个会让 RunTask 以为行
	// 已经有了而跳过插行，收尾却写向一条从未插入的行——真实的 FinishTaskRun 找不到它
	// 会报错，于是一条正常跑完的子任务被记成 failed。
	//
	// 顺序：这次预插排在 childFor 里那条 subtask_delegated_to_agent 审计之后，是刻意
	// 的。预插要的 agent 身份与「两边同一个 store」这两件事都由 childFor 产出，排到它
	// 前面就得先写一条还不知道派给谁的行。于是预插失败时，审计里会留下一条没有运行记
	// 录的委派——那不是两份记录互相矛盾，而是各自说着实话：确实做出过一次选择（审计
	// 的事），而那条子任务确实一次都没有跑起来（运行记录的事，下面失败就直接返回）。
	var openingRunID string
	if r.taskRuns != nil {
		openingRunID = uuid.NewString()
		if err := r.taskRuns.StartTaskRun(ctx, domain.TaskRun{
			ID:           openingRunID,
			TaskID:       subTaskID,
			AgentID:      agent.ID,
			StartedAt:    time.Now(),
			Status:       domain.RunStatusRunning,
			ParentTaskID: spec.ParentTaskID,
			Background:   true,
			Goal:         spec.Goal,
		}); err != nil {
			endBackground()
			return SubTaskHandle{}, fmt.Errorf("record the start of background sub-task %s: %w", subTaskID, err)
		}
	}
	go func() {
		defer endBackground()
		bg := context.WithoutCancel(ctx)
		res, err := r.runChild(bg, agent, child, subTaskID, spec, true, openingRunID)
		// ParentTaskID 必须在这里填：子任务 id 是 UUID，回注方从它身上解析不出父任务，
		// 而这一刻是父子关系唯一还在手边的地方。不填，父任务就永远等不到这条结果。
		event := domain.RuntimeEvent{
			Type:         "subtask_completed",
			TaskID:       subTaskID,
			ParentTaskID: spec.ParentTaskID,
			CreatedAt:    time.Now(),
		}
		if err != nil {
			event.Message = "sub-task failed: " + err.Error()
		} else {
			event.Message = res.Summary
		}
		// Goroutine boundary: publish the outcome. If the event sink fails, fall
		// back to the audit log.
		if pubErr := r.events.Publish(bg, event); pubErr != nil {
			// That fallback is not independent, though: audit and event bus are
			// both SQLite-backed, so whatever took out the publish routinely takes
			// this out too — not a low-probability coincidence. When both go, the
			// sub-task's outcome is gone and the parent waits forever, so the log,
			// which depends on no database, is the actual last resort. It ends the
			// work unit rather than looping.
			// RequestID 与上面那条委派事件写同一个东西：发起委派的父任务 id。两条
			// subtask_* 事件对「这条事件属于哪一次动作」给出两个不同的答案，就等于
			// 谁都回答不了。子任务自己的 id 在 ID 与 SubjectID 里。
			if auditErr := r.audit.Append(bg, domain.AuditEvent{
				ID:          subTaskID + ":subtask-publish-failed",
				RequestID:   spec.ParentTaskID,
				SubjectType: "runtime",
				SubjectID:   subTaskID,
				Action:      "subtask_event_publish_failed",
				Hash:        pubErr.Error(),
				CreatedAt:   time.Now(),
			}); auditErr != nil {
				r.logger.WarnContext(bg, "record sub-task event publish failure",
					"component", "runtime",
					"task_id", subTaskID,
					"publish_error", pubErr,
					"error", auditErr)
			}
		}
	}()
	return SubTaskHandle{TaskID: subTaskID}, nil
}

// nextSubTaskID 为一次委派铸一个新 id。
//
// 用 UUID 而不是「父任务 + 序号」：序号来自进程内计数器，重启之后从头数，于是重启
// 前后两条不同的子任务会拿到同一个 id，而运行记录的全部意义就是按 id 找回它。父子
// 关系改存 domain.Task.ParentTaskID 与 task_runs.parent_task_id，那里它是可查询的，
// 而不是要从字符串里解析出来的。
func (r *Runtime) nextSubTaskID() string { return uuid.NewString() }

// ParentTaskIDForSubTask 从老形态的子任务 id（"<父>:sub-<n>"）里解析出父任务。
// ok 报告 s 是否真是那个形态；为 false 时原样返回整串，调用方不得把它当成父任务
// 用——那会把结果回注给子任务自己。
//
// 今天铸出来的子任务 id 是 UUID（见 nextSubTaskID），解析不出任何东西；这个函数
// 留着，是因为 runtime_events 里存着改造之前发布的事件，它们的父任务只剩这一条线索。
// 新代码一律读 domain.RuntimeEvent.ParentTaskID（事件）与 domain.TaskRun.ParentTaskID
// （运行记录）——那里父子关系是存着的，而不是要从字符串里解析出来的。
func ParentTaskIDForSubTask(s string) (parentTaskID string, ok bool) {
	idx := strings.LastIndex(s, ":sub-")
	if idx < 0 {
		return s, false
	}
	return s[:idx], true
}

// composeSubTaskInput renders a sub-task spec into the child's task input: the
// goal, plus the optional supporting context under a labeled section.
func composeSubTaskInput(spec SubTaskSpec) string {
	if strings.TrimSpace(spec.Context) == "" {
		return spec.Goal
	}
	return spec.Goal + "\n\n[上下文]\n" + spec.Context
}
