package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/testsupport"
)

// 真机守卫：BuildServeService 内部把 resolver 递给 buildDefaultRunnerConfig 的那一句
// 调用（command.go, buildDefaultRunnerConfig(...) 的最后一个实参）此前没有任何测试够
// 得着。TestBuildDefaultRunnerConfigWiresDelegationAgents
// (default_runner_config_test.go) 只是直接调 buildDefaultRunnerConfig 本身、自己喂了
// 一个假 DelegationAgents 去断言字段搬运——它验的是那个被抽出来的函数,够不到
// BuildServeService 装配处传的到底是 resolver 还是别的什么（例如 nil）。
//
// Task 2 把 agent_id 接进 validateSubTaskSpec 之后，这条差异第一次可以从外部观察到：
// 一个真的 serve、一个真的 agent 注册表（config 里的 "agents" 段）、一条不带 agent_id
// 的默认任务（因此落 defaultTaskRunner，即 buildDefaultRunnerConfig 那份配置）——任务
// 里的模型直接发起 delegate_task 并点名一个注册表里真实存在的 agent_id。若那句实参真
// 的是 resolver，delegate_task 会被放行（子任务真的跑起来）；若换成 nil，
// validateSubTaskSpec 会硬拒并在错误里说“这个部署没有配置 agents”。
//
// 两种结局都不会让任务本身失败或挂起：delegate_task 的错误只是被回灌给模型（见
// runtime.go 里“a dispatch-level Go error”那段处理），所以必须去读 session_events 里
// delegate_task 那次 tool/result 的 is_error 与 preview，而不是看任务的终态。
func TestServeDefaultAgentTaskDelegatesByNameThroughTheRealResolver(t *testing.T) {
	fixture := newDelegateResolverFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := BuildServeService(ctx, ServeOptions{
		ConfigPath: fixture.configPath,
		Addr:       "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("BuildServeService: %v", err)
	}
	serveDone := make(chan error, 1)
	serveCtx, stopServe := context.WithCancel(ctx)
	go func() { serveDone <- result.Service.Start(serveCtx) }()
	shutdown := sync.OnceFunc(func() {
		stopServe()
		<-serveDone
		result.Close()
	})
	t.Cleanup(shutdown)

	baseURL := result.BaseURL
	if baseURL == "" {
		t.Fatal("ServeResult.BaseURL 是空的：没法向这台 serve 提交任务")
	}
	addr := strings.TrimPrefix(baseURL, "http://")
	if err := waitForServeListening(addr, serveDone, serveReadyTimeout); err != nil {
		t.Fatalf("wait for serve: %v", err)
	}

	// agent_id 留空：这条任务必须落在 defaultTaskRunner 上，也就是 buildDefaultRunnerConfig
	// 那份配置——resolver 是否被真的递给它，正是这条测试要守的东西。
	const taskID = delegateResolverRootTaskID
	createTask(t, baseURL, taskID, "")
	waitForTaskDone(t, baseURL, taskID)

	shutdown()

	rows := readSessionEvents(t, fixture.dbPath, taskID)
	if len(rows) == 0 {
		t.Fatal("库里一条会话事件都没有：这条任务没有留下任何轨迹")
	}

	var found bool
	for _, row := range rows {
		if domain.SessionEventType(row.typ) != domain.SessionEventToolResult {
			continue
		}
		// 用 preview 内容识别这就是 delegate_task 的那次 tool/result：成功输出带
		// "mode":"single"，失败输出带 validateSubTaskSpec 的错误前缀，二者都只
		// 会出现在 delegate_task 的结果里。
		if strings.Contains(row.data, `mode\":\"single\"`) || strings.Contains(row.data, "validate sub task: agent_id") {
			found = true
			t.Logf("delegate_task tool/result (seq=%d): %s", row.seq, row.data)
			if strings.Contains(row.data, "validate sub task: agent_id") {
				t.Errorf("delegate_task 被拒绝了：%s\n"+
					"这说明 BuildServeService 传给 buildDefaultRunnerConfig 的 delegationAgents "+
					"实参不是那个真的 resolver（或 resolver 认不出配置里注册的 agent_id %q）——"+
					"默认 agent 的委派再也没法按名字解析。", row.data, delegateResolverAgentName)
			}
		}
	}
	if !found {
		t.Fatalf("没有找到 delegate_task 的 tool/result：这条任务没有真的调用 delegate_task，"+
			"守卫没有验到东西。offers=%v", fixture.offers.dump())
	}
	if !fixture.offers.offered(taskID, "delegate_task") {
		t.Error("默认任务没有被提供 delegate_task：这条任务没走默认 runner，判据的归属就错了")
	}

	// 被委派出去的子任务确实自己跟模型说上话了，而且它的 id 是一个新铸的 UUID。
	// 这条断言接替了原来的 strings.Contains(req.RequestID, ":sub-")：那个形态已经
	// 不再被铸出来，再按它认就是一条永不成立的判据——假模型会把子任务的请求当成
	// 根任务的，于是子任务又发起一次委派，一层套一层直到轮数耗尽，而这条测试仍然
	// 是绿的——它不再守得住任何东西。
	subTaskIDs := fixture.subTaskRequests()
	if len(subTaskIDs) == 0 {
		t.Errorf("没有任何一次推理请求来自被委派的子任务："+
			"delegate_task 被放行了，子任务却一次都没真的跑起来。offers=%v", fixture.offers.dump())
	}
	for _, id := range subTaskIDs {
		if _, err := uuid.Parse(id); err != nil {
			t.Errorf("子任务 id %q 不是 UUID（%v）：nextSubTaskID 的契约被改回去了", id, err)
		}
	}
}

// delegateResolverAgentName 是注册表里真实存在的 agent 名字，与假模型发起的
// delegate_task 调用里的 agent_id 必须是同一个字符串。
const delegateResolverAgentName = "researcher"

// delegateResolverRootTaskID 是这条测试提交的那条根任务的 id。假模型靠它区分
// 「这是根任务在说话」与「这是被委派出去的子任务在说话」，所以它必须是一个常量而
// 不是测试函数里的局部字面量。
const delegateResolverRootTaskID = "delegate-resolver-guard-task"

// delegateResolverSubTaskID 从一次推理请求里认出「这是被委派出去的子任务自己的
// 请求」，并给出那条子任务的 id。RequestID 的形态是 "<taskID>:run"（见
// runtime.RunTask）。
//
// 判据是「task id 既不是根任务、又是一个 UUID」，而不是旧的「RequestID 里带
// ":sub-"」：子任务 id 现在由 nextSubTaskID 铸成 UUID，"<父>:sub-<n>" 那个形态再也
// 不会出现。要求它是 UUID而不只是「不等于根任务」，是为了把压缩、情景蒸馏、
// coordinator 那些带着别的 RequestID 形态的请求排除在外。
func delegateResolverSubTaskID(req port.InferenceRequest) (subTaskID string, ok bool) {
	taskID := strings.TrimSuffix(req.RequestID, ":run")
	if taskID == req.RequestID || taskID == delegateResolverRootTaskID {
		return "", false
	}
	if _, err := uuid.Parse(taskID); err != nil {
		return "", false
	}
	return taskID, true
}

// delegateResolverSuccessMarker 与 delegateResolverFailureMarker 是假模型用来判断
// "delegate_task 是否已经跑过一轮"的信号：前者是 delegateJSON 成功输出里必然出现的
// 片段，后者是 validateSubTaskSpec 拒绝时错误信息的前缀。两者都只会在 delegate_task
// 的 tool/result 被渲染回下一轮提示词之后才出现在请求文本里。
const delegateResolverSuccessMarker = `"mode":"single"`
const delegateResolverFailureMarker = "validate sub task: agent_id"

// delegateResolverFixture 是这条真机守卫的全部外部依赖：一个假模型服务、一份带一个
// 具名 agent 的 agent.json、一个空的工作目录。
type delegateResolverFixture struct {
	configPath string
	dbPath     string
	offers     *serveEventsToolOffers
	subTasks   *delegateResolverSubTaskLog
}

// subTaskRequests 返回那些发出过推理请求的子任务 id（去重，按首次出现排序）。
func (f delegateResolverFixture) subTaskRequests() []string { return f.subTasks.ids() }

// delegateResolverSubTaskLog 记下哪些子任务真的向假模型发过请求。假模型在 httptest
// 的 goroutine 里写、测试主 goroutine 读，所以带锁。
type delegateResolverSubTaskLog struct {
	mu    sync.Mutex
	seen  map[string]bool
	order []string
}

func (l *delegateResolverSubTaskLog) record(subTaskID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]bool)
	}
	if l.seen[subTaskID] {
		return
	}
	l.seen[subTaskID] = true
	l.order = append(l.order, subTaskID)
}

