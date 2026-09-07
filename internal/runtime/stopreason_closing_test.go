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

// A stop reason that is none of the three that reach the dispatch (the fourth,
// StopReasonCompleted, never does: this function is only ever called when
// calls are still pending) is a new terminal path that forgot to teach this
// switch about itself. Silently answering with an empty instruction would let
// a future stop reason slip through unexplained; it must panic instead.
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
