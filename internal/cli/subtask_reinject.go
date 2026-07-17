package cli

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/task"
	"github.com/stardust/legion-agent/internal/tool"
)

// newSubtaskReinjectionJob builds a background job that reinjects completed
// background sub-tasks back to their parent. A background delegate_task returns
// immediately and its result arrives later as a "subtask_completed" runtime
// event; this job turns each such event into an AgentMessage (type=result) keyed
// to the parent task, so the parent agent surfaces it on its next round via
// read_messages(task_id=<parent>). The message id is derived from the sub-task id
// so SaveAgentMessage's upsert makes reinjection idempotent across scans. A save
// failure is returned so the scheduler logs it rather than dropping the result.
func newSubtaskReinjectionJob(events port.EventBus, store tool.AgentMessageStore) task.BackgroundJob {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if events == nil || store == nil {
			return nil
		}
		for _, event := range events.Events() {
			if event.Type != "subtask_completed" {
				continue
			}
			parentTaskID, _ := agentruntime.ParentTaskIDForSubTask(event.TaskID)
			message := domain.AgentMessage{
				ID:            event.TaskID + ":reinject",
				TaskID:        parentTaskID,
				ThreadID:      parentTaskID,
				SourceEventID: event.TaskID,
				FromAgentID:   "delegate-runtime",
				Type:          domain.AgentMessageTypeResult,
				Status:        domain.AgentMessageUnread,
				Summary:       event.Message,
				CreatedAt:     event.CreatedAt,
			}
			if err := store.SaveAgentMessage(ctx, message); err != nil {
				return fmt.Errorf("reinject subtask %q result to parent %q: %w", event.TaskID, parentTaskID, err)
			}
		}
		return nil
	}
}