func (l *delegateResolverSubTaskLog) ids() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.order...)
}

func newDelegateResolverFixture(t *testing.T) delegateResolverFixture {
	t.Helper()

	workDir := t.TempDir()

	offers := &serveEventsToolOffers{}
	subTasks := &delegateResolverSubTaskLog{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		offers.record(req)
		if subTaskID, ok := delegateResolverSubTaskID(req); ok {
			subTasks.record(subTaskID)
		}
		resp := delegateResolverAnswer(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode fake model response: %v", err)
		}
	}))
	t.Cleanup(model.Close)

	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "agent.json")
	// 一个具名 agent，只是为了让 resolver 的底层注册表里存在这个名字——这条测试的
	// 任务本身不带 agent_id，不会走 per-agent 那条路径。
	agentPath := filepath.Join(configDir, "researcher.json")
	if err := os.WriteFile(agentPath, []byte(`{"id":"agent-researcher","role":"developer"}`), 0o600); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	body := fmt.Sprintf(`{
  "storage": {"driver": "sqlite", "path": %s},
  "context_files": {"root": %s},
  "maas": {"base_url": %s},
  "agents": {%s: %s},
  "runtime": {"max_tool_rounds": 4, "lazy_tools": false}
}`, jsonString(dbPath), jsonString(workDir), jsonString(model.URL),
		jsonString(delegateResolverAgentName), jsonString(agentPath))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return delegateResolverFixture{configPath: configPath, dbPath: dbPath, offers: offers, subTasks: subTasks}
}

