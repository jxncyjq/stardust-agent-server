package app

import (
	"context"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
)

// recordingSessionEventStore 记下被写入的会话与事件类型。
//
// 它足够真：ReadFrom 回放已写入的内容，所以 newTaskRecorder 解 turn 号那段逻辑
// 真的会跑，而不是被一个恒返回 nil 的假 store 短路。
type recordingSessionEventStore struct {
	mu       sync.Mutex
	sessions map[string][]domain.SessionEvent
}

func (s *recordingSessionEventStore) Append(_ context.Context, sessionID string, events []domain.SessionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string][]domain.SessionEvent)
	}
	s.sessions[sessionID] = append(s.sessions[sessionID], events...)
	return nil
}

func (s *recordingSessionEventStore) ReadFrom(_ context.Context, sessionID string, from int64) ([]domain.SessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.SessionEvent
	for _, e := range s.sessions[sessionID] {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *recordingSessionEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (s *recordingSessionEventStore) typesFor(sessionID string) []domain.SessionEventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.SessionEventType, 0, len(s.sessions[sessionID]))
	for _, e := range s.sessions[sessionID] {
		out = append(out, e.Type)
	}
	return out
}

// TestARunTaskWithAStoreWritesItsSessionEventLog 守 `agent run --prompt` /
// `agent tui` 这条路（app.App.RunTask，五条任务入口里的「app 直调」）。
//
// 它断言的是**装配的结果**：给 RunTaskOptions 一个 store，库里就真的出现这次运行的
// 事件——而不是「Config 里有 SessionEvents 那一行」。少接这一处不会有任何报错：任务
// 照跑照返回，只是这条路径永远不留轨迹。
//
// 会话号取 task.ID（RunTaskOptions 不带 SessionID，走 eventRecorder 的 D-A 回退），
// 所以断言查的就是 TaskID 这条日志。
func TestARunTaskWithAStoreWritesItsSessionEventLog(t *testing.T) {
	t.Parallel()

	store := &recordingSessionEventStore{}
	_, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:        "app-events-task",
		Prompt:        "介绍一下这个运行时",
		Maas:          adapter.NewRecordingMaas("跑完了"),
		ToolRoot:      t.TempDir(),
		SessionEvents: store,
	})
	if err != nil {
		t.Fatalf("RunTask error = %v, want nil", err)
	}

	types := store.typesFor("app-events-task")
	if len(types) == 0 {
		t.Fatal("app.RunTask 配了会话事件 store，库里却一条事件都没有：" +
			"`agent run` / `agent tui` 跑出来的任务不留任何轨迹")
	}
	if types[0] != domain.SessionEventTurnStart {
		t.Errorf("第一条事件 = %q, want %q", types[0], domain.SessionEventTurnStart)
	}
	if last := types[len(types)-1]; last != domain.SessionEventTurnEnd {
		t.Errorf("最后一条事件 = %q, want %q", last, domain.SessionEventTurnEnd)
	}
}

// TestARunTaskWithoutAStoreStillRuns 是上一条的对照：没有配 store 是契约允许的部署
// 形态（内存驱动、测试），任务必须照常跑完，而不是因为「没有日志落点」失败。
func TestARunTaskWithoutAStoreStillRuns(t *testing.T) {
	t.Parallel()

	result, err := New().RunTask(context.Background(), RunTaskOptions{
		TaskID:   "app-no-events-task",
		Prompt:   "介绍一下这个运行时",
		Maas:     adapter.NewRecordingMaas("跑完了"),
		ToolRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("RunTask error = %v, want nil：没有配 store 是合法部署形态", err)
	}
	if result.TaskID != "app-no-events-task" {
		t.Errorf("TaskID = %q, want %q", result.TaskID, "app-no-events-task")
	}
}
