package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/testsupport"

	_ "modernc.org/sqlite"
)

// 真机验证：一台**真的 serve**（BuildServeService 的完整装配 + 真 SQLite + 真 HTTP
// 任务提交）跑完一个会调工具的任务之后，库里必须有一条完整且平衡的会话日志。
//
// 它与 session_events_wiring_test.go 里那几条是两种东西：那几条断言「这个字段被喂到
// 了」，这一条断言「整条链真的通了」——BuildServeService 的 store 解析、
// buildDefaultRunnerConfig 的转发、Runtime.RunTask 的发射、SQLite 的落盘，任何一环
// 断掉这里都会红。serve 装配里那句 `sessionEvents = repo` 没有别的测试能守住它。
//
// 假模型**按请求内容作答**，绝不按调用次数：按次数的假模型会把「工具不存在」之类的
// 错误路径伪装成成功路径（P2 的 Task 4 就这么假绿过一次——夹具注册 read_file、假模型
// 却请求 lookup，那条「平衡日志」测试跑的其实是 tool-not-found）。这里请求的
// read_file 是默认 runner 注册表里真实存在、且对 developer 角色自动放行的工具。

// serveEventsFixture 是这次真机验证的全部外部依赖：一个假模型服务、一份 agent.json、
// 一个工作目录。
type serveEventsFixture struct {
	configPath string
	dbPath     string
	workDir    string
	// offers 记下每个任务的推理请求被提供了哪些工具。
	//
	// 它存在是为了排除一种**会让这条测试假绿**的可能：具名 agent 的配置若没被
	// registry 认出来，ResolveTaskRunner 返回 ok=false，那条任务就悄悄落回默认
	// runner——日志照样完整平衡，per-agent 那条腿却根本没被走到。
	offers *serveEventsToolOffers
	// bigFileRunes 是被读的那个文件的 rune 数，远大于 maxEventPreviewRunes(2000)，
	// 好让「preview 是截断过的，不是工具输出全文」这条判据可被观测。
	bigFileRunes int
}

// newServeEventsFixture 起假模型、写配置、造一个足够大的待读文件。
func newServeEventsFixture(t *testing.T) serveEventsFixture {
	t.Helper()

	workDir := t.TempDir()
	// 每行都不同，避免任何形式的去重把文件压小；总量远超 2000 rune。
	var big bytes.Buffer
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&big, "第 %03d 行：缓存命中率与预取窗口的关系笔记。\n", i)
	}
	notes := big.String()
	if err := os.WriteFile(filepath.Join(workDir, "notes.md"), []byte(notes), 0o600); err != nil {
		t.Fatalf("write notes.md: %v", err)
	}

	offers := &serveEventsToolOffers{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req port.InferenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		offers.record(req)
		resp := answerByContent(req)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode fake model response: %v", err)
		}
	}))
	t.Cleanup(model.Close)

	dbPath := filepath.Join(t.TempDir(), "agent.db")
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "agent.json")
	// 一个具名 agent，好让第二个任务落在 **per-agent resolver** 那条路上。
	// role 必须是 developer：per-agent 注册表的权限表按 "developer:read_file"
	// 放行，换个角色这个任务会被拒，而那验的就不是同一件事了。
	agentPath := filepath.Join(configDir, "researcher.json")
	if err := os.WriteFile(agentPath, []byte(`{"id":"agent-researcher","role":"developer"}`), 0o600); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	body := fmt.Sprintf(`{
  "storage": {"driver": "sqlite", "path": %s},
  "context_files": {"root": %s},
  "maas": {"base_url": %s},
  "agents": {%s: %s},
  "runtime": {"max_tool_rounds": 4},
  "service": {"background_interval": "50ms"}
}`, jsonString(dbPath), jsonString(workDir), jsonString(model.URL),
		jsonString(serveEventsAgentName), jsonString(agentPath))
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return serveEventsFixture{
		configPath:   configPath,
		dbPath:       dbPath,
		workDir:      workDir,
		offers:       offers,
		bigFileRunes: len([]rune(notes)),
	}
}

