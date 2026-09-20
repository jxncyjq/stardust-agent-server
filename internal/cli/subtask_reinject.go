package cli

import (
	"context"
	"errors"
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
//
// 一轮扫描里，「这条事件定位不了父任务」与「存不进去」被区别对待：
//
//   - 定位不了是这条事件自己的缺陷，而且是不会自愈的——runtime_events 是落盘的，同
//     一条坏事件每一次调度都会被重新读到。在它身上直接 return，排在它后面的正常事件
//     就永远轮不到，一条坏事件把整条回注通道掐断，父任务一个都等不到结果。所以跳过
//     它、把错误攒起来、扫完整轮再一并报出去：报告是响亮的（调度器每轮都记一次），
//     但不阻断别人。错误每轮重复出现不是噪音，是实情——那条结果确实还卡着；报一次
//     之后闭嘴才是把一条永远送不出去的结果藏起来。
//   - 存不进去是存储层的事，通常对这一轮的每一条事件都成立，重试下一轮才有意义，所以
//     它仍然当场中断整轮（攒下的定位错误一并带出，不丢）。
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
		var unaddressable []error
		for _, event := range published {
			if event.Type != "subtask_completed" {
				continue
			}
			parentTaskID, err := parentTaskIDOf(event)
			if err != nil {
				unaddressable = append(unaddressable, err)
				continue
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
				saveErr := fmt.Errorf("reinject subtask %q result to parent %q: %w", event.TaskID, parentTaskID, err)
				return errors.Join(append(unaddressable, saveErr)...)
			}
		}
		// errors.Join(nil...) 是 nil：一轮里没有定位不了的事件就是没有错误。
		return errors.Join(unaddressable...)
	}
}

// parentTaskIDOf 回答「这条 subtask_completed 事件的结果该送回哪个父任务」。
//
// 两条来源，顺序是有意的：事件自己带的 ParentTaskID 是今天的答案；老形态 id
// "<父>:sub-<n>" 是改造之前发布、并且已经落在 runtime_events 里的那些事件仅剩的线索
// ——这是一条显式的老数据兼容分支，不是兜底：新事件一律走字段，走到这里就说明这条
// 事件是改造之前写下的。
//
// 两条都不成立时报错。静默跳过会让父任务永远等一个已经算完的结果，而这正是「子任务
// id 改成 UUID」这件事失效时的样子：它不会自己喊出来。
func parentTaskIDOf(event domain.RuntimeEvent) (string, error) {
	if event.ParentTaskID != "" {
		return event.ParentTaskID, nil
	}
	if parent, ok := agentruntime.ParentTaskIDForSubTask(event.TaskID); ok {
		return parent, nil
	}
	return "", fmt.Errorf("reinject subtask result: event %q carries no parent task id and %q is not a "+
		"legacy sub-task id", event.TaskID, event.TaskID)
}
