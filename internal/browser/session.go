package browser

import (
	"fmt"
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

// SessionStore 是 Session 的内存 CRUD（Phase 3 再加落盘）。
type SessionStore struct {
	mu   sync.Mutex
	seq  int
	byID map[string]*Session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{byID: make(map[string]*Session)}
}

func (st *SessionStore) Create(taskID string) *Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.seq++
	sess := &Session{
		ID:         fmt.Sprintf("sess-%d", st.seq),
		TaskID:     taskID,
		Refs:       make(map[string]string),
		CreatedAt:  time.Now(),
		LastUsedAt: time.Now(),
	}
	st.byID[sess.ID] = sess
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
	defer st.mu.Unlock()
	delete(st.byID, id)
}

// pageHandle 封装当前活跃页。page 存放 *rod.Page（runtime.go 以类型断言取回），
// 保持 interface{} 是为了让 session.go 不直接依赖 go-rod，隔离平台无关层。
type pageHandle struct {
	page interface{} // 实际存 *rod.Page
}
