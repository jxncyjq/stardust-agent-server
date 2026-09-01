package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// eventRecorder 把一次任务执行写成会话事件日志（spec §5）。
//
// 它是**每次 RunTask 一个**：seq 游标、缓冲区都属于这一次执行，不跨任务共享。
//
// **seq 的连续性不是由 store 保证的**：P1 的 store（internal/storage.appendLocked）
// 是**校验** seq 而不是分配 seq——首条 seq 对不上库里的 next-seq 就整批硬失败。所以
// 「同一会话上同时只有一个写入者」必须在这一层之外成立，它由 RunTask 持有的会话执行
// 锁（sessionRunLocks）提供。曾经有一版注释把这件事写成「store 保证串行化」，于是
// flush 的「首刷对齐、之后本地递增」看上去是安全的——它不安全，两条并发任务会让其中
// 一条在第二次 flush 上硬失败并连累整条任务（C-1）。这里只负责「发什么、什么时候
// 必须落盘」。
//
// 接收者契约：本类型的所有方法都假设接收者非 nil。字面 nil 接收者是编程错误，不是本
// 类型承诺处理的状态，panic 是预期行为，不需要也不应该把它做成「安全返回」。
// newEventRecorder 保证它返回的 recorder 永不是字面 nil（拿不到 session id 就直接
// panic），所以持有 recorder 的调用方必须保证字段被赋过值：某次部署没有配置 store 时，
// 正确做法是持有一个 store 字段为 nil 的 *eventRecorder（走 enabled() 判断的可选路径），
// 而不是让持有 recorder 的字段保持零值 nil。
//
// 这与 enabled() 是两回事，不要混为一谈：enabled() 处理的是「Runtime 没配 store」——
// 这是契约显式声明的可选部署形态（见 enabled() 的文档注释），不是对错误状态的兜底。
// enabled() 方法体里的 e != nil 只是让这一个方法本身能在 nil 接收者上求值，不代表
// 其余方法对 nil 接收者也是安全的：recordUserMessage/recordStepStart/
// recordAssistantMessage/recordToolCall/recordToolResult/recordTurnEnd 六个方法在
// 构造 e.append 的参数时会先经由 currentTurn()/currentStep() 直接 e.mu.Lock()，
// 在字面 nil 接收者上仍会 panic；不要把 enabled() 的 e != nil 读成「本类型对 nil
// 接收者是安全的」承诺。
type eventRecorder struct {
	store   port.SessionEventStore
	session string
	// taskID / agentID 是这次执行的任务与承接它的 agent 身份，newEventRecorder 时
	// 定死、不随后续调用变化。record* 方法把它们写进 user/message 与
	// assistant/message 的载荷，供 P3 投影出 domain.ConversationTurn.TaskID/AgentID：
	// session_turns.go 用 TaskID 滤掉任务自己的 user turn，AgentID 是 /turns 响应
	// 与 FTS5 索引都要用的字段。
	taskID  string
	agentID string

	mu      sync.Mutex
	pending []domain.SessionEvent
	nextSeq int64
	// seqKnown 表示 nextSeq 已经与库对齐过。第一次 flush 之前不知道库里走到哪，
	// 所以 seq 在 flush 时才最终确定（见 flush）。
	seqKnown bool
	turn     int
	step     int
	// issuedTurnIDs 记录这次执行里 newTurnID 已经发放过的 (turn, step, type) 坐标。
	// 只在 enabled() 时使用（懒初始化），理由见 newTurnID 里对 enabled() 分支的
	// 说明。与 pending/nextSeq/turn/step 用同一把 e.mu 保护，不新造锁。
	issuedTurnIDs map[turnIDCoordinate]bool
}

// turnIDCoordinate 是 newTurnID 用来判断一个坐标是否已经发放过的键，分量与
// newTurnID 派生 ID 字符串时用的分量一一对应——session 已经是这个 eventRecorder
// 唯一固定的一份，不需要再放进键里。
type turnIDCoordinate struct {
	turn int
	step int
	typ  domain.SessionEventType
}

