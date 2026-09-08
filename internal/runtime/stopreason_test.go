package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 模型第一轮就给最终答案，没有任何工具调用：正常结束。
func TestStopReasonCompletedWhenTheModelAnswersWithoutTools(t *testing.T) {
	t.Parallel()
	maas := &recordingRoundsMaas{responses: []port.InferenceResponse{{Text: "done"}}}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: 4})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "answer"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonCompleted {
		t.Errorf("run.StopReason = %q, want %q: the model answered with no pending tool calls",
			run.StopReason, domain.StopReasonCompleted)
	}
}

// 每轮都要求一个不同的工具调用，轮数先耗尽：max_rounds。
// 这条路径今天在代码里没有任何痕迹，是纯新增。
func TestStopReasonMaxRoundsWhenTheRoundBudgetRunsOut(t *testing.T) {
	t.Parallel()
	const rounds = 3
	responses := make([]port.InferenceResponse, 0, rounds+2)
	for i := range rounds + 2 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	maas := &recordingRoundsMaas{responses: responses}
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: rounds})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "keep reading"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonMaxRounds {
		t.Errorf("run.StopReason = %q, want %q: the loop exited because round %d reached the limit",
			run.StopReason, domain.StopReasonMaxRounds, rounds)
	}
}

// 模型死抠同一个调用：重复守卫先于轮数耗尽切断循环。
func TestStopReasonRepeatLoopBrokenWhenTheModelRepeatsOneCall(t *testing.T) {
	t.Parallel()
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	// MaxToolRounds 没有照抄 brief 原稿的 0：NewRuntime 的 normalizeMaxToolRounds
	// 把直接构造 Config 时的 <=0 规范化成 defaultMaxToolRounds(4)（这与
	// config.normalizeMaxToolRounds 把 <=0 当「无限」是两套不同语义，
	// 见 runtime.go 该函数的文档注释），4 轮不足以让 repeatAbortCount(6) 触发——
	// 循环会先撞轮数预算得到 max_rounds，而不是这里要测的 repeat_loop_broken。
	// 给足穿过重复守卫所需的轮数，让熔断路径先于轮数预算命中。
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: repeatAbortStreak + 2})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonRepeatLoopBroken {
		t.Errorf("run.StopReason = %q, want %q: the repeat guard cut the loop",
			run.StopReason, domain.StopReasonRepeatLoopBroken)
	}
}

// 同一个工具名、每轮不同参数：名字级熔断（toolLoopCap）先于重复守卫命中。
func TestStopReasonToolLoopCapWhenOneToolNameExhaustsItsAllowance(t *testing.T) {
	t.Parallel()
	responses := make([]port.InferenceResponse, 0, toolLoopCap+2)
	for i := range toolLoopCap + 2 {
		args, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal(round index) error = %v, want nil", err)
		}
		responses = append(responses, port.InferenceResponse{ToolCalls: []domain.ToolCall{{
			ID:        "c" + string(args),
			Name:      "read_file",
			Arguments: map[string]string{"path": "file-" + string(args) + ".txt"},
		}}})
	}
	maas := &recordingRoundsMaas{responses: responses}
	// MaxToolRounds 同样没有照抄 brief 原稿的 0，理由与上一个测试相同：直接构造
	// Config 时 <=0 被 normalizeMaxToolRounds 规范化成 defaultMaxToolRounds(4)，
	// 远不够让 toolLoopCap(30) 触发。给足 len(responses) 那么多轮，让名字级熔断
	// 先于轮数预算命中。
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: len(responses)})

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many files"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if run.StopReason != domain.StopReasonToolLoopCap {
		t.Errorf("run.StopReason = %q, want %q: read_file exhausted its per-name allowance (%d)",
			run.StopReason, domain.StopReasonToolLoopCap, toolLoopCap)
	}
}

// 零值不是一种结局，是「没人填」。成功的 TaskRun 只从 finishRun 出来，所以
// 断言放在那里；它炸掉的是「加了字段但某条路径忘了填」这个形状。
func TestFinishRunPanicsWhenNoStopReasonWasRecorded(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: &recordingRoundsMaas{
		responses: []port.InferenceResponse{{Text: "done"}},
	}, Tools: unchangingReadRegistry(t)})

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("finishRun() did not panic on an unrecorded stop reason; an empty reason must never pass for completed")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, "stop reason") {
			t.Fatalf("panic value = %v, want a message naming the missing stop reason", recovered)
		}
	}()

	//nolint:errcheck // the call panics before it can return
	_, _ = rt.finishRun(context.Background(), "req-1", domain.Agent{ID: "a"},
		domain.Task{ID: "t1"}, loopState{started: time.Now()})
}
