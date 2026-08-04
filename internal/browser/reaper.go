package browser

import (
	"context"
	"log/slog"
	"time"
)

// reapIdle 回收所有空闲超过 TTL 的会话：先落盘 storageState，再释放物理 Context、
// 置 Context=nil（会话记录仍在内存与 DB，供之后重建）。返回被回收的会话 id。
// SessionTTL<=0 时不回收。传入 now 便于测试注入。
//
// 选择判据只看空闲时长（now-LastUsedAt >= SessionTTL），不看 Context 是否为 nil——
// 对 nil Context 会话的物理释放安全跳过放在 evictSession 里（见其注释），
// 这样选择逻辑可在无 Chromium 的单测里用 nil Context 验证。
func (r *Runtime) reapIdle(now time.Time) []string {
	if r.cfg.SessionTTL <= 0 {
		return nil
	}
	var reaped []string
	for _, sess := range r.sessions.Snapshot() { // Snapshot: 复制指针，避免遍历时改 map / 持锁回收
		if now.Sub(sess.LastUsedAt) < r.cfg.SessionTTL {
			continue // 未空闲够久
		}
		r.evictSession(sess)
		reaped = append(reaped, sess.ID)
	}
	return reaped
}

// evictSession 落盘登录态 → 释放 Context → 置 nil。顺序与 Create/Delete 相反：先落盘后释放，
// 因为登录态不可丢——落盘失败就保留 Context 下次再试，绝不先把 Context 释放掉。
//
// 已无物理 Context（Context==nil：已回收或尚未建）时整体 no-op：既抓不到 cookies，也不应
// 用空快照覆盖此前已落盘的登录态，更无 Context 可释放。r.mgr==nil / persist==nil 均安全。
func (r *Runtime) evictSession(sess *Session) {
	if sess == nil || sess.Context == nil {
		return
	}
	// 1. 抓 cookies + 落盘（先落盘）。落盘任一步失败都保留 Context，不进入释放。
	if store := r.sessions.persist; store != nil {
		cookies, err := r.captureCookies(sess)
		if err != nil {
			// 抓 cookies 失败：保留 Context，避免用不完整快照覆盖已存登录态，下次再试。
			slog.Warn("browser: evict capture cookies failed — keeping Context", "session", sess.ID, "err", err)
			return
		}
		js, err := marshalStorageState(cookies)
		if err != nil {
			slog.Warn("browser: evict marshal storage state failed — keeping Context", "session", sess.ID, "err", err)
			return
		}
		if err := store.Save(SessionRecord{
			ID:           sess.ID,
			TaskID:       sess.TaskID,
			ActiveURL:    r.activeURLOf(sess),
			StorageState: js,
			CreatedAt:    sess.CreatedAt,
			LastUsedAt:   sess.LastUsedAt,
			Evicted:      true,
		}); err != nil {
			// 落盘失败：不释放 Context，避免丢登录态（登录态不可恢复），下次 tick 再试。
			slog.Warn("browser: evict persist storage state failed — keeping Context to avoid losing login", "session", sess.ID, "err", err)
			return
		}
	}
	// 2. 释放物理 Context（后释放）。释放失败记 Warn，但 Context 本就要弃用，仍置 nil。
	if r.mgr != nil {
		if err := r.mgr.ReleaseContext(sess.Context); err != nil {
			slog.Warn("browser: evict release context failed", "session", sess.ID, "err", err)
		}
	}
	r.stopScreencast(sess.ID) // Phase 2：会话没了视图也停
	sess.WithLock(func() {
		sess.Context = nil
		sess.ActivePage = nil
	})
}

// startReaper 起后台回收 goroutine，按 ReapInterval（默认 60s）扫描并回收空闲会话，
// 直到 ctx 取消。SessionTTL<=0（未启用 TTL）时直接返回，不起 goroutine。
func (r *Runtime) startReaper(ctx context.Context) {
	if r.cfg.SessionTTL <= 0 {
		return // 未启用 TTL 回收
	}
	interval := r.cfg.ReapInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				if reaped := r.reapIdle(t); len(reaped) > 0 {
					slog.Info("browser: reaped idle sessions", "count", len(reaped), "sessions", reaped)
				}
			}
		}
	}()
}