// newEventRecorder 建一次任务执行的记录器。
//
// 会话号取 task.SessionID，为空时退到 task.ID（决定 D-A：单次任务与委派子任务没有
// 会话号，让它们各自成为一条短日志，比加特例分支更简单，轨迹也一样看得到）。
// 两者都空说明这条任务没有任何身份——写出来的事件谁也认不回去，直接 panic：
// 这是编程错误，不是运行期状况。
//
// task.ID 单独再拦一道，即使 SessionID 非空、会话号已经解得出来：task.ID 会被原样
// 写进 user/message 与 assistant/message 载荷的 task_id，而读侧拿它当**唯一的地址**
// ——projectTurns 用它当折叠键与 ID 词干，storage.SearchMessages 用它拼 discovery
// 命中的 turn id。缺了它写出来的是一条搜得到却回访不了的事件。
//
// 为什么必须堵在这一侧：SearchMessages 是**全库**检索，没有会话过滤，所以库里任何
// 一条会话留下一条缺 task_id 的消息事件，所有词面命中它的 discovery 查询都会整体
// fail-loud，健康会话的命中一并丢失——一条坏事件让整个部署的 session_search 失效
// （P3 Task 4 复审 Important-1）。读侧那条校验本身是对的——把「task_id 非空」当成
// 过滤条件塞进 SQL 才是静默跳过——错的是让它有机会在生产上触发。堵住这里之后，
// 读侧的校验退化成一条永不触发的深层断言。
func newEventRecorder(store port.SessionEventStore, task domain.Task) *eventRecorder {
	// 与 RunTask 取会话执行锁用的是同一个解析函数，这一点是硬要求而不是顺手复用：
	// 锁按 A 解出的键切分、日志按 B 解出的键写，两者一旦漂移，锁就守不住它要守的那条
	// 日志（C-1）。让它们共用一个函数，漂移就不可能发生。
	session := sessionKeyForTask(task)
	if session == "" {
		panic("runtime: event recorder needs a session id or a task id; a task with neither cannot own a log")
	}
	if task.ID == "" {
		panic("runtime: event recorder needs a task id; every message event carries it as the address of the " +
			"turn it belongs to, and one event without it fails every session_search discovery query in the database")
	}
	return &eventRecorder{store: store, session: session, taskID: task.ID, agentID: task.AgentID}
}

// sessionID 是这次执行写入的会话日志。
func (e *eventRecorder) sessionID() string { return e.session }

// enabled 说明这个部署是否记录会话事件。
//
// 没有配 store 是**契约允许的可选**（见 Config.SessionEvents），不是错误：内存后端与
// 大量测试构造都不配。它与「配了但写不进去」是两回事——后者由 flush 硬失败。
//
// 方法体里的 e != nil 只是让 enabled() 自己能在 nil 接收者上求值（append/flush/
// recordTurnStart/recordStepEnd 都先调它），不是本类型「nil 接收者安全」的承诺——
// 其余六个 record* 方法在构造参数时会先经由 currentTurn()/currentStep() 直接解引用
// e.mu，enabled() 的判空来不及保护它们。接收者契约见 eventRecorder 类型文档。
func (e *eventRecorder) enabled() bool { return e != nil && e.store != nil }

// eventUsage 是一次模型响应的 token 用量。
//
// 本地定义而不是复用某个领域结构：那些结构（TaskRun/RuntimeEvent/ConversationTurn）
// 各自还带着一堆与事件无关的字段，让事件记录去依赖它们会把两件事绑死。
type eventUsage struct {
	Prompt     int
	Completion int
	Cached     int
	Total      int
}

// maxEventPreviewRunes 是事件里预览文本的上限。
//
// 它守的是 spec §4.3 不变量 6 在发射侧的对应物：事件表的增长与调用次数成正比，
// 不与工具输出体积成正比。全文仍由既有的截断治理落盘（toolcache.go），事件里只留
// 这段预览。按 rune 而不是 byte 计，与该仓其余截断口径一致（中文一个字符 3 字节，
// 按 byte 截会把预览砍成三分之一）。
const maxEventPreviewRunes = 2000

