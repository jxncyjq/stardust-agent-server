package runtime

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// TestTheSessionEventStoreReachesEveryPerAgentRuntime 断言的是**装配的结果**：
// resolver 拿到的 store 真的出现在它建出来的每个 *Runtime 上，而不是「代码里有那
// 一行」。
//
// 这个仓栽过两次同形的：插件工具与审批仲裁者都只接了 per-agent resolver，默认任务
// 路径没接。这一条守的是它的镜像——只接默认路径、per-agent 路径落空。两种漏法的
// 症状一样：日志里出现一段空洞，而空洞与「这段时间什么都没发生」在数据上无法区分，
// 不会有任何报错。
func TestTheSessionEventStoreReachesEveryPerAgentRuntime(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	resolver := NewAgentRuntimeResolver(AgentRuntimeResolverConfig{
		Gate: taskgate.NewTaskGate(),
		Registry: agentregistry.New(map[string]agentregistry.AgentConfig{
			"researcher": {ID: "agent-researcher", Role: "researcher", MaasProfile: "deep"},
		}),
		RootConfig: config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}},
		Audit:      adapter.NewMemoryAuditLog(),
		Events:     adapter.NewMemoryEventBus(),
		MaasFactory: func(string) (MaasRunnerFactoryResult, error) {
			return MaasRunnerFactoryResult{Client: readFileThenAnswerMaas()}, nil
		},
		SessionEvents: store,
	})

	_, runner, ok, err := resolver.ResolveTaskRunner(context.Background(), domain.Task{
		ID:      "task-events",
		AgentID: "researcher",
	})
	if err != nil {
		t.Fatalf("ResolveTaskRunner error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ResolveTaskRunner ok = false, want true")
	}
	rt, isRuntime := runner.(*Runtime)
	if !isRuntime {
		t.Fatalf("runner type = %T, want *Runtime", runner)
	}
	if rt.sessionEvents == nil {
		t.Fatalf("per-agent 运行时的 sessionEvents 是 nil：resolver 配了 store，" +
			"这条路径上的任务却一个事件都不会写")
	}
	if rt.sessionEvents != port.SessionEventStore(store) {
		t.Fatalf("per-agent 运行时的 sessionEvents = %p，want 同一个 store %p", rt.sessionEvents, store)
	}
}
