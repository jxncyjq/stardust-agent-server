package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// stubEventStore 是一个什么都不做的 store，供不关心写入结果的用例使用。
type stubEventStore struct{}

func (stubEventStore) Append(context.Context, string, []domain.SessionEvent) error { return nil }
func (stubEventStore) ReadFrom(context.Context, string, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}
func (stubEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) { return nil, nil }

// 会话号的来源有两个，且**都不能是空**：一条写不进任何会话的事件等于没记。
//
// task.SessionID 是常态（server 与 CLI 都会填）；空的情况来自单次任务与委派子任务，
// 那时用 task.ID —— 每个这样的任务自成一条短日志，轨迹一样看得到，且不需要特例分支。
func TestTheSessionIDFallsBackToTheTaskID(t *testing.T) {
	t.Parallel()

	withSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1", SessionID: "s1"})
	if got := withSession.sessionID(); got != "s1" {
		t.Errorf("sessionID() = %q, want %q", got, "s1")
	}

	withoutSession := newEventRecorder(stubEventStore{}, domain.Task{ID: "t1"})
	if got := withoutSession.sessionID(); got != "t1" {
		t.Errorf("sessionID() = %q, want the task id %q: 没有会话号的任务也要有自己的日志", got, "t1")
	}
}

// 两者都空 = 这条任务没有任何身份，写出来的事件谁也认不回去。fail-loud。
func TestARecorderWithNoIdentityIsRefused(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("既没有 SessionID 也没有 ID 的任务被接受了：写出来的事件谁也认不回去")
		}
	}()
	newEventRecorder(stubEventStore{}, domain.Task{})
}

// 没有配 store 的部署（内存后端、测试构造）不记事件。
//
// 这**不是兜底**：Config.SessionEvents 是契约里显式声明的可选项（见它的文档注释），
// 「没有配」是一种合法部署形态，与「配了但写不进去」是两回事——后者必须硬失败。
func TestNoStoreMeansNoRecording(t *testing.T) {
	t.Parallel()

	rec := newEventRecorder(nil, domain.Task{ID: "t1"})
	if rec.enabled() {
		t.Error("没有 store 却报告 enabled")
	}
}