// append 把一条事件放进缓冲。seq 在 flush 时统一分配（见 flush）。
//
// 记录器没启用时直接丢弃：没有配 store 是契约允许的部署形态。
func (e *eventRecorder) append(typ domain.SessionEventType, payload map[string]any) {
	if !e.enabled() {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// 载荷是本文件自己构造的 map，编不出 JSON 属编程错误。
		panic(fmt.Sprintf("runtime: marshal %s payload: %v", typ, err))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, domain.SessionEvent{Type: typ, Time: time.Now(), Data: data})
}

// recordTurnStart 记一个轮次的开始，并把 step 计数归零（spec §4.1：step 每 turn 重置）。
//
// 先过 enabled() 再碰 e.mu：目的是在「没配 store」这个契约允许的可选部署形态下，不必为
// 丢弃一次记录而白白加锁（见 enabled() 的文档注释）。这**不代表**本方法与其余六个
// record* 方法在 nil 接收者上的行为一致——那六个方法会先经由 currentTurn()/
// currentStep() 直接解引用 e.mu，在字面 nil 接收者上仍会 panic；本方法把 enabled()
// 判断放在任何解引用之前，只是这一个方法自己的写法。真正的接收者契约见 eventRecorder
// 类型文档：所有方法都假设接收者非 nil，nil 接收者是编程错误，panic 是预期行为。
func (e *eventRecorder) recordTurnStart(turn int) {
	if !e.enabled() {
		return
	}
	e.mu.Lock()
	e.turn, e.step = turn, 0
	e.mu.Unlock()
	e.append(domain.SessionEventTurnStart, map[string]any{"turn": turn})
}

// recordUserMessage 记这一轮的用户输入。
//
// content 存**全文**，不截断：对话正文是对话本体，不是工具输出。模型侧允许
// defaultMaxTurnChars = 6000 字符（session_turns.go），截到 maxEventPreviewRunes
// = 2000 会让历史对话缩到 1/3。超长单条消息撞 P1 的 64 KiB/条上限时 flush → Append
// 会 fail-loud 报错——那是正确行为，这里不为它兜底。
//
// task_id 供投影滤掉任务自己的 user turn（session_turns.go 用 turn.TaskID ==
// task.ID），同时是投影折叠这一行 turn 的键与它 id 的词干；自 Task 4 起它还是
// discovery 命中拼 turn id 的词干（storage.SearchMessages）。turn_id 标识**这一条
// 事件**，P4 轨迹按它定位；Task 4 的检索**不**按它定位（SearchMessages 连这一列
// 都不 SELECT），见 newTurnID。
//
// agent_id 与 assistant/message 一样必填：/turns 的响应与 conversation_turns_fts
// 的索引都带这个字段，退役中的 recordUserTurn（server/http.go）写的正是
// task.AgentID。P3 计划把它列为要补齐的五个字段缺口之一，Task 1 当时只补了
// assistant 一侧，缺了这里 /turns 的 user 项 agent_id 就恒为空。
func (e *eventRecorder) recordUserMessage(content string) {
	e.append(domain.SessionEventUserMessage, map[string]any{
		"turn":     e.currentTurn(),
		"turn_id":  e.newTurnID(domain.SessionEventUserMessage),
		"task_id":  e.taskID,
		"agent_id": e.agentID,
		"content":  content,
	})
}

// recordStepStart 记一次模型请求的开始。
func (e *eventRecorder) recordStepStart() {
	e.append(domain.SessionEventStepStart, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
	})
}