// serveEventsToolOffers 按任务号记下模型这一次**够得着**哪些工具。
//
// 「够得着」有两条来源，缺一不可：懒工具模式（配置默认开着）下 req.Tools 里只有
// call_tool / load_capabilities 两个元工具，真正的工具清单在提示里的能力目录中。
// 只看 req.Tools 会得出「两条路径提供的工具一模一样」的错误结论。
type serveEventsToolOffers struct {
	mu     sync.Mutex
	byTask map[string]map[string]bool
}

// record 把一次推理请求的工具清单记到它所属的任务名下。RequestID 的形状是
// "<taskID>:run"（见 runtime.RunTask），据此归属。
func (o *serveEventsToolOffers) record(req port.InferenceRequest) {
	taskID, _, found := strings.Cut(req.RequestID, ":")
	if !found || len(req.Tools) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byTask == nil {
		o.byTask = make(map[string]map[string]bool)
	}
	names := o.byTask[taskID]
	if names == nil {
		names = make(map[string]bool)
		o.byTask[taskID] = names
	}
	for _, tool := range req.Tools {
		names[tool.Name] = true
	}
	// 懒工具模式下模型只被直接提供 call_tool/load_capabilities 两个元工具，真正的
	// 工具清单在提示里的能力目录中，所以工具名也要从提示文本里认。
	for _, candidate := range []string{"delegate_task", "session_search", "moa_consult", "read_file"} {
		if strings.Contains(testsupport.RequestText(req), candidate) {
			names[candidate] = true
		}
	}
}

// dump 把整张表打出来，作为「这条任务确实走了哪条路径」的原始证据。
func (o *serveEventsToolOffers) dump() map[string][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string][]string{}
	for task, names := range o.byTask {
		for name := range names {
			out[task] = append(out[task], name)
		}
		sort.Strings(out[task])
	}
	return out
}

// offered 报告某个任务的模型是否够得着这个工具。
func (o *serveEventsToolOffers) offered(taskID, name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.byTask[taskID][name]
}

// answerByContent 决定假模型这一次怎么答——**只看它被展示了什么**。
//
//   - 没有工具可用（收尾的无工具请求、情景蒸馏、上下文压缩）：直接给一段文本。
//   - 已经看见过 read_file 的结果：说明工具跑完了，给最终答案。
//   - 否则：请求一次 read_file。
func answerByContent(req port.InferenceRequest) port.InferenceResponse {
	const answer = "读完了：那份笔记讲的是缓存命中率与预取窗口。"
	text := testsupport.RequestText(req)
	if len(req.Tools) == 0 || strings.Contains(text, serveEventsToolMarker) {
		return port.InferenceResponse{Text: answer, PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}
	}
	return port.InferenceResponse{
		ToolCalls: []domain.ToolCall{{
			ID:        "call-notes-1",
			Name:      "read_file",
			Arguments: map[string]string{"path": "notes.md"},
		}},
		PromptTokens: 9, CompletionTokens: 5, TotalTokens: 14,
	}
}

// serveEventsToolMarker 是 read_file 结果里必然出现、而提示里不会出现的片段。
// 假模型据此判断「工具已经跑过了」。
const serveEventsToolMarker = "第 000 行：缓存命中率与预取窗口的关系笔记。"

// serveEventsAgentName 是注册表里那个具名 agent 的键。任务的 agent_id 用它，
// 才会被 AgentRuntimeResolver 认出来并走 per-agent 路径。
const serveEventsAgentName = "researcher"

// serveEventRow 是查库查出来的一行。
type serveEventRow struct {
	seq    int64
	typ    string
	callID string
	data   string
}

