package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/tool"
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

// validateSubTaskSpec 的文档写明"a nil registry exposes nothing, so it rejects
// every requested name"，但这条分支此前没有任何用例守着：把守卫从
// `len(spec.Toolsets) > 0` 改成 `len(spec.Toolsets) > 0 && r.tools != nil`，
// go vet 干净、其余用例照样全绿——nil 注册表会悄悄放行所有请求的工具名，而不是
// 文档承诺的"拒绝每一个"。这条用例专守 r.tools 为 nil 这一路径。
func TestRunSubTaskRefusesEveryToolsetNameWhenRegistryIsNil(t *testing.T) {
	t.Parallel()
	// 故意不传 Tools：r.tools 保持 nil。也不传 Maas——校验在触碰 maas 之前就必须
	// 拒绝，走不到需要它的那一步。
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate()})
	_, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"read_file"},
	})
	if err == nil {
		t.Fatal("RunSubTask() error = nil with a nil tool registry, want a refusal: a nil registry exposes nothing")
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Errorf("error = %v, want it to name the tool it does not recognise", err)
	}
}

// 三个入口共用同一个判定：async 这条路也必须拒。
func TestRunSubTaskAsyncRefusesAToolsetNameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})
	_, err := parent.RunSubTaskAsync(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"raed_file"},
	})
	if err == nil {
		t.Fatal("RunSubTaskAsync() error = nil for an unknown toolset name, want a refusal")
	}
	// sync 与 batch 两条同类用例都断言错误点名了工具，async 这条也必须断，否则
	// 三个入口「错误可定位」的一致性只有两处被守住。
	if !strings.Contains(err.Error(), "raed_file") {
		t.Errorf("error = %v, want it to name the tool it does not recognise", err)
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

// plugToolRegistry 模拟插件注册表：只有它注册了 plug_tool，宿主注册表一个也没有。
func plugToolRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(
		tool.NewExecutionPolicy(tool.ExecutionPolicyConfig{AutoAllowTools: []string{"plug_tool"}}),
		tool.PermissionEnforcerFunc(func(domain.Agent, domain.ToolCall) error { return nil }),
		tool.NoopGuardrails{},
	)
	registry.RegisterDescriptor(tool.Descriptor{Name: "plug_tool", Description: "a plugin tool"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: true, Output: "plugin output"}, nil
		}))
	return registry
}

// 情形 a：工具来自被继承的注册表（生产上每条任务路径都 InheritFrom 插件注册表）。
// 它真实可调用，校验必须放行——只看本层注册的判定会把它误拒，而「这个 agent 没有
// 这个工具」是一句假话。
func TestRunSubTaskAdmitsAToolsetNameInheritedFromAnotherRegistry(t *testing.T) {
	t.Parallel()
	host := unchangingReadRegistry(t)
	host.InheritFrom(plugToolRegistry(t))
	// 前提：plug_tool 只在被继承的那层，宿主自己没有。前提不成立这条用例就没在
	// 测继承路径。
	if host.HasTool("plug_tool") {
		t.Fatal("前提不成立：plug_tool 本应只注册在被继承的注册表上")
	}
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: host})

	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "use the plugin tool",
		Toolsets:     []string{"plug_tool"},
	})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil：plug_tool 就在 Descriptors() 里，子代理拿得到也调得通", err)
	}
	if res.Summary != "ok" {
		t.Errorf("res.Summary = %q, want %q", res.Summary, "ok")
	}
	// 反面：继承进来的注册表里也没有的名字，仍然必须拒。
	if _, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "use a tool nobody registered",
		Toolsets:     []string{"plgu_tool"},
	}); err == nil {
		t.Error("RunSubTask() error = nil for a name no registry in the chain has, want a refusal")
	}
}

// 情形 b：这个 runtime 的 r.tools 本身就是 Subset 视图（嵌套委派里子 runtime 拿到
// 的正是这个），视图自己的 handlers 是空的。视图暴露的工具必须放行，被视图滤掉的
// 必须继续拒——否则修法就从「一律拒」翻成了「一律放」。
func TestRunSubTaskAdmitsAToolsetNameSeenThroughASubsetView(t *testing.T) {
	t.Parallel()
	host := unchangingReadRegistry(t)
	host.RegisterDescriptor(tool.Descriptor{Name: "write_file", Description: "write a file"},
		tool.HandlerFunc(func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return domain.ToolResult{CallID: call.ID, Success: true}, nil
		}))
	view := host.Subset("read_file")
	// 前提：视图不注册任何工具，它靠沿 parent 链解析。
	if view.HasTool("read_file") {
		t.Fatal("前提不成立：Subset 视图本应自己不注册任何工具")
	}
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: view})

	res, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "read it",
		Toolsets:     []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("RunSubTask() error = %v, want nil：read_file 就在这个视图的 Descriptors() 里", err)
	}
	if res.Summary != "ok" {
		t.Errorf("res.Summary = %q, want %q", res.Summary, "ok")
	}
	// 反面：宿主有、但被这个视图滤掉的名字，仍然必须拒。
	if _, err := parent.RunSubTask(context.Background(), SubTaskSpec{
		ParentTaskID: "t1",
		Goal:         "write it",
		Toolsets:     []string{"write_file"},
	}); err == nil {
		t.Error("RunSubTask() error = nil for a tool this view filters out, want a refusal")
	}
}

// background 的硬拒接线：纯函数 parseDelegateBool 有用例，但从工具入口喂进去这条
// 接缝之前一条用例都没有——把那个 error 吞掉，整包照样全绿。
func TestDelegateTaskRefusesAnUnparseableBackgroundValue(t *testing.T) {
	t.Parallel()
	maas := &recordingSubMaas{summary: "ok"}
	parent := NewRuntime(Config{Gate: taskgate.NewTaskGate(), Maas: maas, Tools: unchangingReadRegistry(t)})

	res, err := parent.handleDelegateTask(context.Background(), domain.ToolCall{
		ID:   "c1",
		Name: "delegate_task",
		Arguments: map[string]string{
			"goal":       "read it",
			"background": "ture",
		},
	})
	if err != nil {
		t.Fatalf("handleDelegateTask() error = %v, want nil：拼错的字面量走 ToolResult 回喂模型，不是调度层错误", err)
	}
	if res.Success {
		t.Fatal("handleDelegateTask() Success = true for background=\"ture\"：拼错的字面量被悄悄当成了前台运行")
	}
	if !strings.Contains(res.Error, "ture") {
		t.Errorf("res.Error = %q, want it to name the value it could not read", res.Error)
	}
	// 副作用数为 0：拒绝意味着一个子代理都没起。
	if got := len(maas.recorded()); got != 0 {
		t.Errorf("%d children were started, want 0", got)
	}
}
