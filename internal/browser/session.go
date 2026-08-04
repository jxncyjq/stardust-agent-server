package browser

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session 见 spec §3.3。本 Phase 只含最小闭环字段；StorageState/ActionLog/TTL 后续 Phase。
type Session struct {
	ID         string
	TaskID     string
	Context    *BrowserContext   // 回收后为 nil
	ActivePage *pageHandle       // 当前活跃页（Task 6 定义 pageHandle）
	ActiveURL  string            // 当前地址（Open 成功导航后写入）；TTL 回收落盘 + 重启恢复据此重新导航
	Refs       map[string]string // ref → CDP backendNodeID/selector，会话内稳定
	CreatedAt  time.Time
	LastUsedAt time.Time

	mu sync.Mutex // 会话内串行锁（spec §3.3 关键决策）
}

// WithLock 在会话串行锁下执行 fn——同 Session 动作串行，跨 Session 并行。
func (s *Session) WithLock(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

// SessionStore 是 Session 的内存 CRUD，可选写穿到持久化端口（persist）。
type SessionStore struct {
	mu      sync.Mutex
	seq     int
	byID    map[string]*Session
	persist BrowserSessionStore // nil = 纯内存（Phase 1/2 行为不变）
}

func NewSessionStore() *SessionStore {
	return &SessionStore{byID: make(map[string]*Session)}
}

// SetPersist 装配可选持久化端口（nil = 纯内存，Phase 1/2 行为不变）。
func (st *SessionStore) SetPersist(p BrowserSessionStore) { st.persist = p }

func (st *SessionStore) Create(taskID string) *Session {
	st.mu.Lock()
	st.seq++
	sess := &Session{
		ID:         fmt.Sprintf("sess-%d", st.seq),
		TaskID:     taskID,
		Refs:       make(map[string]string),
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}
	st.byID[sess.ID] = sess
	persist := st.persist
	st.mu.Unlock()

	// 写穿：先内存后落盘，落盘失败记 Warn 不致命——会话已在内存可用，
	// 元数据丢了可从下次动作或重启列表恢复（storageState 的落盘顺序相反，见 reaper）。
	if persist != nil {
		if err := persist.Save(SessionRecord{
			ID:         sess.ID,
			TaskID:     sess.TaskID,
			CreatedAt:  sess.CreatedAt,
			LastUsedAt: sess.LastUsedAt,
		}); err != nil {
			slog.Warn("browser: persist session on create failed", "session", sess.ID, "err", err)
		}
	}
	return sess
}

// Adopt 把一条持久化记录纳入内存表，Context=nil（懒重建，首次访问时由 rebuildContext 补齐），
// 且不回写持久层——本方法是「从持久层加载」，回写会形成无谓的自我覆盖。
//
// 维护 seq 单调：若 rec.ID 形如 sess-<N>，把 st.seq 抬到至少 N。否则重启后 Adopt 了 sess-3，
// 之后一次 Create 会从 seq=0 铸出 sess-1，与已存会话撞号。
func (st *SessionStore) Adopt(rec SessionRecord) *Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess := &Session{
		ID:         rec.ID,
		TaskID:     rec.TaskID,
		ActiveURL:  rec.ActiveURL,
		Refs:       make(map[string]string),
		CreatedAt:  rec.CreatedAt,
		LastUsedAt: rec.LastUsedAt,
	}
	st.byID[rec.ID] = sess
	if n, ok := parseSessionSeq(rec.ID); ok && n > st.seq {
		st.seq = n
	}
	return sess
}

// parseSessionSeq 解析 "sess-<N>" 形式 id 的数字后缀，供 Adopt 维护 seq 单调。
// 不匹配前缀或后缀非数字时返回 (0,false)——非本类命名的 id 不参与 seq 计算。
func parseSessionSeq(id string) (int, bool) {
	const prefix = "sess-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(id[len(prefix):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Snapshot 返回当前所有会话指针的浅拷贝切片，供并发安全遍历（reaper 用）：
// 只在锁内复制指针，遍历/回收发生在锁外，避免回收时持锁调用外部（CDP/落盘）。
func (st *SessionStore) Snapshot() []*Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*Session, 0, len(st.byID))
	for _, s := range st.byID {
		out = append(out, s)
	}
	return out
}

func (st *SessionStore) Get(id string) (*Session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byID[id]
	return s, ok
}

func (st *SessionStore) Delete(id string) {
	st.mu.Lock()
	delete(st.byID, id)
	persist := st.persist
	st.mu.Unlock()

	// 写穿删除：best-effort，落盘失败记 Warn 不致命。
	if persist != nil {
		if err := persist.Delete(id); err != nil {
			slog.Warn("browser: persist session delete failed", "session", id, "err", err)
		}
	}
}

// pageHandle 封装当前活跃页。page 存放 *rod.Page（runtime.go 以类型断言取回），
// 保持 interface{} 是为了让 session.go 不直接依赖 go-rod，隔离平台无关层。
type pageHandle struct {
	page interface{} // 实际存 *rod.Page
}
