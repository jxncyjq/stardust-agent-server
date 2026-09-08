package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 子代理撞了熔断，父必须看得出来 —— 裸 Summary 分不清「答完了」和「被截断了」。
func TestSubTaskResultCarriesTheChildStopReason(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
	})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil", err)
	}
	if res.StopReason == "" {
		t.Fatal("SubTaskResult.StopReason is empty: the parent cannot tell a finished child from a cut one")
	}
}

// 工具输出里也要有，single 与 batch 都要。
func TestDelegateTaskOutputCarriesTheStopReason(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	out, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID:        "call-1",
		Name:      "delegate_task",
		Arguments: map[string]string{"goal": "read it"},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask() error = %v, want nil", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out.Output), &payload); err != nil {
		t.Fatalf("Unmarshal(tool output) error = %v, want nil", err)
	}
	if payload["stop_reason"] == nil || payload["stop_reason"] == "" {
		t.Errorf("tool output = %v, want a non-empty stop_reason", payload)
	}
}

// batch 分支是独立的输出组装路径（delegateResultsView），single 模式的两条用例都不会
// 经过它：一条只测 SubTaskResult 本身，另一条只测 single 分支自己拼的 map。这条覆盖
// batch 分支，确认每一条 batch 结果也带 stop_reason。
func TestDelegateTaskBatchOutputCarriesTheStopReason(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "子任务摘要：完成"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas})
	out, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID:        "call-1",
		Name:      "delegate_task",
		Arguments: map[string]string{"tasks": `[{"goal":"a"},{"goal":"b"}]`},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask(batch) error = %v, want nil", err)
	}
	payload := decodeDelegate(t, out)
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("batch results = %v, want two", payload["results"])
	}
	for _, r := range results {
		entry, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("batch result entry = %v, want object", r)
		}
		if entry["stop_reason"] == nil || entry["stop_reason"] == "" {
			t.Errorf("batch result entry = %v, want a non-empty stop_reason", entry)
		}
	}
}
