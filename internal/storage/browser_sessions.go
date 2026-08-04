package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BrowserSessionRecord 是一个内置浏览器会话的持久化快照（登录态 = cookies JSON）。
type BrowserSessionRecord struct {
	ID           string
	TaskID       string
	ActiveURL    string
	StorageState string // cookies 的 JSON 序列化（[]NetworkCookieParam 形状）
	CreatedAt    time.Time
	LastUsedAt   time.Time
	Evicted      bool // 物理 Context 已被回收，下次访问需重建
}

// SaveBrowserSession upsert 整条记录（用于回收/Close 时写入含 storage_state 的完整快照）。
func (r *SQLiteRepository) SaveBrowserSession(ctx context.Context, rec BrowserSessionRecord) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO browser_sessions (id, task_id, active_url, storage_state, created_at, last_used_at, evicted)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			task_id = excluded.task_id,
			active_url = excluded.active_url,
			storage_state = excluded.storage_state,
			last_used_at = excluded.last_used_at,
			evicted = excluded.evicted
	`, rec.ID, rec.TaskID, rec.ActiveURL, rec.StorageState,
		formatTime(rec.CreatedAt), formatTime(rec.LastUsedAt), boolToInt(rec.Evicted))
	if err != nil {
		return fmt.Errorf("save browser session %q: %w", rec.ID, err)
	}
	return nil
}

// TouchBrowserSession 只更新 active_url 与 last_used_at（字段级写穿，绝不触碰
// storage_state——动作路径没有新 cookies 快照，全行 UPSERT 会用空串覆盖登录态）。
// 记录不存在时报错（fail-loud：touch 一个未持久化的会话是调用方 bug）。
func (r *SQLiteRepository) TouchBrowserSession(ctx context.Context, id, activeURL string, lastUsed time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE browser_sessions SET active_url = ?, last_used_at = ?, evicted = 0 WHERE id = ?
	`, activeURL, formatTime(lastUsed), id)
	if err != nil {
		return fmt.Errorf("touch browser session %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch browser session %q: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("touch browser session %q: no such session", id)
	}
	return nil
}

// GetBrowserSession 按 id 读取一条会话记录；ok=false 表示记录不存在（契约允许）。
func (r *SQLiteRepository) GetBrowserSession(ctx context.Context, id string) (BrowserSessionRecord, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, active_url, storage_state, created_at, last_used_at, evicted
		FROM browser_sessions WHERE id = ?
	`, id)
	rec, err := scanBrowserSession(row)
	if err == sql.ErrNoRows {
		return BrowserSessionRecord{}, false, nil
	}
	if err != nil {
		return BrowserSessionRecord{}, false, fmt.Errorf("get browser session %q: %w", id, err)
	}
	return rec, true, nil
}

// ListBrowserSessions 返回全部持久化会话，按 last_used_at 倒序。
func (r *SQLiteRepository) ListBrowserSessions(ctx context.Context) ([]BrowserSessionRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, active_url, storage_state, created_at, last_used_at, evicted
		FROM browser_sessions ORDER BY last_used_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list browser sessions: %w", err)
	}
	defer rows.Close()
	var out []BrowserSessionRecord
	for rows.Next() {
		rec, err := scanBrowserSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan browser session: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list browser sessions: %w", err)
	}
	return out, nil
}

// DeleteBrowserSession 删除一条会话记录（幂等：不存在也不报错）。
func (r *SQLiteRepository) DeleteBrowserSession(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM browser_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete browser session %q: %w", id, err)
	}
	return nil
}

type browserSessionRowScanner interface{ Scan(dest ...any) error }

func scanBrowserSession(s browserSessionRowScanner) (BrowserSessionRecord, error) {
	var rec BrowserSessionRecord
	var created, lastUsed string
	var evicted int
	if err := s.Scan(&rec.ID, &rec.TaskID, &rec.ActiveURL, &rec.StorageState, &created, &lastUsed, &evicted); err != nil {
		return BrowserSessionRecord{}, err
	}
	var err error
	if rec.CreatedAt, err = parseTime(created); err != nil {
		return BrowserSessionRecord{}, fmt.Errorf("parse created_at %q: %w", created, err)
	}
	if rec.LastUsedAt, err = parseTime(lastUsed); err != nil {
		return BrowserSessionRecord{}, fmt.Errorf("parse last_used_at %q: %w", lastUsed, err)
	}
	rec.Evicted = evicted != 0
	return rec, nil
}
