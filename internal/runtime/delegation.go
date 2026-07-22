package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
// ("orchestrator" to allow further nesting, otherwise "leaf"). AgentID labels the
// child agent; empty defaults to a derived id.
type SubTaskSpec struct {
	ParentTaskID string
	AgentID      string
	Goal         string
	Context      string
	Role         string
	// Toolsets, when non-empty, narrows the child runtime to only these tool
	// names (a subset of the parent registry). Empty inherits the full parent
	// tool set. This is the token-optimization knob: a focused sub-agent is
	// offered only the tools its goal needs.
	Toolsets []string
}

// SubTaskResult is what a delegated sub-task returns to its parent: only the
// final summary text, not the child's full working context. This is the whole
// point of delegation — the child burns its own context and hands back a digest.
type SubTaskResult struct {
	TaskID  string
	Summary string
	Err     string
}

// SubTaskHandle references a background sub-task whose completion is delivered
// later as a runtime event (Type "subtask_completed"). It is process-local and
// non-durable: if the parent process exits, an in-flight background sub-task is
// lost.
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
		maas:               r.maas,
		audit:              r.audit,
		events:             r.events,
		contextBuilder:     r.contextBuilder,
		contextPrefix:      r.contextPrefix,
		tools:              tools,
		maxToolRounds:      r.maxToolRounds,
		maxToolResultChars: r.maxToolResultChars,
		maxPromptChars:     r.maxPromptChars,
		lazyTools:          r.lazyTools,
		role:               role,
		depth:              depth,
		maxSpawnDepth:      r.maxSpawnDepth,
		maxConcurrent:      r.maxConcurrent,
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
	if strings.TrimSpace(spec.Goal) == "" {
		return SubTaskResult{}, fmt.Errorf("run sub task: goal is required")
	}
	if !r.canDelegate() {
		return SubTaskResult{}, fmt.Errorf("run sub task: delegation not permitted for role %q at depth %d", r.role, r.depth)
	}
	child, err := r.newSubRuntime(spec.Role, spec.Toolsets)
	if err != nil {
		return SubTaskResult{}, err
	}
	return r.runChild(ctx, child, r.nextSubTaskID(spec.ParentTaskID), spec)
}

// runChild executes a prepared child runtime against spec and maps its run to a
// SubTaskResult. A child run error is wrapped and returned.
func (r *Runtime) runChild(ctx context.Context, child *Runtime, subTaskID string, spec SubTaskSpec) (SubTaskResult, error) {
	agentID := spec.AgentID
	if agentID == "" {
		agentID = subTaskID
	}
	task := domain.Task{
		ID:        subTaskID,
		AgentID:   agentID,
		Input:     composeSubTaskInput(spec),
		CreatedAt: time.Now(),
	}
	agent := domain.Agent{ID: agentID, Role: "developer"}
	run, err := child.RunTask(ctx, agent, task)
	if err != nil {
		return SubTaskResult{}, fmt.Errorf("run sub task %q: %w", subTaskID, err)
	}
	return SubTaskResult{TaskID: subTaskID, Summary: run.Result}, nil
}

// RunSubTasks delegates a batch concurrently, bounded by maxConcurrent. Results
// preserve input order. A single sub-task that fails does not abort the batch:
// its error is reported in that entry's Err field so the model sees it, matching
// the "report each result, swallow nothing" contract. Only a scheduling-level
// failure (delegation not permitted) fails the whole call loud.
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
func (r *Runtime) RunSubTaskAsync(ctx context.Context, spec SubTaskSpec) (SubTaskHandle, error) {
	if err := ctx.Err(); err != nil {
		return SubTaskHandle{}, err
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return SubTaskHandle{}, fmt.Errorf("run sub task async: goal is required")
	}
	if !r.canDelegate() {
		return SubTaskHandle{}, fmt.Errorf("run sub task async: delegation not permitted for role %q at depth %d", r.role, r.depth)
	}
	child, err := r.newSubRuntime(spec.Role, spec.Toolsets)
	if err != nil {
		return SubTaskHandle{}, err
	}
	subTaskID := r.nextSubTaskID(spec.ParentTaskID)
	go func() {
		bg := context.WithoutCancel(ctx)
		res, err := r.runChild(bg, child, subTaskID, spec)
		event := domain.RuntimeEvent{
			Type:      "subtask_completed",
			TaskID:    subTaskID,
			CreatedAt: time.Now(),
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
			if auditErr := r.audit.Append(bg, domain.AuditEvent{
				ID:          subTaskID + ":subtask-publish-failed",
				RequestID:   subTaskID,
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

// nextSubTaskID mints a process-unique child task id from the parent id and a
// monotonic counter, so batch and background sub-tasks never collide.
func (r *Runtime) nextSubTaskID(parentTaskID string) string {
	if parentTaskID == "" {
		parentTaskID = "task"
	}
	return fmt.Sprintf("%s:sub-%d", parentTaskID, r.subTaskSeq.Add(1))
}

// ParentTaskIDForSubTask recovers the parent task id from a sub-task id minted by
// nextSubTaskID ("<parent>:sub-<n>"). ok reports whether s carried the expected
// suffix; when false the whole string is returned so callers can still associate
// the result rather than drop it. It lets a subtask_completed consumer route a
// background result back to its parent task.
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
