package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/approval"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/task"
)

var ErrInvalidNode = errors.New("invalid workflow node")

var taskResultPlaceholderRE = regexp.MustCompile(`\{\{tasks\.([A-Za-z0-9_.:-]+)\.result\}\}`)

type Config struct {
	Scheduler *task.Scheduler
	Approvals *approval.Service
	Events    port.EventBus
	Audit     port.AuditLog
}

type Engine struct {
	scheduler *task.Scheduler
	approvals *approval.Service
	events    port.EventBus
	audit     port.AuditLog
}

func NewEngine(cfg Config) *Engine {
	return &Engine{
		scheduler: cfg.Scheduler,
		approvals: cfg.Approvals,
		events:    cfg.Events,
		audit:     cfg.Audit,
	}
}

func (e *Engine) Execute(ctx context.Context, def Definition) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result := Result{WorkflowID: def.ID, Status: StatusRunning}
	if err := e.publish(ctx, def.ID, "workflow_started", "workflow started"); err != nil {
		return result, err
	}
	status, err := e.executeNode(ctx, def, def.Root, &result)
	result.Status = status
	if err != nil {
		result.Status = StatusFailed
		_ = e.publish(ctx, def.ID, "workflow_failed", err.Error())
		_ = e.appendAudit(ctx, def.ID, "workflow_failed")
		return result, err
	}
	switch status {
	case StatusWaitingApproval:
		if err := e.publish(ctx, def.ID, "workflow_waiting_approval", "workflow waiting approval"); err != nil {
			return result, err
		}
		if err := e.appendAudit(ctx, def.ID, "workflow_waiting_approval"); err != nil {
			return result, err
		}
	case StatusWaitingEvent:
		if err := e.publish(ctx, def.ID, "workflow_waiting_event", "workflow waiting event"); err != nil {
			return result, err
		}
		if err := e.appendAudit(ctx, def.ID, "workflow_waiting_event"); err != nil {
			return result, err
		}
	case StatusCompensated:
		if err := e.publish(ctx, def.ID, "workflow_compensated", "workflow compensated"); err != nil {
			return result, err
		}
		if err := e.appendAudit(ctx, def.ID, "workflow_compensated"); err != nil {
			return result, err
		}
	default:
		result.Status = StatusCompleted
		if err := e.publish(ctx, def.ID, "workflow_completed", "workflow completed"); err != nil {
			return result, err
		}
		if err := e.appendAudit(ctx, def.ID, "workflow_completed"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (e *Engine) executeNode(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if err := ctx.Err(); err != nil {
		return StatusFailed, err
	}
	if node.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, node.Timeout)
		defer cancel()
	}
	if err := e.publish(ctx, def.ID, "workflow_node_started", node.ID); err != nil {
		return StatusFailed, err
	}
	switch node.Kind {
	case NodeSequence:
		return e.executeSequence(ctx, def, node, result)
	case NodeParallel:
		return e.executeParallel(ctx, def, node, result)
	case NodeLoop:
		return e.executeLoop(ctx, def, node, result)
	case NodeJoin:
		return e.executeJoin(ctx, def, node, result)
	case NodeAgentTask:
		return e.executeAgentTask(ctx, def, node, result)
	case NodeApproval:
		return e.executeApproval(ctx, def, node, result)
	case NodeWaitEvent:
		return e.executeWaitEvent(ctx, def, node, result)
	case NodeSubworkflow:
		return e.executeSubworkflow(ctx, def, node, result)
	case NodeCondition:
		return e.executeCondition(ctx, def, node, result)
	case NodeErrorHandler:
		return e.executeErrorHandler(ctx, def, node, result)
	default:
		return StatusFailed, fmt.Errorf("%w: unknown kind %q", ErrInvalidNode, node.Kind)
	}
}

