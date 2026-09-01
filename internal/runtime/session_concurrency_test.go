package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/storage"
	"github.com/stardust/legion-agent/internal/taskgate"
	"github.com/stardust/legion-agent/internal/testsupport"
)

// interleavingMaas 制造 C-1 需要的那个**确定的**交错顺序。
//
// 它按请求内容作答（不按调用次数）：这条夹具规矩在本仓栽过一次——一个实例服务多次
// 运行时，按次数作答的假模型会把第二条任务的第一次请求当成第一条任务的第二次请求。
//
// 交错的编排（三步，全部有字面超时上界，不拿被测功能当终止条件）：
//
//  1. A 先跑。它走到第一次模型请求时，屏障 1 已经把 A 的首批事件刷进库了 —— 这一刻
//     关闭 aFlushed。
//  2. B 的 goroutine 等 aFlushed 才提交自己的任务，所以 B 的首刷一定排在 A 的首刷
//     之后，拿到的是接着 A 的那段 seq。B 走到第一次模型请求时关闭 bFlushed。
//  3. A 的第一次模型请求在返回前等 bFlushed（上限 waitBudget），于是 A 拿到工具调用、
//     记下 tool/call、走到**屏障 2** 时，库里的 seq 已经被 B 推过去了。
//
// 没有会话锁时第 3 步必定失败，而且失败在屏障 2 —— 也就是「已经准备派发工具」的时刻。
// 有会话锁时 B 在第 2 步根本起不来，A 等到 waitBudget 超时后自己走完，B 再跑；两条
// 任务都成功。
type interleavingMaas struct {
	// waitBudget 是 A 等待 B 的上界。修好之后 B 永远不会在 A 跑完前起来，所以这段
	// 等待一定会走完；取一个小值让测试仍然跑得快。
	waitBudget time.Duration

	aFlushed chan struct{}
	bFlushed chan struct{}
	onceA    sync.Once
	onceB    sync.Once
}

func (m *interleavingMaas) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	text := testsupport.RequestText(req)
	// 第二轮：请求里已经带上了工具输出，收工。
	if len(req.Tools) == 0 || strings.Contains(text, "file contents of") {
		return port.InferenceResponse{Text: "读完了"}, nil
	}
	switch {
	case strings.Contains(text, "TASK-B"):
		m.onceB.Do(func() { close(m.bFlushed) })
	case strings.Contains(text, "TASK-A"):
		m.onceA.Do(func() {
			close(m.aFlushed)
			select {
			case <-m.bFlushed:
			case <-time.After(m.waitBudget):
			}
		})
	default:
		return port.InferenceResponse{}, fmt.Errorf("interleavingMaas: 请求里既没有 TASK-A 也没有 TASK-B：%q", text)
	}
	// 两条任务都从 provider 拿到**同一个** call_id "call_1"。这是真实 provider 的
	// 常态（spec §4.3.1 第 4 条自己就是为此写的），也是 C-1 里「两条未应答调用共用
	// 一个 call_id」的来源。
	return port.InferenceResponse{ToolCalls: []domain.ToolCall{
		{ID: "call_1", Name: "read_file", Arguments: map[string]string{"path": "notes.md"}},
	}}, nil
}

