package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

// maxSessionEventDataBytes 是单个事件载荷的上限。
//
// 它守的是 spec §4.3 不变量 6：事件表的增长与**调用次数**成正比，不与工具输出
// 体积成正比。超限的输出走 spill（internal/runtime/toolcache.go），事件里只留
// 预览与定位符。把这条守在存储层，是为了让写错的人在当场看见，而不是在库涨到
// 几个 G 之后才发现。
//
// 64 KiB 的取法：一条预览按现有截断治理是几 KiB 量级，assistant 消息含 tool_calls
// 时可能到几十 KiB。64 KiB 给了足够余量，同时离「一份完整工具输出」还差一个量级。
const maxSessionEventDataBytes = 64 << 10

// sessionWriteLocks 串行化同一会话的写入。
//
// 同会话的并发写是常态（并行工具返回、审批恢复与新消息相撞），而 seq 的连续性
// 是一个「读-改-写」不变量：两个写入方同时读到同一个 next-seq，就会一个成功、
// 一个撞主键失败。锁按会话切分，不同会话互不阻塞。
//
// 【不变量保护层级说明】
// 当前 SQLite 实现（sqlite.go）的 SetMaxOpenConns(1) 已把所有写入路径限制在单连接
// 上，连接池层面天然串行化了所有 BeginTx/Commit 操作，使得这把锁在今天的配置下形式
// 上冗余（删掉锁测试仍 5/5 通过）。但连接池宽度不是本文件能依赖的契约。验证表明：
// 一旦放宽连接池（实测 MaxOpenConns=4），同一变异（删掉这把锁）会 5/5 稳定失败，
// 证实锁是 seq 连续性在多连接配置下的唯一防线。因此保留之，并**不要因为「现在删掉
// 测试还绿」就把它删了**——那只反映了当前配置的特殊性，不代表锁本身逻辑冗余或不必要。
type sessionWriteLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *sessionWriteLocks) get(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	lock, ok := s.locks[sessionID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[sessionID] = lock
	}
	return lock
}

// Append 追加一批事件（spec §4.3 不变量 1、4、5、6）。
//
// 整批在一个事务里：中途失败整批回滚，不留半批——半批写入留下的日志读得出来、
// 也验得过，却与真实发生的事不符，那种损坏比读不出来更难发现。
//
// 空批次是合法的无操作（懒物化：没有事件的会话不在表里留痕）。
func (r *SQLiteRepository) Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	if len(events) == 0 {
		return nil
	}
	for i, event := range events {
		if err := domain.ValidateSessionEventType(event.Type); err != nil {
			return fmt.Errorf("append session events for %q: %w", sessionID, err)
		}
		if len(event.Data) > maxSessionEventDataBytes {
			return fmt.Errorf("append session events for %q: event %d (%s) carries %d bytes, "+
				"over the %d-byte limit; large tool output belongs in spill with only a preview "+
				"and locator in the event",
				sessionID, event.Seq, event.Type, len(event.Data), maxSessionEventDataBytes)
		}
		if !json.Valid(event.Data) {
			return fmt.Errorf("append session events for %q: event %d (%s) carries data that is not "+
				"valid JSON; writing it would make this session unreadable forever, since the read "+
				"path rejects malformed JSON for every event it decodes",
				sessionID, event.Seq, event.Type)
		}
		// 批内连续性：只验首条对上库里的 next-seq 还不够——首条对了，批内后续元素
		// 仍可能跳号，在日志中间留下一个永久空洞（读到它就判定整条会话损坏）。
		if i > 0 && event.Seq != events[i-1].Seq+1 {
			return fmt.Errorf("append session events for %q: batch element %d has seq %d, want %d "+
				"(must follow element %d's seq %d contiguously); a gap here would leave a permanent "+
				"hole in the log and the session could never be rebuilt into a unique history",
				sessionID, i, event.Seq, events[i-1].Seq+1, i-1, events[i-1].Seq)
		}
	}

	lock := r.sessionEventLocks.get(sessionID)
	lock.Lock()
	defer lock.Unlock()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append session events for %q: %w", sessionID, err)
	}
	defer tx.Rollback()

	next, err := nextSeqTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if events[0].Seq != next {
		return fmt.Errorf("append session events for %q: first seq is %d but the log continues at %d; "+
			"the log is append-only and its seq must stay contiguous",
			sessionID, events[0].Seq, next)
	}

	for _, event := range events {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_events (session_id, seq, type, time, data)
			VALUES (?, ?, ?, ?, ?)
		`, sessionID, event.Seq, string(event.Type), event.Time.UnixMilli(), string(event.Data)); err != nil {
			return fmt.Errorf("append session event %d (%s) for %q: %w", event.Seq, event.Type, sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session events for %q: %w", sessionID, err)
	}
	return nil
}

// nextSeqTx 在事务内读该会话的下一个 seq。
//
// 权威值来自库而不是内存游标：少一个可能与库不一致的状态。主键覆盖这次查询，
// 代价可忽略。
func nextSeqTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq)+1, 0) FROM session_events WHERE session_id = ?`, sessionID,
	).Scan(&next); err != nil {
		return 0, fmt.Errorf("read next seq for %q: %w", sessionID, err)
	}
	return next, nil
}

