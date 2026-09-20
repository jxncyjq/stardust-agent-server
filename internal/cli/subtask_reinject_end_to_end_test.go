package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	agentruntime "github.com/stardust/legion-agent/internal/runtime"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// reinjectEndToEndMaas 是一个只会给一句话的假模型：这条测试要的是委派链路，不是推理。
type reinjectEndToEndMaas struct{ summary string }

func (m *reinjectEndToEndMaas) Generate(ctx context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	return port.InferenceResponse{Text: m.summary}, nil
}

// TestBackgroundSubTaskResultReachesItsParent 把发布端与回注端接在同一条事件总线上，
// 端到端地钉死「后台子任务的结果确实回到了父任务那条线程」。
//
// 两端各自的单元测试都够不着这件事：运行时那边只知道自己发了一条事件，回注这边的用例
// 是自己手写事件喂进去的。真正会坏掉的是中间那一格——RunSubTaskAsync 忘了往事件上填
// ParentTaskID，于是子任务 id（UUID）里再也解析不出父任务，父任务永远等不到结果。这条
// 测试顺带断言那个 id 确实是 UUID、确实不是老形态，好让「是字段送到的、不是老解析碰巧
// 蒙对的」这件事没有第二种解释。
func TestBackgroundSubTaskResultReachesItsParent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const parentTaskID = "task-parent-end-to-end"
	events := adapter.NewMemoryEventBus()
	runtime := agentruntime.NewRuntime(agentruntime.Config{
		Gate:   taskgate.NewTaskGate(),
		Maas:   &reinjectEndToEndMaas{summary: "后台子任务的结论"},
		Events: events,
	})

	handle, err := runtime.RunSubTaskAsync(ctx, agentruntime.SubTaskSpec{
		ParentTaskID: parentTaskID,
		Goal:         "去查一下",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync error = %v, want nil", err)
	}
	if _, err := uuid.Parse(handle.TaskID); err != nil {
		t.Fatalf("子任务 id %q 不是 UUID（%v）：这条测试要守的前提不成立了", handle.TaskID, err)
	}
	if _, ok := agentruntime.ParentTaskIDForSubTask(handle.TaskID); ok {
		t.Fatalf("子任务 id %q 还是老形态：这条测试就不再证明「父任务是字段送到的」了", handle.TaskID)
	}

	// 轮数有字面上界，绝不把被测功能当作唯一终止条件。
	const maxPolls = 400 // 400 × 10ms = 4s 上界
	var published bool
	for i := 0; i < maxPolls && !published; i++ {
		all, err := events.Events()
		if err != nil {
			t.Fatalf("Events error = %v, want nil", err)
		}
		for _, e := range all {
			if e.Type == "subtask_completed" && e.TaskID == handle.TaskID {
				published = true
				break
			}
		}
		if !published {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !published {
		t.Fatal("等不到 subtask_completed 事件：后台子任务没有把结果发出来")
	}

	store := newFakeMessageStore()
	if err := newSubtaskReinjectionJob(events, store)(ctx); err != nil {
		t.Fatalf("reinjection job error = %v, want nil", err)
	}
	msgs, err := store.ListAgentMessages(ctx, domain.AgentMessageQuery{TaskID: parentTaskID})
	if err != nil {
		t.Fatalf("ListAgentMessages error = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("回注到父任务 %q 的消息 = %d 条, want 1：结果没有回到父任务那条线程",
			parentTaskID, len(msgs))
	}
	if !strings.Contains(msgs[0].Summary, "后台子任务的结论") {
		t.Errorf("回注的摘要 = %q, want 子任务的结论", msgs[0].Summary)
	}
}
