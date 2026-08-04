package browser

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Session 见 spec §3.3。本 Phase 只含最小闭环字段；StorageState/ActionLog/TTL 后续 Phase。
type Session struct {
	ID         string
	TaskID     string
	Context    *BrowserContext   // 回收后为 nil
	ActivePage *pageHandle       // 当前活跃页（Task 6 定义 pageHandle）
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
