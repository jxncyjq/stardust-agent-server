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
		// LastUsedAt 在会话锁下读：动作路径（touch/Open/Read/Click/Type）在锁下写它，
		// reaper 在锁下读它，二者对该字段的访问互斥，避免数据竞争（-race clean）。
		var last time.Time
		sess.WithLock(func() { last = sess.LastUsedAt })
		if now.Sub(last) < r.cfg.SessionTTL {
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
// 关键并发不变量：整段「读 Context → 抓 cookies → 落盘 →（失败即返回不释放）→ ReleaseContext →
// 置 Context/ActivePage=nil」都在该会话锁（sess.mu）下执行。动作路径（Open/Read/Click/Type）
// 同样在会话锁下读写 Context/ActivePage，故本回收与在途动作严格串行，杜绝 reaper 把 Context
// 置 nil 与 Open 读 Context 交错造成的数据竞争 / nil 解引用崩溃。持锁跨 captureCookies(go-rod)
// 与 ReleaseContext 是有意为之——回收要独占该会话。
//
// 已无物理 Context（Context==nil：已回收或尚未建）时整体 no-op：既抓不到 cookies，也不应
// 用空快照覆盖此前已落盘的登录态，更无 Context 可释放。r.mgr==nil / persist==nil 均安全。
//
// stopScreencast 放在锁外调用：它只取 screencaster 自身的锁（非 sess.mu 也非 lifecycle 锁），
// 且既有 Subscribe/restart 路径均按 lifecycleMu→sess.mu 顺序加锁；在锁外停帧流保持该顺序，
// 避免任何锁序反转。
func (r *Runtime) evictSession(sess *Session) {
	if sess == nil {
		return
	}
	var released bool
	sess.WithLock(func() {
		if sess.Context == nil {
			return // 无物理 Context：无可抓、无可释放，整体 no-op
		}
		// 1. 抓 cookies + 落盘（先落盘）。落盘任一步失败都保留 Context，不进入释放。
		if store := r.sessions.persist; store != nil {
			cookies, err := r.captureCookies(sess) // 已持 sess.mu，captureCookies 不再自锁
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
				ActiveURL:    sess.ActiveURL, // 已持锁，直接读（不可再调 activeURLOf——会再入死锁）
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
		sess.Context = nil
		sess.ActivePage = nil
		released = true
	})
	if released {
		r.stopScreencast(sess.ID) // Phase 2：会话没了视图也停（锁外，见上方锁序说明）
	}
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