// recordAssistantMessage 记模型响应（含它请求的工具调用与 token 用量）。
//
// content 同 recordUserMessage：存**全文**，不截断——理由同样是「对话正文是对话
// 本体，不是工具输出」，见 recordUserMessage 的文档注释。
//
// generatedFiles 是这一步经 write_file 产出的工作区相对路径；GUI 的「对话生成
// 文件卡片」靠它渲染（server/http.go 的 generatedFilesDTO）。为 nil/空是合法的
// 可选（这一步没写文件），不是兜底。
//
// task_id / agent_id / turn_id 供投影还原 domain.ConversationTurn 的对应字段：
// turn_id 见 newTurnID 的文档注释。
//
// tool_calls 摘要数组**故意不截断**，这是权衡过的决定，不是漏掉的截断：
// spec §4.3.1 第 2 条要求 P3 投影按 call_id 配对，而这个数组正是配对时要用的清单。
// 截掉其中任意一项，恢复/投影就会缺项——那是比容量风险更重的正确性缺陷，用一个
// 换另一个不划算。arguments/preview 两处按 maxEventPreviewRunes 截断是因为
// 它们是自由文本，截了不影响可配对性；tool_calls 每项只是 call_id+name，本身已经
// 很小，风险来自「条目数」而不是「单项体积」。
//
// 真的超过 P1 的 64 KiB/事件硬上限（internal/storage.maxSessionEventDataBytes）时，
// flush → Append 会整批失败并返回包装过的错误，由 Task 4 的屏障感知并阻断执行——
// 这是 fail-loud 的正确表现，不是数据损坏（不静默丢事件、不裁剪配对信息）。
// TestRecordAssistantMessageFlushesWithManyToolCalls 用一个远超真实单步工具调用数量
// 的上界证明：在这个上界内，flush 确实能落盘、不会撞到那个上限。
func (e *eventRecorder) recordAssistantMessage(content string, calls []domain.ToolCall, usage eventUsage, profile string, generatedFiles []string) {
	names := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		names = append(names, map[string]any{"call_id": c.ID, "name": c.Name})
	}
	e.append(domain.SessionEventAssistantMessage, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"turn_id":  e.newTurnID(domain.SessionEventAssistantMessage),
		"task_id":  e.taskID,
		"agent_id": e.agentID,
		"content":  content, "tool_calls": names,
		"usage": map[string]any{
			"prompt": usage.Prompt, "completion": usage.Completion,
			"cached": usage.Cached, "total": usage.Total,
		},
		"model_profile":   profile,
		"generated_files": generatedFiles,
	})
}

