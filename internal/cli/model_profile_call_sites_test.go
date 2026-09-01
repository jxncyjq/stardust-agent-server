package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/agentregistry"
	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/domain"
)

// 这一组补 final-review-2.md I-3 指出的断言缺口。
//
// model_profile 的 9 个生产传递点里，4 个「构造点」（buildDefaultRunnerConfig /
// internal/runtime/agent_resolver.go / internal/app/app.go App.RunTask /
// internal/app/app.go demoRuntimeConfig）已经有断言守着（见
// model_profile_wiring_test.go、internal/app/session_events_wiring_test.go、
// internal/runtime/agent_resolver_test.go），但 5 个「调用点」——把值真正送进那些
// 构造点/直调点的实参——一个都没有断言守着。复核者把 5 处逐个改成 ""，全仓测试仍然
// 全绿：今天生产接线是对的，但没有任何东西守它，下一次重构会静悄悄把这一栏清空。
//
// 五处里：①（BuildServeService 主路径）补在 session_events_serve_test.go；
// ②（newRunCommand）补在 command_test.go；这个文件补④（runTUITask 的转发）与
// ⑤（runMentionedTUIAgentTask 的解析）。
//
// 断言的是**装配的结果**（落进 assistant/message 载荷里的那个值），不是「代码里有
// 那一行」——照 model_profile_wiring_test.go 与 internal/app 那组测试的范式。

// cliCaptureSessionEventStore 只用来把某次 runTUITask/runMentionedTUIAgentTask
// 写下的事件收住，好在事后核对 assistant/message 载荷里的 model_profile。
type cliCaptureSessionEventStore struct {
	mu       sync.Mutex
	sessions map[string][]domain.SessionEvent
}

func (s *cliCaptureSessionEventStore) Append(_ context.Context, sessionID string, events []domain.SessionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string][]domain.SessionEvent)
	}
	s.sessions[sessionID] = append(s.sessions[sessionID], events...)
	return nil
}

func (s *cliCaptureSessionEventStore) ReadFrom(_ context.Context, sessionID string, from int64) ([]domain.SessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.SessionEvent
	for _, e := range s.sessions[sessionID] {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *cliCaptureSessionEventStore) Load(context.Context, string) ([]domain.SessionEvent, error) {
	return nil, nil
}

func (s *cliCaptureSessionEventStore) eventsFor(sessionID string) []domain.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.SessionEvent(nil), s.sessions[sessionID]...)
}

// assistantMessageModelProfiles 抽出某条会话日志里每条 assistant/message 载荷的
// model_profile 字段。
func assistantMessageModelProfiles(t *testing.T, events []domain.SessionEvent) []string {
	t.Helper()
	var out []string
	for _, e := range events {
		if e.Type != domain.SessionEventAssistantMessage {
			continue
		}
		var payload struct {
			ModelProfile string `json:"model_profile"`
		}
		if err := json.Unmarshal(e.Data, &payload); err != nil {
			t.Fatalf("unmarshal assistant/message: %v", err)
		}
		out = append(out, payload.ModelProfile)
	}
	return out
}

// TestRunTUITaskCarriesItsModelProfileIntoTheSessionEventLog 守调用点④：runTUITask
// 把 cfg.ModelProfile 原样转发进 app.RunTaskOptions.ModelProfile（command.go:582，
// 「与上面 maasClientFromConfig 选客户端同源的档位名」那一行）。
func TestRunTUITaskCarriesItsModelProfileIntoTheSessionEventLog(t *testing.T) {
	t.Parallel()

	store := &cliCaptureSessionEventStore{}
	maas := &cliCaptureMaas{response: "已完成"}
	result, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:        config.Config{Runtime: config.RuntimeConfig{MaxToolRounds: 1}},
		Prompt:        "介绍一下这个运行时",
		DefaultMaas:   maas,
		SessionEvents: store,
		ModelProfile:  "tui-deep",
	})
	if err != nil {
		t.Fatalf("runTUITask() error = %v, want nil", err)
	}

	profiles := assistantMessageModelProfiles(t, store.eventsFor(result.TaskID))
	if len(profiles) == 0 {
		t.Fatal("这次运行一条 assistant/message 都没写：断言的前提不成立")
	}
	for _, got := range profiles {
		if got != "tui-deep" {
			t.Errorf("assistant/message 的 model_profile = %q, want %q："+
				"runTUITask 没有把 cfg.ModelProfile 转发进 RunTaskOptions", got, "tui-deep")
		}
	}
}

// TestRunMentionedTUIAgentTaskCarriesItsModelProfileIntoTheSessionEventLog 守调用点
// ⑤：runMentionedTUIAgentTask 按被 @提及 的 agent 自己的档位解 ModelProfile
// （command.go:677，「@提及路径的客户端是……完全不看 --maas-url，所以这里直接按那个
// 档位解，与它同源」）。
//
// 根配置的 DefaultProfile 与 agent 自己的档位刻意取不同名字：这样如果这处回归成
// 「解析用了根默认档位而不是 agent 自己的档位」，而不只是「传空」，这条断言也能抓到。
func TestRunMentionedTUIAgentTaskCarriesItsModelProfileIntoTheSessionEventLog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEChoice(t, w, map[string]any{"content": "mention result"})
	}))
	t.Cleanup(server.Close)

	cfg := config.Config{
		Maas: config.MaasConfig{
			DefaultProfile: "root-default-profile",
			Profiles: map[string]config.MaasProfile{
				"review": {BaseURL: server.URL, Model: "deepseek-reasoner"},
			},
		},
		Runtime: config.RuntimeConfig{MaxToolRounds: 1},
	}
	registry := agentregistry.New(map[string]agentregistry.AgentConfig{
		"researcher": {
			ID:          "researcher",
			Role:        "researcher",
			MaasProfile: "review",
		},
	})
	store := &cliCaptureSessionEventStore{}

	result, err := runTUITask(context.Background(), app.New(), tuiTaskRunConfig{
		Config:        cfg,
		Registry:      registry,
		Prompt:        "@researcher 调研一下当前实现",
		SessionEvents: store,
	})
	if err != nil {
		t.Fatalf("runTUITask(@researcher) error = %v, want nil", err)
	}

	profiles := assistantMessageModelProfiles(t, store.eventsFor(result.TaskID))
	if len(profiles) == 0 {
		t.Fatal("这次运行一条 assistant/message 都没写：断言的前提不成立")
	}
	for _, got := range profiles {
		if got != "review" {
			t.Errorf("assistant/message 的 model_profile = %q, want %q："+
				"runMentionedTUIAgentTask 没有按 @researcher 自己的档位解 ModelProfile",
				got, "review")
		}
	}
}
