package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskledger"
)

var taskToolIDCounter atomic.Uint64

// RegisterTaskLedgerTools adds event-backed task collaboration tools to registry.
func RegisterTaskLedgerTools(registry *Registry, ledger *taskledger.Ledger) {
	if registry == nil || ledger == nil {
		return
	}
	registry.RegisterDescriptor(createTaskDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		taskID := strings.TrimSpace(call.Arguments["task_id"])
		if taskID == "" {
			taskID = nextTaskID()
		}
		event, err := ledger.Append(ctx, taskledger.Event{
			TaskID:         taskID,
			Type:           taskledger.EventTaskCreated,
			Title:          call.Arguments["title"],
			Status:         firstTaskArgument(call.Arguments["status"], "planned"),
			Owner:          call.Arguments["owner"],
			ActorAgentID:   call.Arguments["owner"],
			Summary:        call.Arguments["summary"],
			Artifact:       call.Arguments["artifact"],
			CorrelationID:  call.ID,
			IdempotencyKey: firstTaskArgument(call.Arguments["idempotency_key"], call.ID),
		})
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("create task: %w", err)
		}
		if _, err := ledger.Rebuild(ctx); err != nil {
			return domain.ToolResult{}, fmt.Errorf("rebuild task after create: %w", err)
		}
		return taskToolResult(call.ID, fmt.Sprintf("created task %s with event %s", event.TaskID, event.EventID)), nil
	}))
	registry.RegisterDescriptor(claimTaskDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		owner := strings.TrimSpace(call.Arguments["owner"])
		event, err := ledger.Append(ctx, taskledger.Event{
			TaskID:         call.Arguments["task_id"],
			Type:           taskledger.EventTaskClaimed,
			Owner:          owner,
			ActorAgentID:   owner,
			Summary:        call.Arguments["summary"],
			CorrelationID:  call.ID,
			IdempotencyKey: firstTaskArgument(call.Arguments["idempotency_key"], call.ID),
		})
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("claim task: %w", err)
		}
		if _, err := ledger.Rebuild(ctx); err != nil {
			return domain.ToolResult{}, fmt.Errorf("rebuild task after claim: %w", err)
		}
		return taskToolResult(call.ID, fmt.Sprintf("claimed task %s with event %s", event.TaskID, event.EventID)), nil
	}))
	registry.RegisterDescriptor(updateTaskDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		event, err := ledger.Append(ctx, taskledger.Event{
			TaskID:         call.Arguments["task_id"],
			Type:           taskledger.EventTaskStatusChanged,
			Status:         call.Arguments["status"],
			ActorAgentID:   call.Arguments["actor_agent_id"],
			Summary:        call.Arguments["summary"],
			CorrelationID:  call.ID,
			IdempotencyKey: firstTaskArgument(call.Arguments["idempotency_key"], call.ID),
		})
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("update task: %w", err)
		}
		if _, err := ledger.Rebuild(ctx); err != nil {
			return domain.ToolResult{}, fmt.Errorf("rebuild task after update: %w", err)
		}
		return taskToolResult(call.ID, fmt.Sprintf("updated task %s status to %s with event %s", event.TaskID, event.Status, event.EventID)), nil
	}))
	registry.RegisterDescriptor(appendTaskMessageDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		eventType, err := taskMessageEventType(call.Arguments["type"])
		if err != nil {
			return domain.ToolResult{}, err
		}
		event, err := ledger.Append(ctx, taskledger.Event{
			TaskID:         call.Arguments["task_id"],
			Type:           eventType,
			From:           call.Arguments["from"],
			To:             call.Arguments["to"],
			ActorAgentID:   firstTaskArgument(call.Arguments["actor_agent_id"], call.Arguments["from"]),
			Summary:        call.Arguments["summary"],
			Artifact:       call.Arguments["artifact"],
			CorrelationID:  call.ID,
			IdempotencyKey: firstTaskArgument(call.Arguments["idempotency_key"], call.ID),
		})
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("append task message: %w", err)
		}
		if _, err := ledger.Rebuild(ctx); err != nil {
			return domain.ToolResult{}, fmt.Errorf("rebuild task after append message: %w", err)
		}
		return taskToolResult(call.ID, fmt.Sprintf("appended %s to task %s with event %s", event.Type, event.TaskID, event.EventID)), nil
	}))
	registry.RegisterDescriptor(readTaskDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		projection, err := ledger.Snapshot(ctx)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("read task snapshot: %w", err)
		}
		taskID := strings.TrimSpace(call.Arguments["task_id"])
		content, ok := projection.TaskMarkdown[taskID]
		if !ok {
			return domain.ToolResult{}, fmt.Errorf("read task: task %q not found", taskID)
		}
		return taskToolResult(call.ID, content), nil
	}))
	registry.RegisterDescriptor(rebuildTasksDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		projection, err := ledger.Rebuild(ctx)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("rebuild tasks: %w", err)
		}
		return taskToolResult(call.ID, fmt.Sprintf("rebuilt %s from task ledger events", pluralTaskCount(len(projection.Tasks)))), nil
	}))
}

