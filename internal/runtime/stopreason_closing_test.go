package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 撞单工具上限的任务，收尾话术必须说的是上限，不是「你在重复调用」。
func TestClosingInstructionForAToolNameCapNamesTheCap(t *testing.T) {
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
	// MaxToolRounds is len(responses), not the brief's literal 0: NewRuntime's
	// normalizeMaxToolRounds maps a directly-constructed Config's <=0 to
	// defaultMaxToolRounds(4) (see that function's doc comment in runtime.go),
	// which is far short of toolLoopCap(30). stopreason_test.go's
	// TestStopReasonToolLoopCapWhenOneToolNameExhaustsItsAllowance hit the same
	// gap and fixed it the same way, so the cap is what actually stops the loop
	// here rather than the round budget.
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: len(responses)})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many"}); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if maas.sawText("重复同一个工具调用") {
		t.Error("a per-tool-name cap was explained to the model as repeating one call; that is not what happened")
	}
	if !maas.sawText("调用次数已达上限") {
		t.Error("the model was never told which limit it hit")
	}
}

// 死抠同一个调用的任务，收尾话术说的是重复。
func TestClosingInstructionForARepeatCutNamesTheRepetition(t *testing.T) {
	t.Parallel()
	maas := &loopingMaas{call: domain.ToolCall{Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}}}
	// MaxToolRounds is repeatAbortStreak+2, not the brief's literal 0, for the
	// same reason as above: defaultMaxToolRounds(4) is far short of
	// repeatAbortStreak(8)/repeatAbortCount(6), so the round budget would win
	// before the repeat guard ever fires. stopreason_test.go's
	// TestStopReasonRepeatLoopBrokenWhenTheModelRepeatsOneCall uses the same
	// fix for the same scenario.
	rt := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t), MaxToolRounds: repeatAbortStreak + 2})

	if _, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read it"}); err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	if !maas.sawText("重复同一个工具调用") {
		t.Error("a repeat-guard cut was not explained to the model as repetition")
	}
}

// 三个被截断的原因各有一句话术，第四个 StopReasonCompleted 没有——模型是自己
// 停下的，没有什么要打断。除这四个之外的任何值，都是新的终止路径忘了教会这个
// switch；静默返回一句空话术会让它悄悄溜过去，必须 panic。
func TestClosingInstructionForStopReasonPanicsOnAnUnknownReason(t *testing.T) {
	t.Parallel()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("closingInstructionForStopReason did not panic on an unknown stop reason")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, "stop reason") {
			t.Fatalf("panic value = %v, want a message naming the missing stop reason", recovered)
		}
	}()
	_ = closingInstructionForStopReason(domain.StopReason("bogus"))
}

// 轮数用光的收尾话术必须说轮数。这一臂之前没有任何用例：把它改成另外两句里的
// 任何一句，整包仍然全绿，而在默认 4 轮预算下它恰恰是生产里最常命中的那条。
func TestClosingInstructionForARoundBudgetCutNamesTheRounds(t *testing.T) {
	t.Parallel()
	// 每轮都是不同的 path，所以重复守卫（连续 8 次相同调用）与单工具上限（30 次）
	// 都够不着，停下来的只可能是轮数预算。
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

	run, err := rt.RunTask(context.Background(), domain.Agent{ID: "a"}, domain.Task{ID: "t1", Input: "read many"})
	if err != nil {
		t.Fatalf("RunTask() error = %v, want nil", err)
	}
	// 前提：确实是轮数预算把循环停下来的，否则下面断的是别人的话术。
	if run.StopReason != domain.StopReasonMaxRounds {
		t.Fatalf("run.StopReason = %q, want %q: 这条用例要的是轮数预算耗尽这一臂", run.StopReason, domain.StopReasonMaxRounds)
	}
	if !maas.sawText("工具调用轮数已达上限") {
		t.Error("轮数预算耗尽，模型却没被告知是轮数到顶")
	}
	if maas.sawText("重复同一个工具调用") {
		t.Error("轮数预算耗尽被讲成了「你在重复同一个调用」，那件事没有发生")
	}
	if maas.sawText("单个工具的调用次数已达上限") {
		t.Error("轮数预算耗尽被讲成了单工具熔断，那件事没有发生")
	}
}