// TestTwoConcurrentTasksOnOneSessionDoNotCorruptTheLog 守 C-1。
//
// 它用的是**会校验 seq 的 store**：真的 storage.SQLiteRepository。这一点是这条测试
// 的全部要害——整套事件测试原先用的 captureEventStore 把拿到的 seq 原样收下、从不
// 校验，而真 store 校验；夹具与真实组件语义相反，于是这个失败模式对整套测试结构性
// 隐形（现在那个夹具也补上了同款校验，见 captureEventStore.Append）。
//
// 修复前它必然红：A 首刷占 0-2、B 首刷占 3-5、A 的第二次 flush 仍从 3 开始而库里已
// 走到 6 → Append 硬失败 → fail-closed 的屏障 2 把它变成**整条任务失败**。同时两条
// 任务解出同一个 turn 号（违反 spec §4.1），两条同时未应答的 tool/call 共用 call_1
// （违反 spec §4.3.1 第 4 条）。
func TestTwoConcurrentTasksOnOneSessionDoNotCorruptTheLog(t *testing.T) {
	t.Parallel()

	repo, err := storage.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	audit := adapter.NewMemoryAuditLog()
	maas := &interleavingMaas{
		waitBudget: 2 * time.Second,
		aFlushed:   make(chan struct{}),
		bFlushed:   make(chan struct{}),
	}
	rt := NewRuntime(Config{
		Gate:  taskgate.NewTaskGate(),
		Maas:  maas,
		Audit: audit,
		// 字面轮数上界：假模型即便永远不满足自己的收工条件，循环也会结束。
		MaxToolRounds: 3,
		Events:        adapter.NewMemoryEventBus(),
		Tools:         readFileRegistry(audit),
		SessionEvents: repo,
		ModelProfile:  "concurrency-test",
	})

	const session = "shared-session-c1"
	errs := make([]error, 2)
	var wg sync.WaitGroup
	run := func(i int, input string) {
		defer wg.Done()
		_, err := rt.RunTask(context.Background(), domain.Agent{ID: "a1"}, domain.Task{
			ID: fmt.Sprintf("t%d", i+1), SessionID: session, Input: input,
		})
		errs[i] = err
	}
	wg.Add(2)
	go run(0, "TASK-A 读一下那个文件")
	// B 必须等 A 的首批事件已经落盘再提交（见 interleavingMaas 的编排），否则两条
	// 任务的首刷本身就在抢同一段 seq，撞的是另一个（更早、更浅）的窗口。超时是字面
	// 上界：A 若一直没走到模型请求，这里也不会挂住。
	go func() {
		select {
		case <-maas.aFlushed:
		case <-time.After(30 * time.Second):
		}
		run(1, "TASK-B 也读一下那个文件")
	}()

	// 字面超时上界，不拿被测功能当终止条件：卡住就报出来，不是挂着。
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("两条任务 60s 没跑完：同一会话的串行化把自己锁死了")
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("任务 t%d 失败：%v\n"+
				"同一会话上的两条任务并发跑，其中一条整任务失败了——"+
				"这不是日志瑕疵，是 P2 之前能跑通的场景被改坏了（C-1）", i+1, err)
		}
	}

	events, err := repo.ReadFrom(context.Background(), session, 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("两条任务都跑完了，会话日志却是空的")
	}

	// spec §4.1：turn 每会话单调。两条任务 = 两个 turn，必须是 0 和 1。
	turns := map[int]int{}
	for _, e := range events {
		if e.Type == domain.SessionEventTurnStart {
			turns[intFieldOfEvent(t, e, "turn")]++
		}
	}
	if len(turns) != 2 || turns[0] != 1 || turns[1] != 1 {
		t.Errorf("turn/start 的 turn 号分布 = %v，want map[0:1 1:1]："+
			"两条任务共用了一个 turn 号（spec §4.1 要求 turn 每会话单调）", turns)
	}

	// spec §4.3.1 第 4 条：同一 (turn, step) 内，未应答的 tool/call 不得复用 call_id。
	// 这里两条任务本来就从 provider 拿到同一个 "call_1"，串行化之后它们落在不同的
	// turn 上，所以按 (turn, step, call_id) 计数必须都是 1。
	seen := map[string]int{}
	for _, e := range events {
		if e.Type != domain.SessionEventToolCall {
			continue
		}
		key := fmt.Sprintf("turn %d step %d call %s",
			intFieldOfEvent(t, e, "turn"), intFieldOfEvent(t, e, "step"), stringFieldOfEvent(t, e, "call_id"))
		seen[key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s 出现 %d 次：两条同时未应答的 tool/call 共用了一个 call_id"+
				"（spec §4.3.1 第 4 条），P3 的按 call_id 配对会错配", key, count)
		}
	}

	// 每条 tool/call 都必须有同 call_id 的 tool/result（spec §4.3.1 第 1 条）。
	answered := map[string]bool{}
	for _, e := range events {
		if e.Type == domain.SessionEventToolResult {
			answered[fmt.Sprintf("turn %d call %s",
				intFieldOfEvent(t, e, "turn"), stringFieldOfEvent(t, e, "call_id"))] = true
		}
	}
	for _, e := range events {
		if e.Type != domain.SessionEventToolCall {
			continue
		}
		key := fmt.Sprintf("turn %d call %s",
			intFieldOfEvent(t, e, "turn"), stringFieldOfEvent(t, e, "call_id"))
		if !answered[key] {
			t.Errorf("%s 没有对应的 tool/result", key)
		}
	}
}

// TestASessionRunLockIsNotReentrant 守嵌套自锁：委派子任务今天用的是自己的任务 ID
// 当会话号，所以永远不会撞上父任务那把锁；但这把锁不可重入，一旦将来有人给子任务塞
// 上父会话号，就会自己等自己——那是一个没有日志、没有错误的死锁。这里要求它报错。
func TestASessionRunLockIsNotReentrant(t *testing.T) {
	t.Parallel()

	held, release, err := acquireSessionRunLock(context.Background(), "nested")
	if err != nil {
		t.Fatalf("第一次取锁失败：%v", err)
	}
	defer release()

	if _, _, err := acquireSessionRunLock(held, "nested"); err == nil {
		t.Fatal("同一调用栈上第二次取同一把会话锁被放行了：那会死锁，而死锁没有任何日志")
	}
}

// TestWaitingForASessionRunLockIsCancellable 守「等锁的任务占着 worker 槽」这笔账
// 的兜底：调用方的 ctx 一取消，等待方必须立刻放手，而不是把槽位一直占着。
func TestWaitingForASessionRunLockIsCancellable(t *testing.T) {
	t.Parallel()

	_, release, err := acquireSessionRunLock(context.Background(), "cancellable")
	if err != nil {
		t.Fatalf("第一次取锁失败：%v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := acquireSessionRunLock(ctx, "cancellable"); err == nil {
		t.Fatal("ctx 已取消，等锁却还是返回了成功")
	}
}

// intFieldOfEvent / stringFieldOfEvent 从事件载荷里取字段，取不到就让测试失败。
func intFieldOfEvent(t *testing.T, e domain.SessionEvent, name string) int {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(float64)
	if !ok {
		t.Fatalf("%s 的载荷里没有数值字段 %q", e.Type, name)
	}
	return int(value)
}

func stringFieldOfEvent(t *testing.T, e domain.SessionEvent, name string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Type, err)
	}
	value, ok := payload[name].(string)
	if !ok {
		t.Fatalf("%s 的载荷里没有字符串字段 %q", e.Type, name)
	}
	return value
}