func createTaskDescriptor() Descriptor {
	return taskDescriptor("create_task", "Create a task ledger task and rebuild task projections.", "medium", true, // writes task ledger events
		[]string{"title"}, map[string]any{
			"task_id":         taskString("Optional stable task id; generated when omitted."),
			"title":           taskString("Human-readable task title."),
			"summary":         taskString("Short task summary."),
			"status":          taskString("Initial task status. Defaults to planned."),
			"owner":           taskString("Registered agent id owning the task."),
			"artifact":        taskString("Optional workspace-local artifact path."),
			"idempotency_key": taskString("Optional duplicate protection key."),
		})
}

func claimTaskDescriptor() Descriptor {
	return taskDescriptor("claim_task", "Claim a task for a registered agent and rebuild task projections.", "medium", true, // writes task ledger events
		[]string{"task_id", "owner"}, map[string]any{
			"task_id":         taskString("Task id to claim."),
			"owner":           taskString("Registered agent id claiming the task."),
			"summary":         taskString("Optional short claim note."),
			"idempotency_key": taskString("Optional duplicate protection key."),
		})
}

func updateTaskDescriptor() Descriptor {
	return taskDescriptor("update_task", "Update task ledger status and summary.", "medium", true, // writes task ledger events
		[]string{"task_id", "status"}, map[string]any{
			"task_id":         taskString("Task id to update."),
			"status":          taskString("New status."),
			"summary":         taskString("Optional short status summary."),
			"actor_agent_id":  taskString("Registered agent id issuing the update."),
			"idempotency_key": taskString("Optional duplicate protection key."),
		})
}

func appendTaskMessageDescriptor() Descriptor {
	return taskDescriptor("append_task_message", "Append a task message, handoff, result, or review event.", "medium", true, // writes task ledger events
		[]string{"task_id", "summary"}, map[string]any{
			"task_id":         taskString("Task id to append to."),
			"type":            taskString("message, handoff, result, or review. Defaults to message."),
			"from":            taskString("Registered sender agent id."),
			"to":              taskString("Optional registered recipient agent id."),
			"actor_agent_id":  taskString("Optional registered actor agent id."),
			"summary":         taskString("Short human-readable message summary."),
			"artifact":        taskString("Optional workspace-local artifact path."),
			"idempotency_key": taskString("Optional duplicate protection key."),
		})
}

func readTaskDescriptor() Descriptor {
	return taskDescriptor("read_task", "Read a generated task detail projection from TaskLedger.", "low", false,
		[]string{"task_id"}, map[string]any{"task_id": taskString("Task id to read.")})
}

func rebuildTasksDescriptor() Descriptor {
	return taskDescriptor("rebuild_tasks", "Replay task ledger events and rebuild tasks.md projections.", "medium", true, // rewrites tasks.md index projections
		nil, map[string]any{})
}

func taskDescriptor(name, description, risk string, sensitive bool, required []string, properties map[string]any) Descriptor {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return Descriptor{
		Name:        name,
		Description: description,
		RiskLevel:   risk,
		Timeout:     5 * time.Second,
		InputSchema: schema,
		Sensitive:   sensitive,
	}
}

func taskString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func taskMessageEventType(kind string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "", "message":
		return taskledger.EventMessageAppended, nil
	case "handoff":
		return taskledger.EventHandoffAppended, nil
	case "result":
		return taskledger.EventResultAppended, nil
	case "review":
		return taskledger.EventReviewAppended, nil
	default:
		return "", fmt.Errorf("append task message: unsupported type %q", kind)
	}
}

func taskToolResult(callID, output string) domain.ToolResult {
	return domain.ToolResult{CallID: callID, Success: true, Output: output}
}

func firstTaskArgument(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nextTaskID() string {
	seq := taskToolIDCounter.Add(1)
	return "TASK-" + time.Now().Format("20060102") + "-" + strings.Repeat("0", max(0, 3-len(strconv.FormatUint(seq, 10)))) + strconv.FormatUint(seq, 10)
}

func pluralTaskCount(count int) string {
	if count == 1 {
		return "1 task"
	}
	return strconv.Itoa(count) + " tasks"
}
