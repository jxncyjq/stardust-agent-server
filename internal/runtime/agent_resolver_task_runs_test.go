package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// TestResolveTaskRunnerCarriesTheResolverTaskRunStore 守第四个生产装配点：
// AgentRuntimeResolver 建出来的**每一个** per-agent 运行时。
//
// serve 把一条任务派给谁，看的是 task.AgentID 在不在 agent 注册表里：在，就落这条
// 路；不在，才落默认 runner。只接默认 runner 的症状是「直发任务落了盘、具名 agent
// 的任务悄悄没落」——两种任务在库里长得一样，谁也认不出少了哪一半。
func TestResolveTaskRunnerCarriesTheResolverTaskRunStore(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	resolver := newDelegateResolver(t, func(cfg *AgentRuntimeResolverConfig) {
		cfg.TaskRuns = runs
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID: "task-1", AgentID: "researcher",
	})
	if err != nil || !ok {
		t.Fatalf("ResolveTaskRunner() ok = %v, err = %v", ok, err)
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("ResolveTaskRunner 返回的不是 *Runtime，而是 %T", runner)
	}
	if rt.taskRuns == nil {
		t.Fatal("per-agent 运行时的 taskRuns = nil：具名 agent 的任务一条运行记录都不会落盘")
	}
	if rt.taskRuns != port.TaskRunStore(runs) {
		t.Errorf("per-agent 运行时的 taskRuns = %v, want 同一个 store %v", rt.taskRuns, runs)
	}
}

// TestResolveDelegateCarriesTheResolverTaskRunStore 守同一个装配点的另一半：具名
// 委派解出来的子运行时。
//
// 它与上一条必须一起成立才有意义：派发方插开始那一行、子运行时写终态，两者不是同一个
// store 时 RunSubTaskAsync 会直接拒绝这次委派（见 delegation.go 的校验）。
func TestResolveDelegateCarriesTheResolverTaskRunStore(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	resolver := newDelegateResolver(t, func(cfg *AgentRuntimeResolverConfig) {
		cfg.TaskRuns = runs
	})

	_, child, err := resolver.ResolveDelegate(context.Background(), "researcher", DelegationContext{
		Depth: 1, MaxSpawnDepth: 3,
	})
	if err != nil {
		t.Fatalf("ResolveDelegate() error = %v, want nil", err)
	}
	if child.taskRuns != port.TaskRunStore(runs) {
		t.Errorf("子运行时的 taskRuns = %v, want 同一个 store %v", child.taskRuns, runs)
	}
}

// TestNamedBackgroundDelegationThroughTheRealResolverIsNoLongerRefused 是 W-2 的
// 端到端判据：resolver 接上 TaskRuns 之后，一次**具名**后台委派必须真的跑起来并把
// 那一行收尾，而不再被「子运行时写去了别的 store」挡回来。
//
// 这条与 delegation_persistence_test.go 里那两条的分工：那两条用假解析器
// （recordingDelegationAgents）分别钉住「不同就拒」与「相同就通」，够不到真装配；
// 这一条走真的 AgentRuntimeResolver.ResolveDelegate，所以 resolver 上少接
// TaskRuns 这一行，它会以那次拒绝的形式转红。
func TestNamedBackgroundDelegationThroughTheRealResolverIsNoLongerRefused(t *testing.T) {
	t.Parallel()

	runs := &recordingTaskRuns{}
	resolver := newDelegateResolver(t, func(cfg *AgentRuntimeResolverConfig) {
		cfg.TaskRuns = runs
	})
	dispatcher := NewRuntime(Config{
		Gate:             taskgate.NewTaskGate(),
		Maas:             adapter.NewRecordingMaas("OK"),
		Audit:            adapter.NewMemoryAuditLog(),
		Events:           adapter.NewMemoryEventBus(),
		TaskRuns:         runs,
		DelegationAgents: resolver,
		MaxSpawnDepth:    3,
	})

	handle, err := dispatcher.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "task-parent",
		Goal:         "把 A 查清楚",
		AgentID:      "researcher",
	})
	if err != nil {
		t.Fatalf("RunSubTaskAsync(具名后台委派) error = %v, want nil："+
			"resolver 没有把派发方那个 store 传给子运行时", err)
	}

	waitForFinishedRuns(t, runs, 1)
	started, finished := runs.snapshot()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("开始 %d 条 / 终态 %d 条, want 1/1", len(started), len(finished))
	}
	if finished[0].ID != started[0].ID {
		t.Errorf("终态写去了 %q，派发方开的那一行是 %q——那一行永远停在 running",
			finished[0].ID, started[0].ID)
	}
	if finished[0].TaskID != handle.TaskID {
		t.Errorf("终态的 task id = %q, handle 是 %q", finished[0].TaskID, handle.TaskID)
	}
}
