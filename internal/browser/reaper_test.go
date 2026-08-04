package browser

import (
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