func TestAServeRunWritesACompleteSessionEventLog(t *testing.T) {
	fixture := newServeEventsFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	// 停机只能跑一次：serveDone 只会收到一个值，第二次接收会永久阻塞，而 Close
	// 也不承诺可重入。测试体里主动停一次（好在停机之后查库），失败路径由 Cleanup
	// 兜住同一个 once。
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

	// 两条任务，两条生产路径，一台 serve：
	//   - agent_id 为空 → 默认 runner（buildDefaultRunnerConfig 那份配置）；
	//   - agent_id = researcher → per-agent resolver。
	// 本仓两次事故都是「只接了其中一条」，所以两条都得在真机上留下日志。
	const defaultTaskID = "serve-events-default-task"
	const agentTaskID = "serve-events-agent-task"
	createTask(t, baseURL, defaultTaskID, "")
	waitForTaskDone(t, baseURL, defaultTaskID)
	createTask(t, baseURL, agentTaskID, serveEventsAgentName)
	waitForTaskDone(t, baseURL, agentTaskID)

	// serve 停下来（并 Close 仓储）之后再查库，读到的就是真正落盘的东西。
	shutdown()

	for _, probe := range []struct {
		path   string
		taskID string
	}{
		{"默认 runner", defaultTaskID},
		{"per-agent resolver", agentTaskID},
	} {
		rows := readSessionEvents(t, fixture.dbPath, probe.taskID)
		t.Logf("=== %s（session_id=%s）===", probe.path, probe.taskID)
		for _, row := range rows {
			t.Logf("seq=%d type=%-18s call_id=%s", row.seq, row.typ, row.callID)
		}
		assertBalancedSessionLog(t, rows, fixture)
	}

	// 排除「具名 agent 的任务其实悄悄落回了默认 runner」这一假绿：delegate_task 是
	// 编排者专属工具，默认 runner 注册它，per-agent 注册表**故意**不给
	// （见 AgentRuntimeResolver.ResolveTaskRunner 的注释）。两边都offered 说明
	// resolver 根本没接住这条任务，上面那半条日志验的就不是 per-agent 路径。
	t.Logf("offers = %v", fixture.offers.dump())
	if !fixture.offers.offered(defaultTaskID, "delegate_task") {
		t.Error("默认任务没有被提供 delegate_task：这条任务没走默认 runner，判据的归属就错了")
	}
	if fixture.offers.offered(agentTaskID, "delegate_task") {
		t.Error("具名 agent 的任务被提供了 delegate_task：它落回了默认 runner，" +
			"per-agent resolver 这条腿其实没有被验到")
	}
	if !fixture.offers.offered(agentTaskID, "read_file") {
		t.Error("具名 agent 的任务连 read_file 都没被提供：这次跑的不是预期的工具路径")
	}
}

// assertBalancedSessionLog 逐条核对 brief 的四条验收判据。
func assertBalancedSessionLog(t *testing.T, rows []serveEventRow, fixture serveEventsFixture) {
	t.Helper()

	if len(rows) == 0 {
		t.Fatal("库里一条会话事件都没有：serve 装配没有把事件 store 接到运行时上")
	}

	// 判据 1：seq 从 0 连续，无洞。
	for i, row := range rows {
		if row.seq != int64(i) {
			t.Fatalf("seq 不连续：第 %d 条的 seq = %d，want %d", i, row.seq, i)
		}
	}

	// 判据 2：每个 tool/call 都有同 call_id 的 tool/result。
	calls := map[string]int{}
	results := map[string]int{}
	for _, row := range rows {
		switch domain.SessionEventType(row.typ) {
		case domain.SessionEventToolCall:
			calls[row.callID]++
		case domain.SessionEventToolResult:
			results[row.callID]++
		}
	}
	if len(calls) == 0 {
		t.Fatal("日志里没有 tool/call：这次跑的不是一个会调工具的任务，" +
			"「序列平衡」这条判据实际上没有被验到")
	}
	for callID, n := range calls {
		if results[callID] != n {
			t.Errorf("call_id=%q 有 %d 条 tool/call 却有 %d 条 tool/result：日志不平衡",
				callID, n, results[callID])
		}
	}
	for callID := range results {
		if calls[callID] == 0 {
			t.Errorf("call_id=%q 有 tool/result 却没有对应的 tool/call", callID)
		}
	}

	// 判据 3：以 turn/end 收尾，且 reason 不是 interrupted。
	last := rows[len(rows)-1]
	if domain.SessionEventType(last.typ) != domain.SessionEventTurnEnd {
		t.Fatalf("最后一条事件是 %q，want %q", last.typ, domain.SessionEventTurnEnd)
	}
	var end struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(last.data), &end); err != nil {
		t.Fatalf("decode turn/end payload: %v", err)
	}
	t.Logf("turn/end payload: %s", last.data)
	if end.Reason == domain.TurnEndReasonInterrupted {
		t.Errorf("正常跑完的任务收尾 reason = %q：interrupted 只应由崩溃恢复补出来", end.Reason)
	}
	if end.Reason != domain.TurnEndReasonCompleted {
		t.Errorf("turn/end reason = %q, want %q", end.Reason, domain.TurnEndReasonCompleted)
	}

	// 判据 4：tool/result 的 preview 是截断过的，不是工具输出全文。
	sawPreview := false
	for _, row := range rows {
		if domain.SessionEventType(row.typ) != domain.SessionEventToolResult {
			continue
		}
		var payload struct {
			Preview string `json:"preview"`
			IsError bool   `json:"is_error"`
		}
		if err := json.Unmarshal([]byte(row.data), &payload); err != nil {
			t.Fatalf("decode tool/result payload at seq %d: %v", row.seq, err)
		}
		if payload.IsError {
			t.Fatalf("seq %d 的 tool/result 是错误：这次跑的不是成功路径，判据 4 验不到真东西（preview=%q）",
				row.seq, payload.Preview)
		}
		sawPreview = true
		previewRunes := len([]rune(payload.Preview))
		if previewRunes >= fixture.bigFileRunes {
			t.Errorf("preview 有 %d 个 rune，而被读的文件是 %d 个：事件里存的是工具输出全文",
				previewRunes, fixture.bigFileRunes)
		}
		runes := []rune(payload.Preview)
		t.Logf("tool/result preview: %d runes（被读文件 %d runes），尾部 = %q",
			previewRunes, fixture.bigFileRunes, string(runes[max(0, previewRunes-48):]))
	}
	if !sawPreview {
		t.Fatal("没有任何 tool/result：判据 4 没有被验到")
	}
}

