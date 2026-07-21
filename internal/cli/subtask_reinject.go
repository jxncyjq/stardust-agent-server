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
		published, err := events.Events()
		if err != nil {
			return fmt.Errorf("read runtime events for subtask reinjection: %w", err)
		}
		for _, event := range published {
			if event.Type != "subtask_completed" {
				continue
			}
			// A subtask_completed event's TaskID is supposed to be a sub-task id.
			// When it is not, ParentTaskIDForSubTask hands back the id unchanged,
			// and carrying on with that value reinjects the result to the sub-task
			// itself: the parent's read_messages never sees it and the delegation
			// chain breaks silently, leaving the parent waiting for a reply that
			// was already filed elsewhere. Return instead — the scheduler logs it,
			// same as the save failure below.
			parentTaskID, ok := agentruntime.ParentTaskIDForSubTask(event.TaskID)
			if !ok {
				return fmt.Errorf("reinject subtask result: %q is not a sub-task id", event.TaskID)
			}
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
