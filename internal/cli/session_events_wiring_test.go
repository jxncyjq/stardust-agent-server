package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 会话事件日志的装配断言。
//
// P2 的 Task 1-4 造好了记录器、八个 record* 与三个屏障，并把它们接进 Runtime.RunTask。
// 在这一组测试出现之前，生产上**没有任何一处**给 Config.SessionEvents 喂过 store：
// 功能整体在，一个事件都不会写。这个仓栽过两次同形的（插件工具、审批仲裁者都只接了
// per-agent resolver，默认任务路径没接），症状都是「功能整体不工作」而不是一个显眼
// 的报错。
//
// 因此每条断言的都是**装配的结果**——那个字段非 nil 且指向真正的仓储——而不是「代码
// 里有那一行」。少接一处不会让任何东西报错：任务照跑，只是那条路径的会话日志永远
// 是空的。

// TestTheSessionEventStoreReachesTheDefaultRunnerRuntimeConfig 守默认任务路径。
//
// 默认 runner 服务的是 AgentID 不在 agent 注册表里的每一个任务——GUI 自己的那条路，
// 也就是绝大多数任务。它正是本仓两次事故里被漏掉的那一侧。
func TestTheSessionEventStoreReachesTheDefaultRunnerRuntimeConfig(t *testing.T) {
	t.Parallel()

	store := stubSessionEventStore{}
	cfg := buildDefaultRunnerConfig(
		nil, nil, nil, nil,
		config.RuntimeConfig{},
		nil, nil, nil, nil,
		nil,
		nil,
		taskgate.NewTaskGate(),
		store,
		"",
	)

	if cfg.SessionEvents == nil {
		t.Fatal("buildDefaultRunnerConfig().SessionEvents = nil：默认 agent 的任务" +
			"（GUI 的主路径）一条会话事件都不会写")
	}
	if cfg.SessionEvents != port.SessionEventStore(store) {
		t.Fatalf("buildDefaultRunnerConfig().SessionEvents = %v, want 同一个 store %v",
			cfg.SessionEvents, store)
	}
}

// TestThePersistentRunPortsCarryTheSessionEventStore 守 `agent run --prompt` 与
// `agent tui` 走的那条路（app.App.RunTask）。
//
// 它与 serve 是两套装配：serve 的 store 解析在 BuildServeService 里，CLI 的在
// persistentRunPorts 里。漏掉任何一处的症状都是「这条路径跑出来的任务没有轨迹」。
func TestThePersistentRunPortsCarryTheSessionEventStore(t *testing.T) {
	t.Parallel()

	ports, closePorts, err := persistentRunPorts(context.Background(), config.Config{
		Storage: config.StorageConfig{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "agent.db")},
	})
	if err != nil {
		t.Fatalf("persistentRunPorts error = %v, want nil", err)
	}
	defer closePorts()

	if ports.sessionEvents == nil {
		t.Fatal("persistentRunPorts().sessionEvents = nil：`agent run` / `agent tui` " +
			"跑出来的任务一条会话事件都不会写")
	}
}

// TestNonPersistentRunPortsLeaveTheSessionEventStoreUnset 是上一条的对照：没有
// 持久化驱动时**必须**是 nil。
//
// 这不是兜底：Config.SessionEvents 把 nil 显式声明为一种合法部署形态（整体 no-op）。
// 断言它，是为了让「非 sqlite 驱动却塞进一个写不进去的 store」这种改动当场停下来。
func TestNonPersistentRunPortsLeaveTheSessionEventStoreUnset(t *testing.T) {
	t.Parallel()

	ports, closePorts, err := persistentRunPorts(context.Background(), config.Config{
		Storage: config.StorageConfig{Driver: "memory"},
	})
	if err != nil {
		t.Fatalf("persistentRunPorts error = %v, want nil", err)
	}
	defer closePorts()

	if ports.sessionEvents != nil {
		t.Fatalf("非持久化驱动的 sessionEvents = %v, want nil", ports.sessionEvents)
	}
}

// stubSessionEventStore 只用来证明「同一个值走通了装配」，它的方法从不被调用。
type stubSessionEventStore struct{}

func (stubSessionEventStore) Append(context.Context, string, []domain.SessionEvent) error {
	return nil
}

func (stubSessionEventStore) ReadFrom(context.Context, string, int64, int64) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (stubSessionEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}