func (e *Engine) executeSequence(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	for _, child := range node.Children {
		status, err := e.executeNode(ctx, def, child, result)
		if err != nil || status == StatusWaitingApproval || status == StatusWaitingEvent {
			return status, err
		}
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) executeParallel(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.FailurePolicy == "" {
		node.FailurePolicy = FailurePolicyFailFast
	}
	type branchResult struct {
		status Status
		err    error
		nodes  []NodeResult
	}
	out := make(chan branchResult, len(node.Children))
	var wg sync.WaitGroup
	for _, child := range node.Children {
		wg.Go(func() {
			branch := Result{WorkflowID: def.ID}
			status, err := e.executeNode(ctx, def, child, &branch)
			out <- branchResult{status: status, err: err, nodes: branch.Nodes}
		})
	}
	wg.Wait()
	close(out)
	for branch := range out {
		result.Nodes = append(result.Nodes, branch.nodes...)
		if branch.err != nil && node.FailurePolicy == FailurePolicyFailFast {
			return StatusFailed, branch.err
		}
		if branch.status == StatusWaitingApproval || branch.status == StatusWaitingEvent {
			return branch.status, branch.err
		}
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) executeLoop(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.LoopBody == nil {
		return StatusFailed, fmt.Errorf("%w: loop requires body", ErrInvalidNode)
	}
	if node.LoopMaxIterations <= 0 {
		return StatusFailed, fmt.Errorf("%w: loop requires positive max iterations", ErrInvalidNode)
	}
	for i := 1; i <= node.LoopMaxIterations; i++ {
		iterationNode := renderIterationNode(*node.LoopBody, i)
		status, err := e.executeNode(ctx, def, iterationNode, result)
		if err != nil || status == StatusWaitingApproval || status == StatusWaitingEvent {
			return status, err
		}
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) executeJoin(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	quorum := node.Quorum
	if quorum <= 0 {
		quorum = len(node.Children)
	}
	var successes int
	var firstErr error
	for _, child := range node.Children {
		status, err := e.executeNode(ctx, def, child, result)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if status == StatusWaitingApproval || status == StatusWaitingEvent {
			return status, nil
		}
		if status == StatusCompleted {
			successes++
		}
	}
	if successes >= quorum {
		return e.completeNode(ctx, def.ID, node.ID, result)
	}
	if firstErr != nil {
		return StatusFailed, firstErr
	}
	return StatusFailed, fmt.Errorf("%w: join quorum unmet", ErrInvalidNode)
}

func (e *Engine) executeAgentTask(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.Task.ID == "" {
		return StatusFailed, fmt.Errorf("%w: agent_task requires task id", ErrInvalidNode)
	}
	if e.scheduler == nil {
		return StatusFailed, fmt.Errorf("%w: scheduler is required", ErrInvalidNode)
	}
	input := e.renderTaskInput(node.Task.Input)
	if err := e.scheduler.Add(ctx, domain.Task{
		ID:        node.Task.ID,
		CompanyID: node.Task.CompanyID,
		AgentID:   node.Task.AgentID,
		Status:    domain.TaskPending,
		Input:     input,
		CreatedAt: time.Now(),
	}); err != nil {
		return StatusFailed, fmt.Errorf("schedule workflow task %q: %w", node.Task.ID, err)
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) renderTaskInput(input string) string {
	if e.events == nil || input == "" {
		return input
	}
	return taskResultPlaceholderRE.ReplaceAllStringFunc(input, func(match string) string {
		parts := taskResultPlaceholderRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		if result, ok := e.latestTaskResult(parts[1]); ok {
			return result
		}
		return match
	})
}

func (e *Engine) latestTaskResult(taskID string) (string, bool) {
	if e.events == nil {
		return "", false
	}
	events := e.events.Events()
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == "task_completed" && event.TaskID == taskID {
			return event.Message, true
		}
	}
	return "", false
}

func (e *Engine) executeApproval(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if e.approvals == nil {
		return StatusFailed, fmt.Errorf("%w: approval service is required", ErrInvalidNode)
	}
	subjectID := node.Subject
	if subjectID == "" {
		subjectID = def.ID
	}
	if _, err := e.approvals.OpenTicket(ctx, approval.OpenTicketRequest{
		Type:      approval.TicketWorkflowHumanGate,
		SubjectID: subjectID,
		Reason:    node.Reason,
	}); err != nil {
		return StatusFailed, fmt.Errorf("open workflow approval ticket: %w", err)
	}
	result.Nodes = append(result.Nodes, NodeResult{NodeID: node.ID, Status: StatusWaitingApproval})
	return StatusWaitingApproval, nil
}

func (e *Engine) executeWaitEvent(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.EventType == "" {
		return StatusFailed, fmt.Errorf("%w: wait_event requires event type", ErrInvalidNode)
	}
	if e.events == nil {
		return StatusFailed, fmt.Errorf("%w: event bus is required", ErrInvalidNode)
	}
	for _, event := range e.events.Events() {
		if eventMatchesWaitNode(event, node) {
			return e.completeNode(ctx, def.ID, node.ID, result)
		}
	}
	result.Nodes = append(result.Nodes, NodeResult{NodeID: node.ID, Status: StatusWaitingEvent})
	return StatusWaitingEvent, nil
}

func (e *Engine) executeSubworkflow(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.Subworkflow == nil {
		return StatusFailed, fmt.Errorf("%w: subworkflow requires definition", ErrInvalidNode)
	}
	subResult, err := e.Execute(ctx, *node.Subworkflow)
	result.Nodes = append(result.Nodes, subResult.Nodes...)
	if err != nil {
		return StatusFailed, fmt.Errorf("execute subworkflow %q: %w", node.Subworkflow.ID, err)
	}
	if subResult.Status == StatusWaitingApproval || subResult.Status == StatusWaitingEvent {
		result.Nodes = append(result.Nodes, NodeResult{NodeID: node.ID, Status: subResult.Status})
		return subResult.Status, nil
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) executeCondition(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	next := node.Else
	if def.Variables[node.ConditionKey] == node.ConditionValue {
		next = node.Then
	}
	if next == nil {
		return e.completeNode(ctx, def.ID, node.ID, result)
	}
	status, err := e.executeNode(ctx, def, *next, result)
	if err != nil {
		return status, err
	}
	return e.completeNode(ctx, def.ID, node.ID, result)
}

func (e *Engine) executeErrorHandler(ctx context.Context, def Definition, node Node, result *Result) (Status, error) {
	if node.Try == nil {
		return StatusFailed, fmt.Errorf("%w: error_handler requires try node", ErrInvalidNode)
	}
	status, err := e.executeNode(ctx, def, *node.Try, result)
	if err == nil {
		return status, nil
	}
	if node.Handler == nil {
		return StatusFailed, err
	}
	if _, handlerErr := e.executeNode(ctx, def, *node.Handler, result); handlerErr != nil {
		return StatusFailed, handlerErr
	}
	result.Nodes = append(result.Nodes, NodeResult{NodeID: node.ID, Status: StatusCompensated})
	return StatusCompensated, nil
}

func (e *Engine) completeNode(ctx context.Context, workflowID string, nodeID string, result *Result) (Status, error) {
	result.Nodes = append(result.Nodes, NodeResult{NodeID: nodeID, Status: StatusCompleted})
	if err := e.publish(ctx, workflowID, "workflow_node_completed", nodeID); err != nil {
		return StatusFailed, err
	}
	if err := e.appendAudit(ctx, workflowID, "workflow_node_completed"); err != nil {
		return StatusFailed, err
	}
	return StatusCompleted, nil
}

func (e *Engine) publish(ctx context.Context, workflowID string, eventType string, message string) error {
	if e.events == nil {
		return nil
	}
	if err := e.events.Publish(ctx, domain.RuntimeEvent{
		Type:      eventType,
		TaskID:    workflowID,
		Message:   message,
		CreatedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("publish workflow event %q: %w", eventType, err)
	}
	return nil
}

func (e *Engine) appendAudit(ctx context.Context, workflowID string, action string) error {
	if e.audit == nil {
		return nil
	}
	if err := e.audit.Append(ctx, domain.AuditEvent{
		ID:          workflowID + ":" + action + ":" + time.Now().Format("150405.000000000"),
		RequestID:   workflowID + ":workflow",
		SubjectType: "workflow",
		SubjectID:   workflowID,
		Action:      action,
		Hash:        "memory",
		CreatedAt:   time.Now(),
	}); err != nil {
		return fmt.Errorf("append workflow audit %q: %w", action, err)
	}
	return nil
}

func eventMatchesWaitNode(event domain.RuntimeEvent, node Node) bool {
	if event.Type != node.EventType {
		return false
	}
	if node.EventTaskID != "" && event.TaskID != node.EventTaskID {
		return false
	}
	if node.EventMessageContains != "" && !strings.Contains(event.Message, node.EventMessageContains) {
		return false
	}
	return true
}

func renderIterationNode(node Node, iteration int) Node {
	value := fmt.Sprintf("%d", iteration)
	node.ID = strings.ReplaceAll(node.ID, "{{iteration}}", value)
	node.Subject = strings.ReplaceAll(node.Subject, "{{iteration}}", value)
	node.Reason = strings.ReplaceAll(node.Reason, "{{iteration}}", value)
	node.EventType = strings.ReplaceAll(node.EventType, "{{iteration}}", value)
	node.EventTaskID = strings.ReplaceAll(node.EventTaskID, "{{iteration}}", value)
	node.EventMessageContains = strings.ReplaceAll(node.EventMessageContains, "{{iteration}}", value)
	node.Task.ID = strings.ReplaceAll(node.Task.ID, "{{iteration}}", value)
	node.Task.CompanyID = strings.ReplaceAll(node.Task.CompanyID, "{{iteration}}", value)
	node.Task.AgentID = strings.ReplaceAll(node.Task.AgentID, "{{iteration}}", value)
	node.Task.Input = strings.ReplaceAll(node.Task.Input, "{{iteration}}", value)
	for i := range node.Children {
		node.Children[i] = renderIterationNode(node.Children[i], iteration)
	}
	if node.LoopBody != nil {
		body := renderIterationNode(*node.LoopBody, iteration)
		node.LoopBody = &body
	}
	if node.Then != nil {
		thenNode := renderIterationNode(*node.Then, iteration)
		node.Then = &thenNode
	}
	if node.Else != nil {
		elseNode := renderIterationNode(*node.Else, iteration)
		node.Else = &elseNode
	}
	if node.Try != nil {
		tryNode := renderIterationNode(*node.Try, iteration)
		node.Try = &tryNode
	}
	if node.Handler != nil {
		handlerNode := renderIterationNode(*node.Handler, iteration)
		node.Handler = &handlerNode
	}
	return node
}
