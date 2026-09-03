package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// transcriptCallKey 唯一定位**一次**工具调用。
//
// 它是本文件的配对键，而 call_id 单独一个**不是**：spec §4.3.1 第 4 条明文允许
// 跨 turn/step 复用同一个 call_id（provider 的 tool call id 只保证单次响应内唯一，
// 按序号生成 call_1 / tooluse_0 的实现是存在的），而 runtime 的 disambiguateCallIDs
// 只在**单轮内**去重——它的 used/arrived 两张表都是每次调用新建的局部变量。所以
// 「两轮都叫 call_1」是合法且预期的日志，会原样落盘。
//
// 把配对键取成会话级的 call_id，这样的日志会让后一轮的结果覆盖前一轮的，模型读到
// 一条张冠李戴的历史工具输出——而且 port.InferenceRequest.Validate() 放行、provider
// 照收，**没有任何东西会报错**。这比配错被 provider 拒收更糟：错得无声无息。
//
// (turn, step) 把键收窄到「哪一次宣告」。这两个分量成立靠两条已核实的事实：
//
//   - 同一轮里 assistant/message、tool/call、tool/result 带的 (turn, step) **相同**。
//     eventRecorder 的顺序是 recordStepStart → recordAssistantMessage →
//     executeToolCalls（recordToolCall/recordToolResult）→ recordStepEnd，而 step
//     只在 recordStepEnd 里 ++（internal/runtime/eventlog.go）。turn 号按会话单调
//     （newTaskRecorder 从库里已有事件解出下一个），所以 (turn, step) 在整条会话里
//     唯一标识一次模型请求。
//   - 崩溃恢复补出的 tool/result 虽然排在日志**尾部**，载荷里带的却是**原来那次调用
//     的** turn/step——synthesizeClosers 直接抄 planRecovery 从那条 tool/call 读到的
//     call.turn / call.step（internal/storage/session_events.go）。所以「按位置无关地
//     配对」这条性质不受影响：尾部的结果仍然配回它自己那一步。
//
// 守卫：TestTheSameCallIDInTwoRoundsPairsToItsOwnResult。
type transcriptCallKey struct {
	turn   int
	step   int
	callID string
}