// newTurnID 生成一条消息事件的稳定标识（这里的「turn」是历史遗留的叫法，指一条
// user/message 或 assistant/message 事件，不是本文件 e.turn 那个「每次 RunTask
// 一个」的会话轮次计数，两者是同名不同义的两个概念）。
//
// 它标识的是**事件**，不是 domain.ConversationTurn 的一行：P3 Task 3 复审之后，
// 投影按 (task_id, role) 折叠，ConversationTurn.ID 由 storage.projectTurns 派生成
// "<task_id>:<role>"（与退役中 server/http.go 的 recordUserTurn /
// recordAssistantTurn 逐字一致，这样 search_session 的 discovery 与 scroll（投影）
// 落在同一个 ID 空间——Task 4 之后 discovery 搜的是事件索引，命中后同样把 task_id
// 与 role 拼成这个形状）。一次带 N 轮工具的任务写出 N+1 条 assistant/message，
// 它们折成同一行 turn，各自的 turn_id 仍然互不相同——那正是 P4 轨迹定位「哪一条
// 事件」所需要的（检索不按它定位：SearchMessages 只用 task_id 与 type）。
//
// 它必须**写进事件**、而不是投影时现生成：投影每次现生成的话，同一条事件两次投影
// 出来的标识不同，任何按事件定位的消费者都会落空。
//
// 用「会话号 + turn 号 + step 号 + 事件类型」派生，不用 seq：append 把事件放进
// pending 时 seq 还没分配，要 flush 时才知道库里的 next-seq（见 flush 的文档
// 注释）；为了在这里拿到 seq 而去调 ReadFrom 或 Load 违反 spec §4.3.1 第 3 条
// （任务执行路径上不许调 Load），且每条消息一次 ReadFrom 是不必要的 O(n) 额外读。
//
// 这个组合在一次执行里稳定且唯一：
//   - user/message 每个会话轮次只记一次（RunTask 顶部一次；resume 路径不重复调
//     recordUserMessage），此时 step 恒为 recordTurnStart 刚重置过的 0；
//   - assistant/message 总是紧邻 recordStepStart 之后记，而 step 只在
//     recordStepEnd 里推进，所以同一 (turn, step) 在这次执行里只会被
//     recordAssistantMessage 用到一次；
//   - 事件类型这一段是防两者在同一 (turn, step) 上撞车的关键：user/message 与
//     一个 turn 内第一条 assistant/message 都发生在 step 0，不带类型会撞出同一
//     个 ID。
//
// 同一条事件被读多少次，它落盘时的 turn、step、类型都不变，因此这个标识在多次
// 读取之间保持一致——这正是按事件定位所需要的全部保证。
//
// # 这个派生方式为什么站得住脚，以及它现在只靠什么撑着
//
// 「(session, turn, step, type) 唯一」不是类型系统保证的，它是一条**隐式控制流
// 不变量**：本文件之外——RunTask/runToolLoop（runtime.go）——必须保证同一
// (turn, step) 在一次执行里最多被一次 recordAssistantMessage（或一次
// recordUserMessage）用到。今天这条不变量成立，论证见 P3 Task 1 复审
// （.superpowers/sdd/task-1-review.md I-1）：resume 分支手工记一次之后，
// runToolLoop 在决定是否继续循环之前先 recordStepEnd 把 step 推进，循环内两处
// 提前 break 也都在 recordStepEnd 之后才发生，所以不会有两次 recordAssistantMessage
// 落在同一个 (turn, step) 上。
//
// 但这条不变量**只被走查验证过**，不被 newTurnID 的签名或类型强制。下面的运行期
// 断言就是补这个洞：newTurnID 每发放一个坐标就记下来，同一坐标被要第二次时立刻
// panic，而不是安静地返回同一个字符串——那样的话，两行本应各自独立的事件会在
// 投影时用同一个 turn_id 互相覆盖，不会有任何测试或运行时信号提示这件事，正是
// spec 反复强调的「不报错、只是悄悄少东西」的失败形态。它守的不变量正是上一段
// 说的那条：谁在将来改动 runToolLoop（例如把 resume 分支的「手工记一次 + 继续走
// 循环」改成别的形态，或者在循环内加一条新的 recordAssistantMessage 调用点）
// 一旦破坏了它，第一次真的撞车就会在这里响亮地炸出来，而不是被投影悄悄吞掉。
//
// 只在 e.enabled()（配了 store）时才记坐标、才检查撞车：没配 store 时这条 ID
// 从不会被真正落盘或投影，根本不存在「两行事件互相覆盖」这回事——大量测试与
// 无 store 部署会反复用同样的 (turn, step) 构造 disabled recorder，那不是撞车，
// 是这个可选部署形态的正常使用。在这个分支上加检查，等于把「没有 store」这个
// 契约允许的可选状态错当成错误状态去 fail-loud，违反的是同一条铁律的另一半。
func (e *eventRecorder) newTurnID(typ domain.SessionEventType) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	turn, step := e.turn, e.step
	id := fmt.Sprintf("%s:%d:%d:%s", e.session, turn, step, typ)
	if !e.enabled() {
		return id
	}
	coord := turnIDCoordinate{turn: turn, step: step, typ: typ}
	if e.issuedTurnIDs == nil {
		e.issuedTurnIDs = make(map[turnIDCoordinate]bool)
	}
	if e.issuedTurnIDs[coord] {
		panic(fmt.Sprintf(
			"runtime: turn_id coordinate (session=%q, turn=%d, step=%d, type=%s) issued twice in one execution: "+
				"two %s events were recorded for the same (turn, step); newTurnID's uniqueness guarantee assumes "+
				"this never happens (see its doc comment) — the two rows would collide on the same turn_id and "+
				"silently overwrite each other in projection",
			e.session, turn, step, typ, typ))
	}
	e.issuedTurnIDs[coord] = true
	return id
}

