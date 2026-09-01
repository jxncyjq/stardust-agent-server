package storage

import (
	"encoding/json"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
)

// turnKey 是投影的折叠键：一个任务的一个角色恰好折成一行 ConversationTurn。
// 理由见 projectTurns 的文档注释「按 (task_id, role) 折叠」一节。
type turnKey struct {
	taskID string
	role   domain.ConversationRole
}

// mergeGeneratedFiles 把两批工作区相对路径并起来，去重并保持首次出现的顺序。
//
// 与 internal/runtime 的 appendUniqueStr 同语义——那是这些路径在生产上被累积起来
// 的地方，两侧顺序一致，投影出来的列表才与退役中 conversation_turns 那一行存的
// 列表逐项相同。纯函数：不改动入参底层数组（有一侧为空时直接返回另一侧，那一侧
// 是本次事件刚反序列化出来的、或是已经归属 turns[at] 的切片，都没有别的持有者）。
func mergeGeneratedFiles(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	if len(existing) == 0 {
		return incoming
	}
	seen := make(map[string]bool, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, path := range existing {
		if !seen[path] {
			seen[path] = true
			merged = append(merged, path)
		}
	}
	for _, path := range incoming {
		if !seen[path] {
			seen[path] = true
			merged = append(merged, path)
		}
	}
	return merged
}

// projectTurns 把一条会话的事件流投影成对话轮次。
//
// 这是 conversation_turns 退役之后**唯一**的 turn 来源（spec §3 取舍 A2）。它是
// 纯函数：不碰数据库、不读文件、不看时钟，于是同一批事件永远投影出同一批 turn——
// TestProjectingTheSameEventsTwiceProducesIdenticalTurnIDs 验的就是这条：turn_id
// 本身在事件载荷里写死（见 internal/runtime/eventlog.go 的 newTurnID），这里只是
// 原样透传，不重新生成，所以稳定性来自「不生成」而不是某种确定性算法。
//
// events 必须按 seq 升序传入——这正是 ReadFrom 的返回契约（session_events.go：
// `ORDER BY seq`）。本函数不重新排序：P3 的读路径一律用 ReadFrom、绝不调用 Load
// （spec §4.3.1 第 3 条），而 ReadFrom 已经保证了这个顺序；重新排序既是不必要的
// 工作，也会掩盖「调用方传错顺序」这类编程错误。
//
// 按 call_id 配对，不按位置（spec §4.3.1 第 2 条）：崩溃恢复补出的 tool/result
// 排在日志**尾部**，可能排在自己那次调用的 step/end 之后；按位置配会把它配到错误
// 的 assistant 上，产出非法 transcript。
//
// 这条约束在本函数里没有「配对代码」可看：P3 投影只产出 user/assistant 两种 turn
// （domain.ConversationRole 也只定义了这两个值），工具往返进入模型上下文是 G3，
// 属 P5，本期不做。tool/call 与 tool/result 在下面的 switch 里落在同一个 no-op
// 分支，无论它们出现在事件流的哪个位置、以什么顺序出现、call_id 互相之间是什么
// 关系，这条分支都不读取它们的任何字段——因此谈不上「按位置配对」，因为根本没有
// 配对发生。TestToolResultsPairByCallIDNotByPosition 与
// TestToolCallAndResultOrderNeverAffectsProjection 验证的正是这一点：尾部乱序的
// tool/result 不会腐蚀相邻的 assistant turn、也不会让投影出错。
//
// P5 打开 G3、真正要把工具调用及其结果投影成内容时，必须在这里新增一个按 call_id
// 建索引（例如 map[callID]toolResultPayload）、再消费 tool_calls 摘要去查表的分支，
// **不能**假设 tool/result 出现在对应 tool/call 之后的固定相对位置——上面这段
// 注释和这两条测试就是留给那次改动的证据：位置无关性从一开始就是不变量的一部分，
// 不是这次实现漏掉的东西。
//
// 未知事件类型一律**报错并指名**，不静默跳过——静默跳过意味着将来加了新事件类型，
// 这个函数会悄悄少算而没人发现。
//
// # 按 (task_id, role) 折叠：一个任务恰好一条 user turn + 一条 assistant turn
//
// 事件与 ConversationTurn **不是一一对应**的。runtime 的 generateStep 每一轮模型
// 请求都记一条 assistant/message（runtime.go 的首轮调用 + runToolLoop 循环内每轮，
// 外加 resume 分支手工补记的那条），所以一次带 N 轮工具的任务写出 N+1 条
// assistant/message；user/message 也会在「挂起→恢复」时被 RunTask 顶部重记一次。
// 逐事件产出 turn 会让读侧看到与退役中的 conversation_turns **不同**的形状：
// 每轮一个气泡（其中多数正文为空，因为请求工具的那几轮 resp.Text 是空的）、
// recent-N 窗口被空 turn 占满、token 口径从「每任务一条」变成「每轮一条」、
// 文件卡片重复渲染。
//
// 退役中的写入方（internal/server/http.go 的 recordUserTurn / recordAssistantTurn）
// 定义的才是本函数必须复现的契约：**每任务恰好一条** user turn 与一条 assistant
// turn，id 为 "<task_id>:user" / "<task_id>:assistant"，assistant 的正文是最终
// 答案、用量是整个任务的累计值、GeneratedFiles 是这个任务的全部产出。所以这里按
// (task_id, role) 折叠：
//
//   - ID = "<task_id>:<role>"，与退役中的写入方逐字一致。这不只是形状好看：
//     search_session 的 discovery 走 conversation_turns_fts（Task 4 之前仍是
//     serve 写的那批行，id 正是这个形状），scroll 走本函数投影出来的 ID，两个
//     ID 空间必须相等，否则模型拿 discovery 给的 id 回来 scroll 必然
//     anchor not found。事件载荷里的 turn_id 标识的是**那一条事件**（P4 轨迹与
//     Task 4 的检索按它定位），不是这一行 turn 的 id；它仍然必须存在（下面的
//     非空校验），只是不再充当 ConversationTurn.ID。
//   - Content / CreatedAt 取该 (task_id, role) 的**最后**一条事件：assistant 的
//     最后一条就是最终答案（工具循环退出的条件正是模型不再请求工具，那一轮的
//     响应即答案；撞上限走 generateFinalStep 时同理）。
//   - 四个 token 字段**累加**：generateStep 每次记的是「这一次响应的增量用量」
//     （resume 分支特意记 0，因为那条响应的用量已由生成它的那一轮记过），累加
//     出来正是 runToolLoop 的 st.*Tokens，也就是退役中 taskUsage 的口径。
//   - GeneratedFiles 取**并集**（去重、保持首次出现顺序）。runtime 的
//     st.generatedFiles 本身是累积的（runtime.go 的 appendUniqueStr），所以「取
//     最后一条」在正常路径上与并集等价；取并集额外扛住 resume 分支——那条重记的
//     assistant/message 特意传 nil（恢复点上 PendingCalls 还没执行），一旦它因
//     任何原因成为某个任务的最后一条 assistant 事件，「取最后一条」就会把已经
//     产出的文件抹掉，而并集不会。
//   - AgentID / ModelProfile 取最后一条：它们在一个任务内恒定（都来自
//     task.AgentID / r.modelProfile），这里不做「非空才覆盖」之类的挑选——那会
//     变成用旧值给新值兜底。
//
// 折叠不改变顺序：同一条会话日志里一个任务的事件是连续的（会话执行锁保证一次只有
// 一个任务在写），所以按「首次出现」的位置排出来的次序与退役中
// ORDER BY created_at 的次序一致。
func projectTurns(sessionID string, events []domain.SessionEvent) ([]domain.ConversationTurn, error) {
	turns := make([]domain.ConversationTurn, 0, len(events)/3)
	// index 把 (task_id, role) 映到它在 turns 里的下标；不在表里就是这个任务这个
	// 角色的第一条事件，追加一行。
	index := make(map[turnKey]int)
	fold := func(candidate domain.ConversationTurn) {
		key := turnKey{taskID: candidate.TaskID, role: candidate.Role}
		at, ok := index[key]
		if !ok {
			index[key] = len(turns)
			turns = append(turns, candidate)
			return
		}
		existing := &turns[at]
		existing.AgentID = candidate.AgentID
		existing.ModelProfile = candidate.ModelProfile
		existing.Content = candidate.Content
		existing.CreatedAt = candidate.CreatedAt
		existing.PromptTokens += candidate.PromptTokens
		existing.CompletionTokens += candidate.CompletionTokens
		existing.CachedTokens += candidate.CachedTokens
		existing.TotalTokens += candidate.TotalTokens
		existing.GeneratedFiles = mergeGeneratedFiles(existing.GeneratedFiles, candidate.GeneratedFiles)
	}

	for _, event := range events {
		switch event.Type {
		case domain.SessionEventUserMessage:
			var payload struct {
				TurnID  string `json:"turn_id"`
				TaskID  string `json:"task_id"`
				AgentID string `json:"agent_id"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project session %q: decode user/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			// json.Unmarshal only catches syntax errors: legal JSON that simply
			// omits a required key decodes to the Go zero value ("") without
			// error. turn_id/task_id are not contractually optional here — an
			// empty task_id defeats the same-task filter in
			// internal/runtime/session_turns.go, which would let the model see
			// a duplicate user message, and it is also this turn's folding key
			// and the stem of its ID, so an empty one silently merges unrelated
			// tasks into one row. turn_id identifies the *event* (P4's
			// trajectory and Task 4's search locate by it) rather than the
			// projected turn; an absent one means the producer is broken, and
			// letting it through would hand those consumers an unaddressable
			// event. Both must be checked explicitly; a decoded zero value must
			// not be allowed to stand in for "field present but empty".
			if payload.TurnID == "" {
				return nil, fmt.Errorf("project session %q: user/message at seq %d has no turn_id",
					sessionID, event.Seq)
			}
			if payload.TaskID == "" {
				return nil, fmt.Errorf("project session %q: user/message at seq %d has no task_id",
					sessionID, event.Seq)
			}
			// agent_id 与 assistant 分支同样是必填、同样显式校验：P3 计划把它列为
			// 五个字段缺口之一（"/turns 响应与 FTS5 索引都带这个字段"），退役中的
			// recordUserTurn 写的正是 task.AgentID。它没有任何「可选」的契约声明，
			// 而两个生产写入方都保证它非空（app.RunTask 把空的 AgentID 兜成
			// "cli-agent" 之后才建 domain.Task；TUI 的 appendTurnEvent 在写之前
			// 自己先校验），所以「解出空串」只可能是写入方坏了，不是合法状态。
			if payload.AgentID == "" {
				return nil, fmt.Errorf("project session %q: user/message at seq %d has no agent_id",
					sessionID, event.Seq)
			}
			fold(domain.ConversationTurn{
				ID:        payload.TaskID + ":" + string(domain.ConversationRoleUser),
				SessionID: sessionID,
				TaskID:    payload.TaskID,
				AgentID:   payload.AgentID,
				Role:      domain.ConversationRoleUser,
				Content:   payload.Content,
				CreatedAt: event.Time,
			})

		case domain.SessionEventAssistantMessage:
			var payload struct {
				TurnID       string `json:"turn_id"`
				TaskID       string `json:"task_id"`
				AgentID      string `json:"agent_id"`
				Content      string `json:"content"`
				ModelProfile string `json:"model_profile"`
				Usage        struct {
					Prompt     int `json:"prompt"`
					Completion int `json:"completion"`
					Cached     int `json:"cached"`
					Total      int `json:"total"`
				} `json:"usage"`
				GeneratedFiles []string `json:"generated_files"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project session %q: decode assistant/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			// Same rationale as the user/message branch above: a legal-JSON
			// payload missing a required key decodes to "" without error, and
			// that must not be allowed to stand in as a valid turn. turn_id and
			// task_id carry the same stakes here (event addressability for P4 /
			// Task 4, and session_turns.go's same-task filter plus this turn's
			// folding key and ID stem). agent_id is checked too:
			// unlike GeneratedFiles/ModelProfile — which domain.ConversationTurn's
			// doc comment explicitly marks as a legitimate optional — nothing
			// documents agent_id as allowed to be absent on an assistant turn,
			// and internal/runtime/eventlog.go always writes it from
			// task.AgentID (recordAssistantMessage's doc comment lists it
			// alongside task_id/turn_id as fields "投影" needs back, with no
			// optionality carve-out). Per CLAUDE.md's fail-loud rule, when
			// optionality is undocumented it is treated as required.
			if payload.TurnID == "" {
				return nil, fmt.Errorf("project session %q: assistant/message at seq %d has no turn_id",
					sessionID, event.Seq)
			}
			if payload.TaskID == "" {
				return nil, fmt.Errorf("project session %q: assistant/message at seq %d has no task_id",
					sessionID, event.Seq)
			}
			if payload.AgentID == "" {
				return nil, fmt.Errorf("project session %q: assistant/message at seq %d has no agent_id",
					sessionID, event.Seq)
			}
			fold(domain.ConversationTurn{
				ID:               payload.TaskID + ":" + string(domain.ConversationRoleAssistant),
				SessionID:        sessionID,
				TaskID:           payload.TaskID,
				AgentID:          payload.AgentID,
				ModelProfile:     payload.ModelProfile,
				Role:             domain.ConversationRoleAssistant,
				Content:          payload.Content,
				CreatedAt:        event.Time,
				PromptTokens:     payload.Usage.Prompt,
				CompletionTokens: payload.Usage.Completion,
				CachedTokens:     payload.Usage.Cached,
				TotalTokens:      payload.Usage.Total,
				GeneratedFiles:   payload.GeneratedFiles,
			})

		case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
			domain.SessionEventStepStart, domain.SessionEventStepEnd,
			domain.SessionEventToolCall, domain.SessionEventToolResult:
			// 这六类不产出 turn：它们是轨迹（P4）与搜索（Task 4）的素材。
			// 把工具往返也放进模型上下文是 G3，属 P5，本期不做——见本函数文档
			// 注释里对「按 call_id 配对」这条约束在本层为什么没有代码可写的说明。
			//
			// 逐条列出而不是用 default 跳过，是为了让「加了新事件类型却忘了在这里
			// 决定它怎么投影」变成一个编译期就能被发现的遗漏 + 下面那条 default 的
			// 运行期报错，而不是悄悄少算。

		default:
			return nil, fmt.Errorf("project session %q: unknown event type %q at seq %d",
				sessionID, event.Type, event.Seq)
		}
	}

	return turns, nil
}