// decodeSessionEvent 把一行还原成事件。类型在这里再验一次：读到未知类型说明这条
// 日志是另一个版本写的，静默跳过会让重建出的历史缺一段（spec §4.3 不变量 4）。
func decodeSessionEvent(sessionID string, seq int64, typ string, millis int64, data string) (domain.SessionEvent, error) {
	eventType := domain.SessionEventType(typ)
	if err := domain.ValidateSessionEventType(eventType); err != nil {
		return domain.SessionEvent{}, fmt.Errorf("read session event %d for %q: %w", seq, sessionID, err)
	}
	if !json.Valid([]byte(data)) {
		return domain.SessionEvent{}, fmt.Errorf("read session event %d (%s) for %q: data is not valid JSON",
			seq, typ, sessionID)
	}
	return domain.SessionEvent{
		Seq:  seq,
		Type: eventType,
		Time: time.UnixMilli(millis),
		Data: json.RawMessage(data),
	}, nil
}

// ReadFrom 返回 seq >= fromSeq 的事件，按 seq 升序（spec §4.4）。
//
// **不改库**：轨迹的翻页与增量拉取走这条路，而一次「看一眼」不该改变被看的东西。
// 崩溃恢复只发生在 Load 里。
//
// 返回的这段必须自身连续：中间有洞说明日志损坏，报错而不是把缺口当成「本来就这样」
// （spec §4.3 不变量 3）。
func (r *SQLiteRepository) ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error) {
	if fromSeq < 0 {
		return nil, fmt.Errorf("read session events for %q: fromSeq %d is negative", sessionID, fromSeq)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, type, time, data
		FROM session_events
		WHERE session_id = ? AND seq >= ?
		ORDER BY seq
	`, sessionID, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("read session events for %q: %w", sessionID, err)
	}
	defer rows.Close()

	var events []domain.SessionEvent
	var expected int64 = -1
	for rows.Next() {
		var (
			seq    int64
			typ    string
			millis int64
			data   string
		)
		if err := rows.Scan(&seq, &typ, &millis, &data); err != nil {
			return nil, fmt.Errorf("scan session event for %q: %w", sessionID, err)
		}
		if expected >= 0 && seq != expected {
			return nil, fmt.Errorf("session log for %q is broken: seq jumps from %d to %d; "+
				"a gap means the log no longer reconstructs one history", sessionID, expected-1, seq)
		}
		event, err := decodeSessionEvent(sessionID, seq, typ, millis, data)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
		expected = seq + 1
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session events for %q: %w", sessionID, err)
	}
	// Append 保证这条日志从 0 开始稠密连续，所以只要有行返回，第一行的 seq 必须
	// 恰好等于 fromSeq——差一步都说明 [fromSeq, events[0].Seq) 这段被削掉了。
	// 上面的相邻行检查只验「已返回的行之间」，天生漏掉窗口起点本身这个洞：
	// fromSeq 落在一个洞里时，查询直接跳到洞后面的第一行，expected 的初值 -1
	// 会放过这一行的检查，调用方就会以为「从 fromSeq 开始就是这样」，而不知道
	// fromSeq 到 events[0].Seq 之间曾经存在过又消失了。
	if len(events) > 0 && events[0].Seq != fromSeq {
		return nil, fmt.Errorf("session log for %q is broken: requested from seq %d but the first "+
			"event actually present is seq %d; the gap in between means the log no longer "+
			"reconstructs one history", sessionID, fromSeq, events[0].Seq)
	}
	return events, nil
}

// recoveryPlan 是一个未收尾日志需要补上的事件（尚未分配 seq）。
type recoveryPlan struct {
	unansweredCalls []unansweredCall
	needStepEnd     bool
	stepTurn        int
	stepStep        int
	needTurnEnd     bool
	turnNumber      int
}

// unansweredCall 是一个发出去、却没有结果的工具调用。
type unansweredCall struct {
	callID string
	turn   int
	step   int
}

// Load 返回该会话的全部事件，必要时先把崩溃留下的半个 turn 补成合法的
// provider transcript 并落盘（spec §4.3 不变量 2）。
//
// **为什么补而不是截断**：截掉半个 turn 会丢掉真实发生过的事——那些工具是真的
// 执行了、副作用是真的产生了。补的做法保留它们，只是把「没等到结果」这件事
// 记成一条 is_error 的结果，让重建出的消息数组仍然合法：每个 tool_call 都有
// 与之对应的 tool 消息。
//
// 幂等：已经平衡的日志原样返回，不追加任何东西。
func (r *SQLiteRepository) Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error) {
	events, err := r.ReadFrom(ctx, sessionID, 0)
	if err != nil {
		return nil, err
	}
	plan, err := planRecovery(events)
	if err != nil {
		return nil, fmt.Errorf("recover session %q: %w", sessionID, err)
	}
	if len(plan.unansweredCalls) == 0 && !plan.needStepEnd && !plan.needTurnEnd {
		return events, nil
	}

	synthesized, err := synthesizeClosers(plan, int64(len(events)), lastTimeOf(events))
	if err != nil {
		return nil, fmt.Errorf("recover session %q: %w", sessionID, err)
	}
	if err := r.Append(ctx, sessionID, synthesized); err != nil {
		return nil, fmt.Errorf("persist recovery for session %q: %w", sessionID, err)
	}
	return append(events, synthesized...), nil
}

// planRecovery 判断一个日志缺什么收尾事件。
//
// 判据只看事件本身：有 turn/start 而没有对应的 turn/end 就是没收尾；
// tool/call 没有同 call_id 的 tool/result 就是没答。
//
// 每个字段读取都可能失败（intField/stringField 返回 error）：这些字段在
// spec §4.1 里是必填的，取不到说明这条日志本身有问题，不是「缺省当默认值」
// 能糊弄过去的情况，所以一路把 error 上报，不吞。
func planRecovery(events []domain.SessionEvent) (recoveryPlan, error) {
	var plan recoveryPlan
	pending := map[string]unansweredCall{}
	var order []string
	turnOpen, stepOpen := false, false

	for _, event := range events {
		switch event.Type {
		case domain.SessionEventTurnStart:
			turnOpen = true
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			plan.turnNumber = turn
		case domain.SessionEventTurnEnd:
			turnOpen = false
		case domain.SessionEventStepStart:
			stepOpen = true
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			step, err := intField(event.Data, "step")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			plan.stepTurn, plan.stepStep = turn, step
		case domain.SessionEventStepEnd:
			stepOpen = false
		case domain.SessionEventToolCall:
			id, err := stringField(event.Data, "call_id")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			turn, err := intField(event.Data, "turn")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			step, err := intField(event.Data, "step")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			pending[id] = unansweredCall{callID: id, turn: turn, step: step}
			order = append(order, id)
		case domain.SessionEventToolResult:
			id, err := stringField(event.Data, "call_id")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			delete(pending, id)
		}
	}
	for _, id := range order {
		if call, ok := pending[id]; ok {
			plan.unansweredCalls = append(plan.unansweredCalls, call)
		}
	}
	plan.needStepEnd = stepOpen
	plan.needTurnEnd = turnOpen
	return plan, nil
}

// synthesizeClosers 按 spec §4.3 不变量 2 的顺序造出补齐事件：
// 每个未答调用一条 is_error 的 tool/result，然后 step/end，最后 turn/end{interrupted}。
func synthesizeClosers(plan recoveryPlan, nextSeq int64, at time.Time) ([]domain.SessionEvent, error) {
	var out []domain.SessionEvent
	seq := nextSeq
	for _, call := range plan.unansweredCalls {
		data, err := json.Marshal(map[string]any{
			"turn": call.turn, "step": call.step, "call_id": call.callID,
			"preview":     "the agent stopped before this tool returned; its result was never recorded",
			"is_error":    true,
			"duration_ms": 0,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic tool result for %q: %w", call.callID, err)
		}
		out = append(out, domain.SessionEvent{
			Seq: seq, Type: domain.SessionEventToolResult, Time: at, Data: data,
		})
		seq++
	}
	if plan.needStepEnd {
		data, err := json.Marshal(map[string]any{
			"turn": plan.stepTurn, "step": plan.stepStep, "reason": domain.StepEndReasonCancelled,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic step end: %w", err)
		}
		out = append(out, domain.SessionEvent{Seq: seq, Type: domain.SessionEventStepEnd, Time: at, Data: data})
		seq++
	}
	if plan.needTurnEnd {
		data, err := json.Marshal(map[string]any{
			"turn": plan.turnNumber, "reason": domain.TurnEndReasonInterrupted,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal synthetic turn end: %w", err)
		}
		out = append(out, domain.SessionEvent{Seq: seq, Type: domain.SessionEventTurnEnd, Time: at, Data: data})
	}
	return out, nil
}

// lastTimeOf 用最后一条真实事件的时间给补出来的事件打时间戳：它们描述的是
// 那一刻发生的中断，而不是「现在」。空日志不会走到恢复路径。
func lastTimeOf(events []domain.SessionEvent) time.Time {
	if len(events) == 0 {
		return time.UnixMilli(0)
	}
	return events[len(events)-1].Time
}

// intField 从事件载荷里取一个必填的数值字段。
//
// **不吞错**（CLAUDE.md §0）：spec §4.1 规定这些字段必填，取不到说明这条日志
// 本身有问题。返回零值接着走，等于让恢复出的事件带着编造的 turn/step/call_id
// ——那正是「凑个值接着跑」。所以返回 error，由 planRecovery 一路上报。
func intField(data json.RawMessage, name string) (int, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("decode event payload for field %q: %w", name, err)
	}
	value, ok := payload[name].(float64)
	if !ok {
		return 0, fmt.Errorf("event payload has no numeric %q field", name)
	}
	return int(value), nil
}

// stringField 从事件载荷里取一个必填的字符串字段。同 intField 的不吞错理由。
func stringField(data json.RawMessage, name string) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode event payload for field %q: %w", name, err)
	}
	value, ok := payload[name].(string)
	if !ok {
		return "", fmt.Errorf("event payload has no string %q field", name)
	}
	return value, nil
}
