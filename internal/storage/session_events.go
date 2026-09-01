package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// 编译期保证 SQLiteRepository 满足端口契约。
//
// 这一行的作用是让「方法签名改了但端口没改」在**编译时**就停下来，而不是等到
// 装配时才发现某个实现悄悄不再满足接口。
var _ port.SessionEventStore = (*SQLiteRepository)(nil)

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
//
// 【已知上界】locks 只增不减：上界 = 本进程见过的会话数，每个条目是一个
// sync.Mutex（几十字节 + 一个 map 槽位）。正确的清理需要引用计数或 TTL，两者
// 都得先解决「锁被持有时不能删」的竞态，现在做等于在没有真实会话基数的情况下
// 猜。触发条件：P2 把发射点接上、有了真实的会话基数之后，再评估是否需要清理。
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
	lock := r.sessionEventLocks.get(sessionID)
	lock.Lock()
	defer lock.Unlock()
	return r.appendLocked(ctx, sessionID, events)
}

// appendLocked 是 Append 的锁内主体。**调用方必须已持有该会话的写锁**。
//
// 拆出这一层是为了 Load：它的「读 → 规划恢复 → 追加」是一个读-改-写，三步必须
// 在同一把锁下完成，否则两个并发的 Load 会各自读到同一份未收尾日志、各自去追加
// （见 Load 的注释）。Load 不能改调 Append——那会在已持锁时再取同一把锁，直接
// 死锁。
//
// 校验整体留在锁内：Load 合成出来的事件也要走完与外部写入完全相同的六道校验，
// 少一道就等于给恢复路径开了一个绕过写侧不变量的后门。
func (r *SQLiteRepository) appendLocked(ctx context.Context, sessionID string, events []domain.SessionEvent) error {
	if len(events) == 0 {
		return nil
	}
	for i, event := range events {
		// 带上 seq 与批内下标：同一个循环里其余三条校验都指得出「哪一条」，
		// 只有这条不指，拿到错误的人得自己回去数是批里的第几个。
		if err := domain.ValidateSessionEventType(event.Type); err != nil {
			return fmt.Errorf("append session events for %q: event %d (batch element %d): %w",
				sessionID, event.Seq, i, err)
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
	if err := indexSessionEvents(ctx, tx, sessionID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session events for %q: %w", sessionID, err)
	}
	return nil
}

// searchOwner 是一条工具类事件挂靠的对话身份。
//
// tool/call 与 tool/result 的载荷里**没有** task_id / agent_id（见
// internal/runtime/eventlog.go 的 recordToolCall / recordToolResult：它们只带
// turn/step/call_id 与文本）。可它们检索出来必须能被定位到某一行 turn 上，否则
// discovery 搜到一次工具往返、模型却无处 scroll。身份因此从日志里**紧邻在前**的
// 那条消息事件继承——一次工具调用属于发起它的那个任务，而同一条会话日志里一个
// 任务的事件是连续的（会话执行锁保证一次只有一个任务在写，见 projectTurns 的
// 「折叠不改变顺序」一节）。
type searchOwner struct {
	taskID  string
	agentID string
}

// searchRow 是一条事件写进 session_events_fts 的那一行。
//
// 没有 role 字段：角色由 type 列在读侧推出（见 searchHitRole），一处决定、不存两份。
type searchRow struct {
	text    string
	turnID  string
	taskID  string
	agentID string
}

// indexSessionEvents 把一批事件的可搜文本镜进 FTS5 索引。它在调用方的事务里运行，
// 于是索引与事件一起提交；失败包装后返回而不是吞掉，事件永远不会「存进去了却搜不到」
// ——与 indexConversationTurn 立下的规矩同一条（"a turn is never persisted without
// being searchable"）。
//
// 可搜的是四类：user/assistant 的正文，以及 tool/call 的名字与参数、tool/result 的
// 预览——最后两类正是 H1 要解决的「工具往返从不可搜」。turn/step 的边界事件没有可搜
// 文本，跳过它们不是遗漏：它们本来就没有内容可搜。
//
// 工具类事件的身份按批内顺序继承：走到一条消息事件就更新 owner，走到工具事件就用
// 当前 owner。批内还没出现过消息事件时（tool/call 落盘的屏障 2 会让它单独成批），
// 回库里查这批之前最后一条消息事件。
//
// **调用方必须已经把这批事件插进 session_events**：owner 的回查按 seq 严格早于本批
// 首条，插入顺序因此不影响结果，但事务里少了那些行就意味着索引在给一批不存在的事件
// 建索引。
func indexSessionEvents(ctx context.Context, tx *sql.Tx, sessionID string, events []domain.SessionEvent) error {
	var (
		owner      searchOwner
		ownerKnown bool
	)
	for _, event := range events {
		if event.Type == domain.SessionEventToolCall || event.Type == domain.SessionEventToolResult {
			if !ownerKnown {
				resolved, err := loadSearchOwner(ctx, tx, sessionID, events[0].Seq)
				if err != nil {
					return fmt.Errorf("index session event %d of %q for search: %w", event.Seq, sessionID, err)
				}
				owner, ownerKnown = resolved, true
			}
		}
		row, searchable, err := searchableText(event, owner)
		if err != nil {
			return fmt.Errorf("index session event %d of %q for search: %w", event.Seq, sessionID, err)
		}
		if event.Type == domain.SessionEventUserMessage || event.Type == domain.SessionEventAssistantMessage {
			owner, ownerKnown = searchOwner{taskID: row.taskID, agentID: row.agentID}, true
		}
		if !searchable {
			continue
		}
		if err := indexSessionEvent(ctx, tx, sessionID, event, row); err != nil {
			return err
		}
	}
	return nil
}

// indexSessionEvent 写一行 FTS 索引。
func indexSessionEvent(ctx context.Context, ex execer, sessionID string, event domain.SessionEvent, row searchRow) error {
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO session_events_fts (
			content, session_id, seq, type, turn_id, task_id, agent_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, row.text, sessionID, event.Seq, string(event.Type),
		row.turnID, row.taskID, row.agentID, formatTime(event.Time)); err != nil {
		return fmt.Errorf("index session event %d of %q for search: %w", event.Seq, sessionID, err)
	}
	return nil
}

// searchableText 取出一条事件的可搜文本与身份字段，第二个返回值说明这一类事件是否
// 可搜。
//
// owner 只被工具类事件用到（见 searchOwner）。
//
// 未知事件类型**报错**而不是当作不可搜跳过：Append 已经用
// domain.ValidateSessionEventType 拦掉了这个构建不认得的类型，所以走到 default 的
// 只可能是「domain 加了新类型、却没人在这里决定它可不可搜」——与 projectTurns 的
// default 同一个姿态，让那次遗漏在当场停下，而不是让一类事件从此静静地搜不到。
func searchableText(event domain.SessionEvent, owner searchOwner) (searchRow, bool, error) {
	switch event.Type {
	case domain.SessionEventUserMessage, domain.SessionEventAssistantMessage:
		var payload struct {
			TurnID  string `json:"turn_id"`
			TaskID  string `json:"task_id"`
			AgentID string `json:"agent_id"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return searchRow{}, false, fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		return searchRow{
			text:    payload.Content,
			turnID:  payload.TurnID,
			taskID:  payload.TaskID,
			agentID: payload.AgentID,
		}, true, nil

	case domain.SessionEventToolCall:
		var payload struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return searchRow{}, false, fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		// 工具名与参数一起进正文：按工具名找（"哪次 read_file"）与按参数找
		// （"哪次读了 kubernetes.yaml"）是同一个检索需求的两半。
		return searchRow{
			text:    payload.Name + " " + payload.Arguments,
			taskID:  owner.taskID,
			agentID: owner.agentID,
		}, true, nil

	case domain.SessionEventToolResult:
		var payload struct {
			Preview string `json:"preview"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return searchRow{}, false, fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		return searchRow{
			text:    payload.Preview,
			taskID:  owner.taskID,
			agentID: owner.agentID,
		}, true, nil

	case domain.SessionEventTurnStart, domain.SessionEventTurnEnd,
		domain.SessionEventStepStart, domain.SessionEventStepEnd:
		return searchRow{}, false, nil

	default:
		return searchRow{}, false, fmt.Errorf("unknown event type %q has no searchability decision", event.Type)
	}
}

// loadSearchOwner 回查 beforeSeq 之前最后一条消息事件的身份，供工具类事件继承。
//
// 一条会话日志里的工具往返总跟在消息事件后面（turn/start → user/message →
// step/start → assistant/message → tool/call → tool/result），所以生产上这条查询
// 必有结果。查不到只发生在**人工构造的**日志里（例如一条以 tool/result 开头的
// 日志）：那种日志的工具事件本来就无从归属，投影也不会为它产出任何 turn。这里
// 如实记下「没有身份」而不是编一个，写侧照旧接受这批事件（P1 的 Append 契约只管
// 类型/体积/JSON/seq 四道校验，不管载荷完整性）；真正的 fail-loud 落在读侧——
// SearchMessages 命中一行没有 task_id 的索引时会指名报错，因为那样的命中拼不出
// 可回访的 turn 地址。
func loadSearchOwner(ctx context.Context, tx *sql.Tx, sessionID string, beforeSeq int64) (searchOwner, error) {
	var data string
	err := tx.QueryRowContext(ctx, `
		SELECT data FROM session_events
		WHERE session_id = ? AND seq < ? AND type IN (?, ?)
		ORDER BY seq DESC
		LIMIT 1
	`, sessionID, beforeSeq,
		string(domain.SessionEventUserMessage), string(domain.SessionEventAssistantMessage),
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return searchOwner{}, nil
	}
	if err != nil {
		return searchOwner{}, fmt.Errorf("read the message event owning seq %d: %w", beforeSeq, err)
	}
	var payload struct {
		TaskID  string `json:"task_id"`
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return searchOwner{}, fmt.Errorf("decode the message event owning seq %d: %w", beforeSeq, err)
	}
	return searchOwner{taskID: payload.TaskID, agentID: payload.AgentID}, nil
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
//
// **调用契约：只可对「确定没有活跃写入者」的会话调用**——进程启动时的崩溃恢复，
// 或一个已经结束的会话。存储层看得见的只有事件本身，而「崩掉的半个 turn」与
// 「正在跑、还没收尾的 turn」在数据上完全等价，本层没有任何办法区分。对一个活着
// 的会话调 Load，会往那个进行中的 turn 里注入 tool/result{is_error}、step/end 与
// turn/end{interrupted}，把它强行收成中断。这条约束 P1 修不掉，已按 spec §4.3.1
// 的先例写进 spec 交给 P2 的实现者。
//
// 读-规划-追加三步在同一把 per-session 写锁下完成（spec §4.4「同会话写入经同一条
// 串行链」）。锁外做这个读-改-写的后果不是数据损坏——Append 的首条对齐会拦住——
// 而是两个并发 Load 里后到的那个拿到一句指向 Append 写入语义、与真实原因（另一个
// Load 抢先完成了恢复）毫无关系的错误，排查成本极高。
func (r *SQLiteRepository) Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error) {
	lock := r.sessionEventLocks.get(sessionID)
	lock.Lock()
	defer lock.Unlock()

	// ReadFrom 不取这把锁（它是纯读路径），所以这里不会重入。
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

	// 走到这里 events 必非空：空日志的 plan 三项全空，上面已经返回。
	// next-seq 用最后一条的 Seq+1 而不是 len(events)：两者只在「从 0 起稠密连续」
	// 时相等，那个前提由 ReadFrom(0) 的两道断裂检测保证，但那层耦合只在读者脑子里。
	// 时间戳同理直接取最后一条真实事件的时间——补出来的事件描述的是那一刻发生的
	// 中断，不是「现在」。
	last := events[len(events)-1]
	synthesized, err := synthesizeClosers(plan, last.Seq+1, last.Time)
	if err != nil {
		return nil, fmt.Errorf("recover session %q: %w", sessionID, err)
	}
	if err := r.appendLocked(ctx, sessionID, synthesized); err != nil {
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
//
// 同样地，日志本身的结构异常（没有 start 的 end）也一律报错而不是就地纠正：
// 它们说明上游发事件出了问题，静默修正只会把问题挪到更远的地方（见下方注释）。
//
// call_id 重复的检查范围是**会被合成结果的那些未答调用**，不是整条会话
// （spec §4.3.1 第 4 条）：provider 的 tool call id 只保证单次响应内唯一，
// 一条完全平衡的长会话（每次调用都有自己的 tool/result）跨 turn 复用同一个
// call_id 是允许的、也是常见的（按序号生成 id 的 provider/本地模型不保证
// 跨会话唯一）——已应答的调用不进 pending，天然不参与恢复合成，重复无害。
// 只有当同一个 call_id 同时存在两个（或更多）都没等到结果的 tool/call 时，
// 恢复才会真的补出两条同 call_id 的 tool/result，那才是需要拦下的冲突；
// 见下方 openCount 的用法与收尾处的判定。
func planRecovery(events []domain.SessionEvent) (recoveryPlan, error) {
	var plan recoveryPlan
	pending := map[string]unansweredCall{}
	// openCount 记下每个 call_id 当前还有多少条 tool/call 没等到 tool/result。
	// 冲突判定用的是这个「最终仍未清零」的计数，不是「出现过两次」——见函数级
	// 注释与收尾处的判定。
	openCount := map[string]int{}
	// callSeqs 记下每个 call_id 每次以 tool/call 出现的 seq，只用于真的冲突时
	// 把「哪几条」指给排查的人看，不参与冲突判定本身。
	callSeqs := map[string][]int64{}
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
			// 没有 turn/start 的 turn/end 是损坏（P2 漏发一个发射点就长这样）。
			// 无条件置 false 会把这样一条日志判成「已收尾」而一条都不补——
			// 把损坏当正常，正是 CLAUDE.md §0 禁的「非预期状态被吞」。
			if !turnOpen {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): turn/end without a matching "+
					"turn/start; the log does not describe one well-formed turn sequence",
					event.Seq, event.Type)
			}
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
			// 同 turn/end：没有配对 step/start 的 step/end 是损坏，不是「已收尾」。
			if !stepOpen {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): step/end without a matching "+
					"step/start; the log does not describe one well-formed step sequence",
					event.Seq, event.Type)
			}
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
			callSeqs[id] = append(callSeqs[id], event.Seq)
			openCount[id]++
			pending[id] = unansweredCall{callID: id, turn: turn, step: step}
			order = append(order, id)
		case domain.SessionEventToolResult:
			id, err := stringField(event.Data, "call_id")
			if err != nil {
				return recoveryPlan{}, fmt.Errorf("event %d (%s): %w", event.Seq, event.Type, err)
			}
			delete(pending, id)
			if openCount[id] > 0 {
				openCount[id]--
			}
		}
	}

	// 收尾这一步才判定冲突，而不是收集阶段见到第二次 tool/call 就报——判的是
	// 「最终会被合成结果的那些未答调用」，不是「整条会话里出现过的 call_id」
	// （spec §4.3.1 第 4 条）。emitted 去重：同一个 id 即便在 order 里因为
	// 「答了又被同 id 再次调用」出现多次，只要它最终没有两个同时未答的实例，
	// 就只该合成一条 tool/result。
	emitted := map[string]bool{}
	for _, id := range order {
		if emitted[id] {
			continue
		}
		call, ok := pending[id]
		if !ok {
			continue
		}
		if openCount[id] >= 2 {
			seqs := callSeqs[id]
			return recoveryPlan{}, fmt.Errorf("event %d (%s): call_id %q has %d unanswered tool/call "+
				"events still outstanding (first at event %d, most recent at event %d); an unanswered "+
				"tool/call must not reuse a call_id, otherwise recovery would synthesize two tool "+
				"results carrying the same call_id and the rebuilt transcript could never be paired "+
				"up by call_id (a call_id that was already answered may be reused safely)",
				seqs[len(seqs)-1], domain.SessionEventToolCall, id, openCount[id], seqs[0], seqs[len(seqs)-1])
		}
		plan.unansweredCalls = append(plan.unansweredCalls, call)
		emitted[id] = true
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
