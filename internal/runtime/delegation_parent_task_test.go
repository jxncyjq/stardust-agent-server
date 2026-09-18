package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// delegateThenAnswerMaas 第一次推理发起一次后台 delegate_task，之后一律答文本。
//
// 子任务自己那一轮也走这里（它拿到的是第二次以后的调用），所以它直接结束，不会把
// 这条用例变成一棵无限递归的委派树。
type delegateThenAnswerMaas struct {
	mu   sync.Mutex
	n    int
	args map[string]string
}

func (m *delegateThenAnswerMaas) Generate(_ context.Context, _ port.InferenceRequest) (port.InferenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	if m.n != 1 {
		return port.InferenceResponse{Text: "OK"}, nil
	}
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{{
		ID:        "call-delegate-1",
		Name:      "delegate_task",
		Arguments: m.args,
	}}}, nil
}

// delegateRegistry 造一个真的放行 delegate_task 的注册表，好让这条工具调用真的经过
// RunTask 的派发路径（dispatchToolCall）而不是被用例直接调用。
func delegateRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"delegate_task"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
}

// TestDelegateTaskInsideARunTaskCarriesTheRunningTaskID：跑在一条真任务里的
// delegate_task，落盘的父任务 id 必须是那条任务的 id。
//
// 这条钉的是接线本身：工具层此前拿不到任务 id，只能退回工具调用 id，于是
// task_runs.parent_task_id 在生产上恒等于 "call-..."——一个根本不存在的任务。按它
// 回注结果的父任务永远等不到东西，而所有只喂字面量 ParentTaskID 的用例都会全绿。
func TestDelegateTaskInsideARunTaskCarriesTheRunningTaskID(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	model := &delegateThenAnswerMaas{args: map[string]string{"goal": "把 A 查清楚", "background": "true"}}
	registry := delegateRegistry(t)
	rt := newTaskRunRuntime(runs, func(cfg *Config) {
		cfg.Maas = model
		cfg.Tools = registry
	})
	rt.RegisterDelegateTaskTool(registry)

	if _, err := rt.RunTask(context.Background(), testAgent(), testTask("task-outer")); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	started, _ := runs.snapshot()
	var sub *domain.TaskRun
	for i := range started {
		if started[i].TaskID != "task-outer" {
			sub = &started[i]
		}
	}
	if sub == nil {
		t.Fatalf("没有落下子任务的运行记录：started = %+v", started)
	}
	if sub.ParentTaskID != "task-outer" {
		t.Errorf("子任务落盘的 parent_task_id = %q, want %q——工具层拿到的不是真正在跑的那条任务",
			sub.ParentTaskID, "task-outer")
	}
	if strings.Contains(sub.ParentTaskID, "call-") {
		t.Errorf("parent_task_id = %q：写进权威状态的是工具调用 id，不是任务 id", sub.ParentTaskID)
	}
}

// TestDelegateTaskRefusesWhenTheContextCarriesNoTaskID：ctx 里没有任务 id 就拒绝这次
// 委派，而不是就地编一个。
//
// 编一个（退回工具调用 id）会把一个错误数值写进落盘的权威状态 task_runs.parent_task_id，
// 那是本仓第 0 条铁律点名最糟的一类。
func TestDelegateTaskRefusesWhenTheContextCarriesNoTaskID(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	_, err := rt.handleDelegateTask(context.Background(), domain.ToolCall{
		ID:        "call-1",
		Arguments: map[string]string{"goal": "把 A 查清楚"},
	})
	if err == nil {
		t.Fatal("ctx 里没有任务 id，delegate_task 却被放行了")
	}
	if !strings.Contains(err.Error(), "call-1") {
		t.Errorf("错误 %v 没有说清是哪次调用被拒的", err)
	}
	if !strings.Contains(err.Error(), "task id") {
		t.Errorf("错误 %v 没有点名缺的是什么", err)
	}
	if started, _ := runs.snapshot(); len(started) != 0 {
		t.Errorf("被拒的委派仍然落了 %d 条运行记录", len(started))
	}
}

// TestDelegateTaskDoesNotReadAParentTaskIDArgument：父任务 id 只来自 ctx，模型递进来
// 的同名参数不作数。
//
// 这个参数不在 delegate_task 的 InputSchema 里，模型根本没有办法传它；代码若仍然读
// 它，就是在假装一条不存在的入口可以覆盖权威状态里的父子关系。
func TestDelegateTaskDoesNotReadAParentTaskIDArgument(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	rt := newTaskRunRuntime(runs)
	ctx := tool.WithTaskID(context.Background(), "task-real")
	if _, err := rt.handleDelegateTask(ctx, domain.ToolCall{
		ID: "call-1",
		Arguments: map[string]string{
			"goal":           "把 A 查清楚",
			"background":     "true",
			"parent_task_id": "task-forged",
		},
	}); err != nil {
		t.Fatalf("handleDelegateTask: %v", err)
	}
	started, _ := runs.snapshot()
	if len(started) != 1 {
		t.Fatalf("落盘 %d 条, want 1", len(started))
	}
	if started[0].ParentTaskID != "task-real" {
		t.Errorf("parent = %q, want task-real——模型递进来的 parent_task_id 覆盖了真任务 id",
			started[0].ParentTaskID)
	}
}