// recordToolCall 记一次工具调用**被派发之前**的事实（spec §5 屏障 2 的前提）。
func (e *eventRecorder) recordToolCall(call domain.ToolCall) {
	arguments, err := json.Marshal(call.Arguments)
	if err != nil {
		panic(fmt.Sprintf("runtime: marshal tool call arguments for %s: %v", call.ID, err))
	}
	e.append(domain.SessionEventToolCall, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": call.ID, "name": call.Name,
		"arguments": truncateRunes(string(arguments), maxEventPreviewRunes),
	})
}

// dropBufferedToolCall 把缓冲末尾那条 tool/call 撤回——只给屏障 2 落盘失败用。
//
// 屏障 2 失败意味着这次调用**没有被派发**，工具体一次也没跑。但 flush 在 Append
// 失败时按设计保留缓冲（让重试不丢事件），于是这条从未发生的调用会被本次执行后续
// 任何一次 flush（收尾的 closeTurnOnError 就有一次）写进日志，成为永远等不到
// tool/result 的孤儿；恢复逻辑还会为它补一条合成结果。spec §4.3.1 第 1 条守的是
// 「记录过的调用必须有结果」，而这里是它的反面——记了一次根本没发生的调用，比漏记
// 结果更糟。撤回是唯一正确的收场。
//
// 只撤缓冲末尾那一条，且必须真的是同一个 call_id 的 tool/call：不是的话说明调用点
// 与这里的假设已经对不上（编程错误），panic 而不是猜着删——删错一条就是静默丢事件。
func (e *eventRecorder) dropBufferedToolCall(callID string) {
	if !e.enabled() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) == 0 {
		panic(fmt.Sprintf("runtime: drop buffered tool/call %s: nothing is buffered", callID))
	}
	last := e.pending[len(e.pending)-1]
	if last.Type != domain.SessionEventToolCall {
		panic(fmt.Sprintf("runtime: drop buffered tool/call %s: the last buffered event is %s", callID, last.Type))
	}
	var payload struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(last.Data, &payload); err != nil {
		panic(fmt.Sprintf("runtime: drop buffered tool/call %s: decode buffered payload: %v", callID, err))
	}
	if payload.CallID != callID {
		panic(fmt.Sprintf("runtime: drop buffered tool/call %s: the last buffered tool/call is %s", callID, payload.CallID))
	}
	e.pending = e.pending[:len(e.pending)-1]
}

// recordToolResult 记一次工具调用的结果。
//
// **每条记录过的 tool/call 都必须有它**（spec §4.3.1 第 1 条）：工具失败、取消、被
// 拒绝一样要发，`isError` 为真。少发一条，恢复时会把它当成「崩在工具里」而补一条
// 合成结果，日志就与真实发生的事不符了。
func (e *eventRecorder) recordToolResult(callID string, preview string, isError bool, dur time.Duration) {
	e.append(domain.SessionEventToolResult, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(),
		"call_id": callID, "preview": truncateRunes(preview, maxEventPreviewRunes),
		"is_error": isError, "duration_ms": dur.Milliseconds(),
	})
}

// recordStepEnd 记一步的结束，并把 step 计数推进一格。
//
// 同 recordTurnStart：先过 enabled() 再碰 e.mu，避免在「没配 store」时白白加锁。这不
// 意味着与其余六个 record* 方法在 nil 接收者上的行为一致——见 recordTurnStart 与
// eventRecorder 类型文档对接收者契约的说明。
func (e *eventRecorder) recordStepEnd(reason string) {
	if !e.enabled() {
		return
	}
	e.append(domain.SessionEventStepEnd, map[string]any{
		"turn": e.currentTurn(), "step": e.currentStep(), "reason": reason,
	})
	e.mu.Lock()
	e.step++
	e.mu.Unlock()
}