// delegateResolverAnswer 决定假模型这一次怎么答——只看它被展示了什么：
//
//   - 没有工具可用：直接给一段文本（收尾的无工具请求、情景蒸馏、上下文压缩等）。
//   - 请求文本里已经能看到 delegate_task 跑过一轮的信号（成功或失败的标记）：给最终
//     答案，让任务收尾。
//   - RequestID 属于一条 UUID 形态的、不是根任务的 task：这是被点名的 agent 派生
//     出的子任务自己的请求，子任务直接给出最终答案，不再嵌套委派。
//   - 否则：这是根任务的第一次请求，发起一次点名 agent_id 的 delegate_task 调用。
func delegateResolverAnswer(req port.InferenceRequest) port.InferenceResponse {
	text := testsupport.RequestText(req)
	if len(req.Tools) == 0 ||
		strings.Contains(text, delegateResolverSuccessMarker) ||
		strings.Contains(text, delegateResolverFailureMarker) {
		return port.InferenceResponse{Text: "已完成。", PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}
	}
	if _, ok := delegateResolverSubTaskID(req); ok {
		return port.InferenceResponse{Text: "子任务完成。", PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}
	}
	return port.InferenceResponse{
		ToolCalls: []domain.ToolCall{{
			ID:   "call-delegate-resolver-1",
			Name: "delegate_task",
			Arguments: map[string]string{
				"goal":     "调研一下缓存策略",
				"agent_id": delegateResolverAgentName,
			},
		}},
		PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10,
	}
}