// createTask 通过真的 HTTP API 提交任务。
//
// agentID 决定这条任务落在哪条生产路径上：空 → 默认 runner（绝大多数任务走的路，
// 也是本仓两次事故里被漏掉的那一侧）；注册表里的名字 → per-agent resolver。
func createTask(t *testing.T, baseURL, taskID, agentID string) {
	t.Helper()

	body := fmt.Sprintf(`{"id":%s,"company_id":"default-company","agent_id":%s,"input":"读一下 notes.md 并总结"}`,
		jsonString(taskID), jsonString(agentID))
	resp, err := http.Post(baseURL+"/v1/tasks", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/tasks: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read create-task response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/tasks status = %d, want %d, body=%s", resp.StatusCode, http.StatusCreated, payload)
	}
}

// waitForTaskDone 轮询到任务终态。轮数有字面上界，绝不把被测功能当作唯一终止条件。
func waitForTaskDone(t *testing.T, baseURL, taskID string) {
	t.Helper()

	const maxPolls = 600 // 600 × 100ms = 60s 上界
	lastStatus := ""
	for i := 0; i < maxPolls; i++ {
		resp, err := http.Get(baseURL + "/v1/tasks/" + taskID)
		if err != nil {
			t.Fatalf("GET /v1/tasks/%s: %v", taskID, err)
		}
		payload, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if err != nil {
			t.Fatalf("read task response: %v", err)
		}
		if closeErr != nil {
			t.Fatalf("close task response: %v", closeErr)
		}
		if resp.StatusCode == http.StatusOK {
			var task struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(payload, &task); err != nil {
				t.Fatalf("decode task %s: %v (body=%s)", taskID, err, payload)
			}
			lastStatus = task.Status
			if task.Status == string(domain.TaskDone) || task.Status == string(domain.TaskFailed) {
				if task.Status == string(domain.TaskFailed) {
					t.Fatalf("任务失败了：body=%s", payload)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 %d 次轮询后仍未终结（最后状态 %q）", taskID, maxPolls, lastStatus)
}

// readSessionEvents 用 brief 给的 SQL 形状把这条会话的日志读出来。
func readSessionEvents(t *testing.T, dbPath, sessionID string) []serveEventRow {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	}()

	rows, err := db.Query(
		`SELECT seq, type, COALESCE(json_extract(data,'$.call_id'), ''), data
		 FROM session_events WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatalf("query session_events: %v", err)
	}
	defer rows.Close()

	var out []serveEventRow
	for rows.Next() {
		var row serveEventRow
		if err := rows.Scan(&row.seq, &row.typ, &row.callID, &row.data); err != nil {
			t.Fatalf("scan session_events row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate session_events: %v", err)
	}
	return out
}
