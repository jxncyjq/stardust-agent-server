package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/taskgate"
)

// 收窄用的工具名拼错了，今天被 Subset 静默丢掉，子代理拿到一个能力莫名其妙的
// 工具集。必须硬拒，并说出是哪个名字。
func TestRunSubTaskRefusesAToolsetNameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	_, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"read_file", "raed_file"},
	})
	if err == nil {
		t.Fatal("RunSubTask() error = nil for an unknown toolset name, want a refusal")
	}
	if !strings.Contains(err.Error(), "raed_file") {
		t.Errorf("error = %v, want it to name the tool it does not recognise", err)
	}
}

// 三个入口共用同一个判定：async 这条路也必须拒。
func TestRunSubTaskAsyncRefusesAToolsetNameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	if _, err := parent.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"raed_file"},
	}); err == nil {
		t.Fatal("RunSubTaskAsync() error = nil for an unknown toolset name, want a refusal")
	}
}

// background 拼错了不能悄悄跑成前台。
func TestParseDelegateBoolRefusesAValueItCannotRecognise(t *testing.T) {
	t.Parallel()
	if _, err := parseDelegateBool("ture"); err == nil {
		t.Fatal("parseDelegateBool(\"ture\") error = nil, want a refusal: a typo must not silently run in the foreground")
	}
	for _, value := range []string{"", "true", "false", "1", "0", "yes", "no"} {
		if _, err := parseDelegateBool(value); err != nil {
			t.Errorf("parseDelegateBool(%q) error = %v, want nil", value, err)
		}
	}
}

// 一条非法就整批不启动 —— 断言的是副作用数为 0，不是「返回了 error」。
func TestRunSubTasksStartsNothingWhenOneEntryIsInvalid(t *testing.T) {
	t.Parallel()
	// 每个子代理各自跑 RunTask，都会调一次模型，所以 recorded() 的长度就是
	// 实际启动了几个子代理。
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	started := func() int { return len(maas.recorded()) }
	specs := []SubTaskSpec{
		{ParentTaskID: "t1", Goal: "one"},
		{ParentTaskID: "t1", Goal: "two"},
		{ParentTaskID: "t1", Goal: "three", Toolsets: []string{"raed_file"}},
		{ParentTaskID: "t1", Goal: "four"},
		{ParentTaskID: "t1", Goal: "five"},
	}

	results, err := parent.RunSubTasks(context.Background(), specs)
	if err == nil {
		t.Fatal("RunSubTasks() error = nil with an invalid entry, want the whole batch refused")
	}
	if results != nil {
		t.Errorf("RunSubTasks() results = %v, want nil: a refused batch produced no results", results)
	}
	if got := started(); got != 0 {
		t.Errorf("%d children were started, want 0: a batch with an invalid entry must start none of them", got)
	}
	if !strings.Contains(err.Error(), "raed_file") {
		t.Errorf("error = %v, want it to name the offending tool", err)
	}
}

// 全部合法时批量照旧跑完，单条失败仍然进该条的 Err（既有契约不变）。
func TestRunSubTasksStillRunsEveryValidEntry(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	started := func() int { return len(maas.recorded()) }
	specs := []SubTaskSpec{
		{ParentTaskID: "t1", Goal: "one"},
		{ParentTaskID: "t1", Goal: "two"},
	}

	results, err := parent.RunSubTasks(context.Background(), specs)
	if err != nil {
		t.Fatalf("RunSubTasks() error = %v, want nil", err)
	}
	if len(results) != 2 {
		t.Fatalf("RunSubTasks() returned %d results, want 2", len(results))
	}
	if got := started(); got != 2 {
		t.Errorf("%d children were started, want 2", got)
	}
}
