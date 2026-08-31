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
