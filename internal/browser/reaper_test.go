package browser

import (
	"sync"
	"testing"
	"time"
)

// reapIdle 应回收 LastUsedAt+TTL < now 的、且有物理 Context 的会话；
// 无 Context（已 evicted）或未过期的跳过。此测试不建真 Context——用 nil Context 验证
// 「选择哪些会话」的判定，回收动作里对 nil Context 安全跳过物理释放。
func TestReapIdleSelectsExpired(t *testing.T) {
	rt := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry(), cfg: RuntimeConfig{SessionTTL: 10 * time.Minute}}
	now := time.Unix(1_000_000, 0)

	fresh := rt.sessions.Create("t")
	fresh.LastUsedAt = now.Add(-time.Minute) // 未过期
	old := rt.sessions.Create("t")
	old.LastUsedAt = now.Add(-time.Hour) // 过期

	reaped := rt.reapIdle(now)

	if len(reaped) != 1 || reaped[0] != old.ID {
		t.Fatalf("reaped = %v, want [%s]", reaped, old.ID)
	}
	// 过期会话应标记 Context=nil（evicted）
	got, _ := rt.sessions.Get(old.ID)
	if got.Context != nil {
		t.Fatalf("expired session Context should be nil after reap")
	}
}

// TestConcurrentActionAndReapNoRace 回归 CRITICAL 竞态：reaper 回收路径（evictSession/reapIdle）
// 与动作路径（touch/pageOf/activeURLOf）并发访问同一会话的 Context/ActivePage/LastUsedAt，
// 全部须在会话锁下进行。会话 Context==nil，evictSession 锁内早返回（nil 安全，无需真 go-rod），
// 但两侧对共享字段的读写仍经同一把 sess.mu 串行——在 -race 下应零竞争、无 panic。
//
// touch 写 LastUsedAt/读 ActiveURL、reapIdle 读 LastUsedAt，构成真实的写-读对：修复前
// reapIdle 在锁外读 LastUsedAt 会被 -race 判定竞争，修复后两侧同锁，干净。
func TestConcurrentActionAndReapNoRace(t *testing.T) {
	rt := &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry(), cfg: RuntimeConfig{SessionTTL: time.Minute}}
	sess := rt.sessions.Create("t") // Context==nil：evictSession 锁内早返回，不需 Chromium
	// 远未来的 now 保证 reapIdle 判定「已空闲够久」，从而每轮都进入 evictSession（锁下走一遍）。
	farFuture := time.Now().Add(24 * time.Hour)

	const workers = 8
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				rt.reapIdle(farFuture) // 锁下读 LastUsedAt + evictSession 锁下读/写 Context/ActivePage
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				rt.touch(sess)           // 锁下写 LastUsedAt / 读 ActiveURL
				_ = rt.pageOf(sess)      // 锁下读 ActivePage
				_ = rt.activeURLOf(sess) // 锁下读 ActiveURL
			}
		}()
	}
	wg.Wait()
	// 跑到这里未 panic、且 -race 未报告数据竞争，即证明锁纪律成立。
}