// projectTranscript 把一条会话的事件流投影成 provider transcript——assistant
// 消息带 tool_calls，其后跟与之配对的 tool 消息（spec §6 的 G3）。
//
// 它与 projectTurns 并列而不是取代它：turns 是「谁说了什么」的视图（G3 关闭时
// 走它），transcript 是「模型看到的完整往返」（G3 打开时走它）。两者读同一批
// 事件，谁也不改谁。
//
// events 必须按 seq 升序传入——这正是 ReadFrom 的返回契约（session_events.go：
// `ORDER BY seq`）。本函数不重新排序，理由与 projectTurns 相同：重新排序既是不
// 必要的工作，也会掩盖「调用方传错顺序」这类编程错误。**但它也不依赖顺序来配对**，
// 见下。
//
// # 按 (turn, step, call_id) 配对，不按位置（spec §4.3.1 第 2 条）
//
// 崩溃恢复补出的 tool/result 排在日志尾部，可能排在自己那次调用的 step/end 之后、
// 且顺序与调用相反。按位置配会把结果配到错误的调用上——而 port.InferenceMessage
// 的文档注释写死了「an OpenAI-compatible provider rejects a tool message it
// cannot pair with a preceding tool call」，配错的后果是整个请求被 provider 拒收，
// 不是显示得难看一点。所以这里先扫一遍把结果按 transcriptCallKey 建成表，再由
// assistant 的 tool_calls 清单去查表，全程不看任何事件的相对位置。
//
// 键为什么必须带上 (turn, step) 而不能只是 call_id，见 transcriptCallKey 的文档注释。
// 守卫：TestResultsPairByCallIDEvenWhenTheyTrailAtTheEnd（位置无关）、
// TestTheSameCallIDInTwoRoundsPairsToItsOwnResult（作用域）。
//
// # 只宣告有结果的调用
//
// 一条被记录过却没有结果事件的 tool/call 只可能来自进程硬崩（spec §4.3.1 第 1 条；
// 「记了一次根本没发生的调用」这个反面由 runtime 的 dropBufferedToolCall 挡住）。
// 把它放进 tool_calls 会让 provider 等一条永远不来的 tool 消息，同样拒收。所以
// 必须**先扫完结果、再决定宣告什么**——边走边配做不到这件事，因为决定「c2 没有
// 结果」需要看完整条日志。守卫：TestAnUnansweredCallIsNotAnnouncedInTheTranscript。
//
// 「未答」是崩溃后合法的残留状态，跳过它是在产出一份**合法**的 transcript，不是
// 在给错误兜底；真正的坏数据（空 call_id、缺 turn/step）在下面被显式拒绝，不与它
// 混为一谈。
//
// # 发不出去的空 assistant 消息一并跳过
//
// 上面那条过滤会留下一种残渣：模型只返工具调用、不返文本时 content 就是 ""，若这
// 一步的调用**全部**未答（硬崩，或挂起→恢复那条把同一响应在旧 turn 里重记一次的
// 路径），过滤完就剩一条 {role:"assistant", content:"", tool_calls:nil}。
// OpenAI 兼容 provider 拒收「既无 content 也无 tool_calls」的 assistant 消息，而
// port.InferenceRequest.Validate() 管不到它（它只校验 role=tool 必须有 ToolCallID）。
//
// 裁定：**整条跳过**，不给占位内容。占位内容是替模型编一句它没说过的话，那才是兜底；
// 跳过一条零信息的消息不改变任何语义——它既没有文本要给模型看，也没有可配对的调用。
// 这与「未答的调用不宣告」是同一类：产出一份合法 transcript，而不是掩盖错误。
// 守卫：TestAnAssistantMessageWithNothingLeftToSayIsSkipped，以及它的反面
// TestAnAssistantMessageWithTextSurvivesWhenItsCallsAreFilteredOut（有正文就得留着）。
//
// 未知事件类型一律报错并指名，不静默跳过——server 侧的类型是闭集但它会长，静默
// 跳过意味着加了新类型之后 transcript 会悄悄少东西而没人发现。
//
// 纯函数：不碰数据库、不读文件、不看时钟。同一批事件永远投影出同一个结果。
func projectTranscript(sessionID string, events []domain.SessionEvent) ([]port.InferenceMessage, error) {
	// 第一遍：把所有结果按 (turn, step, call_id) 收起来。必须先扫完，因为恢复补出的
	// 结果排在尾部。
	results, err := collectToolResults(sessionID, events)
	if err != nil {
		return nil, err
	}
	// 同样先扫完的还有每次调用的**参数**。tool/call 事件是 arguments 唯一的落点：
	// assistant/message 的 tool_calls 载荷里只有 call_id 与 name
	// （internal/runtime/eventlog.go 的 recordAssistantMessage）。不收这一遍，
	// domain.ToolCall.Arguments 就是 nil，adapter 把 nil map 编成 `null` 送上线，
	// 而 OpenAI 兼容契约要求 function.arguments 是一个 **JSON 对象**的字符串。
	// 守卫：TestHistoryToolCallsCarryTheirArgumentsOnTheWire（internal/runtime）。
	arguments, err := collectToolCallArguments(sessionID, events)
	if err != nil {
		return nil, err
	}

	msgs := make([]port.InferenceMessage, 0, len(events)/3)
	for _, event := range events {
		switch event.Type {
		case domain.SessionEventUserMessage:
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode user/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			msgs = append(msgs, port.InferenceMessage{
				Role:    port.RoleUser,
				Content: payload.Content,
			})

		case domain.SessionEventAssistantMessage:
			var payload struct {
				// 指针而不是 int：缺字段时 json 给零值，而 turn 0 / step 0 是合法坐标，
				// 「缺了」与「就是 0」会被折叠成同一件事，配对键随之指到别处去。
				// spec §4.1 规定这两个字段必填，缺了是坏日志，不是可选项。
				Turn      *int   `json:"turn"`
				Step      *int   `json:"step"`
				Content   string `json:"content"`
				ToolCalls []struct {
					CallID string `json:"call_id"`
					Name   string `json:"name"`
				} `json:"tool_calls"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, fmt.Errorf("project transcript for %q: decode assistant/message at seq %d: %w",
					sessionID, event.Seq, err)
			}
			// 只有真的要配对时才要求坐标：一条没有 tool_calls 的 assistant 消息
			// （纯文本回答）不查表，不必为它把整条会话判成坏日志。
			if len(payload.ToolCalls) > 0 && (payload.Turn == nil || payload.Step == nil) {
				return nil, fmt.Errorf("project transcript for %q: assistant/message at seq %d announces tool calls but has no turn/step, so its calls cannot be paired",
					sessionID, event.Seq)
			}
			calls := make([]domain.ToolCall, 0, len(payload.ToolCalls))
			keys := make([]transcriptCallKey, 0, len(payload.ToolCalls))
			// 一条 assistant 不得两次宣告同一个 call_id。
			//
			// spec §4.3.1 第 4 条禁的是「同一个 step 内、还未被应答的 tool/call 复用
			// call_id」（跨 turn/step 的复用是允许的）——同一条 assistant 里的两次宣告
			// 在发出时都还没被应答，正落在禁区里。
			//
			// 不拒的话两次都会命中同一个 results[key]/arguments[key]，产出
			// tool_calls=[c1, c1] 加两条 tool_call_id 相同的 tool 消息，provider 拒收
			// 整个请求——响亮，但发生在离原因很远的地方。在这里拒能直接指名 seq。
			//
			// 这补的是一处不对称：collectToolResults 与 collectToolCallArguments 都为
			// 撞键写了 fail-loud（见后者「is a second record of call_id」那条），三处
			// 同性质的地方只有宣告侧放行。
			// 守卫：TestAnAssistantAnnouncingTheSameCallIDTwiceIsRefused。
			announced := make(map[string]bool, len(payload.ToolCalls))
			for _, c := range payload.ToolCalls {
				// 空 call_id 只可能来自坏掉的写入方：recordAssistantMessage 原样抄
				// domain.ToolCall.ID，而它是 provider 给的调用标识。不拒的话它会掉进
				// 下面那条「未答就不宣告」的 continue——因为 results 里永远不会有空
				// call_id 的键（collectToolResults 已经拒绝了空 call_id 的结果）——于是
				// 「写入方坏了」与「硬崩留下未答调用」这两件性质完全不同的事被折叠成
				// 同一种无声后果。
				// 守卫：TestAnAnnouncedCallWithAnEmptyCallIDIsRefused。
				if strings.TrimSpace(c.CallID) == "" {
					return nil, fmt.Errorf("project transcript for %q: assistant/message at seq %d announces a tool call with no call_id, so it can never be paired",
						sessionID, event.Seq)
				}
				if announced[c.CallID] {
					return nil, fmt.Errorf("project transcript for %q: assistant/message at seq %d announces call_id %q twice in turn %d step %d; one assistant cannot make the same call twice",
						sessionID, event.Seq, c.CallID, *payload.Turn, *payload.Step)
				}
				announced[c.CallID] = true
				key := transcriptCallKey{turn: *payload.Turn, step: *payload.Step, callID: c.CallID}
				// 只宣告有结果的调用——见函数头那一节。
				if _, answered := results[key]; !answered {
					continue
				}
				// 有结果的调用必然有它的 tool/call 事件：屏障 2 先记 tool/call 再落盘，
				// 落盘失败时 dropBufferedToolCall 把它撤回、这次调用根本不派发，因此
				// 也不会有结果（internal/runtime/eventlog.go）；恢复合成的结果同样只
				// 为**读到过的 tool/call** 补。所以缺了它是坏日志，不是崩溃残留——
				// 后者已经在上面那条「未答就不宣告」里被跳过了。
				// 守卫：TestAnAnsweredCallWithNoToolCallEventIsRefused。
				raw, recorded := arguments[key]
				if !recorded {
					return nil, fmt.Errorf("project transcript for %q: assistant/message at seq %d announces answered call_id %q in turn %d step %d, but no tool/call event recorded its arguments",
						sessionID, event.Seq, c.CallID, key.turn, key.step)
				}
				args, err := decodeTranscriptCallArguments(sessionID, event.Seq, c.CallID, raw)
				if err != nil {
					return nil, err
				}
				calls = append(calls, domain.ToolCall{ID: c.CallID, Name: c.Name, Arguments: args})
				keys = append(keys, key)
			}
			// 什么都不剩的 assistant 消息发不出去，整条跳过——见函数头那一节。
			if len(calls) == 0 && strings.TrimSpace(payload.Content) == "" {
				continue
			}
			msg := port.InferenceMessage{Role: port.RoleAssistant, Content: payload.Content}
			if len(calls) > 0 {
				msg.ToolCalls = calls
			}
			msgs = append(msgs, msg)
			// 紧跟着把这批调用的结果取出来铺在后面——provider 要求 tool 消息紧随宣告
			// 它的 assistant。keys 与 calls 一一对应，同下标就是同一次调用。
			for i, c := range calls {
				msgs = append(msgs, port.InferenceMessage{
					Role:       port.RoleTool,
					ToolCallID: c.ID,
					Content:    results[keys[i]],
				})
			}

		case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
			domain.SessionEventStepStart, domain.SessionEventStepEnd,
			domain.SessionEventToolCall, domain.SessionEventToolResult:
			// 这六类不直接产出消息：边界事件没有模型可读的内容；tool/call 的信息
			// 已经在 assistant 的 tool_calls 里；tool/result 已在第一遍收进 results。
			//
			// 逐条列出而不是让它们落进 default，是为了让「加了新事件类型却忘了决定
			// 它怎么投影」落到下面那条 default 的运行期报错上，而不是悄悄少算。

		default:
			return nil, fmt.Errorf("project transcript for %q: unknown event type %q at seq %d",
				sessionID, event.Type, event.Seq)
		}
	}
	return msgs, nil
}

// collectToolResults 扫一遍事件，把每条 tool/result 的模型可读内容按
// (turn, step, call_id) 收起来。
//
// 单独一遍是必须的：恢复补出的结果排在日志尾部，边走边配会漏掉它们。
//
// # 撞键就报错，不覆盖
//
// 这张表里撞键意味着「同一次调用有两条结果」，而那不是任何合法路径能产生的状态，
// 所以它是坏日志，必须 fail-loud（CLAUDE.md §0：撞了却猜一个值接着跑 = 兜底）：
//
//   - 同一 (turn, step) 内 call_id 不重复——runtime 的 disambiguateCallIDs 就是在
//     单轮内去重的；
//   - 恢复对每个 call_id 至多合成**一条**结果（session_events.go 的 planRecovery
//     用 emitted 去重），而且只为**从未被应答**的调用合成，所以「真结果 + 合成结果」
//     撞在同一个键上也不可能；
//   - 跨轮复用同一个 call_id 是合法的，但那时 (turn, step) 不同，本来就不撞键——
//     这正是这个键存在的理由（见 transcriptCallKey）。
//
// 早先这里写的是「后者覆盖前者，因为恢复补出的合成结果排在尾部、覆盖正是想要的
// 语义」。那条辩护是事实错误的：按上面第二条，两条结果永远不会都来自恢复，于是
// 那条覆盖分支**从来不在它被设计的场景里触发，只在它没被设计的场景（跨轮复用）里
// 触发，并在那里静默配错**。
// 守卫：TestTwoResultsForTheSameCallAreRefused。
func collectToolResults(sessionID string, events []domain.SessionEvent) (map[transcriptCallKey]string, error) {
	results := make(map[transcriptCallKey]string)
	for _, event := range events {
		if event.Type != domain.SessionEventToolResult {
			continue
		}
		var payload struct {
			// 指针的理由同 assistant/message：turn 0 / step 0 是合法坐标，
			// 缺字段的零值会伪装成它们。
			Turn         *int   `json:"turn"`
			Step         *int   `json:"step"`
			CallID       string `json:"call_id"`
			Preview      string `json:"preview"`
			IsError      bool   `json:"is_error"`
			SpillLocator string `json:"spill_locator"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("project transcript for %q: decode tool/result at seq %d: %w",
				sessionID, event.Seq, err)
		}
		// 空 call_id 的结果配不上任何调用。放过它不只是丢一条 tool 消息：它对应的
		// 那次调用会因此被判成「未答」而整个从 transcript 里消失，无声无息。
		// 守卫：TestAToolResultWithAnEmptyCallIDIsRefused。
		if strings.TrimSpace(payload.CallID) == "" {
			return nil, fmt.Errorf("project transcript for %q: tool/result at seq %d has no call_id, so it cannot be paired",
				sessionID, event.Seq)
		}
		if payload.Turn == nil || payload.Step == nil {
			return nil, fmt.Errorf("project transcript for %q: tool/result at seq %d for call_id %q has no turn/step, so it cannot be paired with the call that made it",
				sessionID, event.Seq, payload.CallID)
		}
		key := transcriptCallKey{turn: *payload.Turn, step: *payload.Step, callID: payload.CallID}
		if _, duplicate := results[key]; duplicate {
			return nil, fmt.Errorf("project transcript for %q: tool/result at seq %d is a second result for call_id %q in turn %d step %d; one call can only be answered once",
				sessionID, event.Seq, payload.CallID, *payload.Turn, *payload.Step)
		}
		results[key] = renderTranscriptToolContent(payload.Preview, payload.IsError, payload.SpillLocator)
	}
	return results, nil
}

// collectToolCallArguments 扫一遍事件，把每条 tool/call 记下的 arguments 按
// (turn, step, call_id) 收起来。
//
// 单独收一张表而不是「边走边从 assistant 后面找最近的 tool/call」，理由与
// collectToolResults 相同：恢复补出的事件位置不可靠，只有键是可靠的。
//
// 撞键就报错、不覆盖：同一 (turn, step, call_id) 出现两条 tool/call 意味着同一次
// 调用被记了两遍，而没有任何合法路径能产生它——runtime 的 disambiguateCallIDs 在
// 单轮内去重，跨轮复用同一个 call_id 时 (turn, step) 不同，挂起→恢复那条路上待派发
// 的调用是在**新的 turn** 里才第一次被记录的（恢复分支手工重记的是 assistant 响应，
// 不是 tool/call）。撞了却挑一个用，模型就会读到一份张冠李戴的参数。
// 守卫：TestTwoToolCallEventsForTheSameCallAreRefused。
func collectToolCallArguments(sessionID string, events []domain.SessionEvent) (map[transcriptCallKey]string, error) {
	arguments := make(map[transcriptCallKey]string)
	for _, event := range events {
		if event.Type != domain.SessionEventToolCall {
			continue
		}
		var payload struct {
			// 指针的理由同 collectToolResults：turn 0 / step 0 是合法坐标。
			Turn      *int   `json:"turn"`
			Step      *int   `json:"step"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("project transcript for %q: decode tool/call at seq %d: %w",
				sessionID, event.Seq, err)
		}
		if strings.TrimSpace(payload.CallID) == "" {
			return nil, fmt.Errorf("project transcript for %q: tool/call at seq %d has no call_id, so its arguments can never be attached to the call that used them",
				sessionID, event.Seq)
		}
		if payload.Turn == nil || payload.Step == nil {
			return nil, fmt.Errorf("project transcript for %q: tool/call at seq %d for call_id %q has no turn/step, so it cannot be paired with the assistant message that announced it",
				sessionID, event.Seq, payload.CallID)
		}
		key := transcriptCallKey{turn: *payload.Turn, step: *payload.Step, callID: payload.CallID}
		if _, duplicate := arguments[key]; duplicate {
			return nil, fmt.Errorf("project transcript for %q: tool/call at seq %d is a second record of call_id %q in turn %d step %d; one call can only be dispatched once",
				sessionID, event.Seq, payload.CallID, *payload.Turn, *payload.Step)
		}
		arguments[key] = payload.Arguments
	}
	return arguments, nil
}

// argumentsTruncationMarker 是 runtime 的 truncateRunes 截断时缀在尾部的记号
// （internal/runtime/eventlog.go）。recordToolCall 按 maxEventPreviewRunes 截
// arguments，所以一段**合法**的 tool/call 载荷完全可能不是合法 JSON——write_file
// 带一整个文件正文的调用天天发生。
//
// 这个常量是这里唯一能把「契约允许的截断」与「载荷坏了」分开的依据。写死一份字面量
// 是有意的：storage 不该为了读一个记号去依赖 runtime。它若哪天变了，下面的分支会把
// 截断过的参数判成坏载荷并**报错**——响亮地错，而不是继续静默产出 `null`。
const argumentsTruncationMarker = "…[truncated:"

// truncatedArgumentsKey 是参数被截断时，那段截断文本挂在哪个键下。
//
// 名字以下划线开头，与任何工具的真实参数名区分开：模型读到它就知道这不是它当时
// 传的参数名，而是一段被截断的原文。
const truncatedArgumentsKey = "_truncated_arguments"

// decodeTranscriptCallArguments 把 tool/call 事件里的 arguments 还原成
// domain.ToolCall.Arguments 需要的键值表。
//
// 三种输入，三种处理，谁也不冒充谁：
//
//   - 空串：契约允许的「这次调用没有参数」。adapter 的入站方向对同一件事的处理就是
//     map[string]string{}（openAIToolCalls），出站编成 `{}`，正是 provider 要的对象。
//     **不是**兜底：空参数是 openAIChatToolCall 明确允许的状态。
//   - 合法 JSON 对象：原样解出来。值不是字符串的（数字、嵌套对象）按 adapter 入站
//     方向的同一规则 fmt.Sprint 成字符串——那一侧就是这么把 provider 的参数塞进
//     map[string]string 的，两边保持同一个损失面。
//   - 被 maxEventPreviewRunes 截断过：解不出来，但这是契约声明过的合法状态。把那段
//     截断原文挂在 truncatedArgumentsKey 下送给模型：它是一个合法 JSON 对象（发得
//     出去），内容是日志里真实存着的字节（没有替模型编造它没传过的参数）。
//
// 除此以外解不出来的载荷只可能来自坏掉的写入方，报错。
func decodeTranscriptCallArguments(sessionID string, seq int64, callID string, raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		if strings.Contains(raw, argumentsTruncationMarker) {
			return map[string]string{truncatedArgumentsKey: raw}, nil
		}
		return nil, fmt.Errorf("project transcript for %q: decode arguments of call_id %q recorded for assistant/message at seq %d: %w",
			sessionID, callID, seq, err)
	}
	// JSON 字面量 null 解进 map 得到 nil map，而 nil map 正是本条 Critical 的病根：
	// adapter 会把它编回 `null`。空对象是同一件事的合法线上形状。
	args := make(map[string]string, len(decoded))
	for key, value := range decoded {
		switch typed := value.(type) {
		case string:
			args[key] = typed
		default:
			args[key] = fmt.Sprint(typed)
		}
	}
	return args, nil
}

// renderTranscriptToolContent 把一条结果渲染成模型看到的文本。
//
// 只放**预览**，不放全文（spec §6：全文仍靠 read_file 按定位符取）——这正是 G3
// 的成本上界所在：预览在写入侧已按 maxEventPreviewRunes 截过，这里不再展开。
// 定位符会一并给出，让模型知道去哪取全文；出错的结果显式标出，否则模型会把一条
// 错误当成数据。
func renderTranscriptToolContent(preview string, isError bool, spillLocator string) string {
	var b strings.Builder
	if isError {
		b.WriteString("[错误] ")
	}
	b.WriteString(preview)
	if strings.TrimSpace(spillLocator) != "" {
		b.WriteString("\n[全文见 ")
		b.WriteString(spillLocator)
		b.WriteString("，用 read_file 按需翻页]")
	}
	return b.String()
}