// recordTurnEnd 记一个轮次的结束。
func (e *eventRecorder) recordTurnEnd(reason string) {
	e.append(domain.SessionEventTurnEnd, map[string]any{
		"turn": e.currentTurn(), "reason": reason,
	})
}

func (e *eventRecorder) currentTurn() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turn
}

func (e *eventRecorder) currentStep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.step
}

// flush 把缓冲里的事件落盘（spec §5 的三个屏障调它）。
//
// seq 在这里才分配：P1 的 store 要求首个 seq 等于库里的 next-seq，而库里走到哪只有
// 这一刻才知道（同一会话可能有别的写入者）。第一次 flush 用 ReadFrom 对齐游标，
// 之后按本次执行自己写过的条数递增。
//
// 「之后按自己写过的条数递增」只在**本次执行是这条会话当前唯一的写入者**时成立，
// 而这一条由 RunTask 持有的会话执行锁（sessionRunLocks）提供，不是这里自己保证的。
//
// **失败就是失败**：调用方（屏障）据此决定不发请求、不进工具体、不开下一步。
// 缓冲保持不变，让调用方能在重试时不丢事件。
func (e *eventRecorder) flush(ctx context.Context) error {
	if !e.enabled() {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pending) == 0 {
		return nil
	}
	if !e.seqKnown {
		existing, err := e.store.ReadFrom(ctx, e.session, 0)
		if err != nil {
			return fmt.Errorf("align session event cursor for %q: %w", e.session, err)
		}
		e.nextSeq = int64(len(existing))
		e.seqKnown = true
	}
	batch := make([]domain.SessionEvent, len(e.pending))
	for i, event := range e.pending {
		event.Seq = e.nextSeq + int64(i)
		batch[i] = event
	}
	if err := e.store.Append(ctx, e.session, batch); err != nil {
		return fmt.Errorf("persist session events for %q: %w", e.session, err)
	}
	e.nextSeq += int64(len(batch))
	e.pending = e.pending[:0]
	return nil
}

// barrier 是一个 fail-closed 的落盘点（spec §5）。
//
// 三处调用：模型请求前、进工具体前、开下一步前。刷不动就返回错误，调用方**不做那件事**。
// 这是行为改变——数据库写不动时任务会失败而不是照跑——理由在屏障 2：tool/call 必须
// 先落盘，否则崩在工具体里就成了「工具真的执行过、但日志里没有这次调用」，恢复时补不出
// 合成结果，而工具正是有外部副作用的那一端。「先记录再执行」保证任何真发生过的副作用
// 在日志里都有它的调用。
//
// at 是这个屏障的位置，进错误信息——排查的人要立刻知道是哪一处挡住了。
func (e *eventRecorder) barrier(ctx context.Context, at string) error {
	if err := e.flush(ctx); err != nil {
		return fmt.Errorf("session event barrier %s: %w", at, err)
	}
	return nil
}

// truncateRunes 按 rune 截断并标注截断量，使读的人知道自己看的是一段而不是全部。
//
// 截断标注本身也占 rune：直接把内容砍到 limit 再拼标注会让总长度超过 limit
// （截断测试真的抓到过这个：标注比它替换掉的内容还长）。所以先按 limit 估出标注的
// 长度上限，把内容留出这部分空间，再用真实保留量重新生成标注——保留量只会比估计时
// 更小或相等，标注只会更短或相等，总长度因此保证不超过 limit。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	total := len(runes)
	if total <= limit {
		return s
	}
	budget := limit
	if budget < 0 {
		budget = 0
	}
	estimate := fmt.Sprintf("\n…[truncated: %d of %d runes shown]", budget, total)
	kept := budget - len([]rune(estimate))
	if kept < 0 {
		kept = 0
	}
	suffix := fmt.Sprintf("\n…[truncated: %d of %d runes shown]", kept, total)
	return string(runes[:kept]) + suffix
}
