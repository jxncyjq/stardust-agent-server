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
	for _, event := range events {
		if err := domain.ValidateSessionEventType(event.Type); err != nil {
			return fmt.Errorf("append session events for %q: %w", sessionID, err)
		}
		if len(event.Data) > maxSessionEventDataBytes {
			return fmt.Errorf("append session events for %q: event %d (%s) carries %d bytes, "+
				"over the %d-byte limit; large tool output belongs in spill with only a preview "+
				"and locator in the event",
				sessionID, event.Seq, event.Type, len(event.Data), maxSessionEventDataBytes)
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
