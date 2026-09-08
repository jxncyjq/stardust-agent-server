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
