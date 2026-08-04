//go:build chromium

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// memStore 是 BrowserSessionStore 的进程内内存实现，仅供本包 chromium 测试使用
// （生产走 internal/cli 的 SQLite 适配器）。map+mutex，并发安全。
type memStore struct {
	mu   sync.Mutex
	recs map[string]SessionRecord
}

func newMemStore() *memStore { return &memStore{recs: map[string]SessionRecord{}} }

func (m *memStore) Save(rec SessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.ID] = rec
	return nil
}

// Touch 字段级更新：只动 active_url/last_used_at/evicted，绝不触碰 storage_state。
func (m *memStore) Touch(id, activeURL string, lastUsed time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[id]
	if !ok {
		return fmt.Errorf("touch browser session %q: no such session", id)
	}
	rec.ActiveURL = activeURL
	rec.LastUsedAt = lastUsed
	rec.Evicted = false
	m.recs[id] = rec
	return nil
}

func (m *memStore) Get(id string) (SessionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[id]
	return rec, ok, nil
}

func (m *memStore) List() ([]SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionRecord, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, id)
	return nil
}

// mustActivePageForTest 取会话当前活跃页的 *rod.Page，供测试直接读页面 body 断言。
func mustActivePageForTest(t *testing.T, rt *Runtime, id string) *rod.Page {
	t.Helper()
	sess, ok := rt.sessions.Get(id)
	if !ok {
		t.Fatalf("session %q not found", id)
	}
	page := rt.pageOf(sess)
	if page == nil {
		t.Fatalf("session %q has no active page", id)
	}
	return page
}

// cookieEchoServer：/set 下发 cookie sid=loginX；/ 回显收到的 sid cookie 值。
func cookieEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/set", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "loginX", Path: "/"})
		_, _ = w.Write([]byte(`<html><body>set</body></html>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		v := "none"
		if c, err := r.Cookie("sid"); err == nil {
			v = c.Value
		}
		_, _ = w.Write([]byte(`<html><body>sid=` + v + `</body></html>`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// TestRebuildAfterEvictionPreservesLogin 验证 Phase 3 核心闭环：会话被回收（Context=nil，
// 登录 cookies 先落盘）后再次访问，rebuildContext 恢复 cookies，导航到根路径仍回显 sid=loginX
// ——登录态跨回收+重建存活。
func TestRebuildAfterEvictionPreservesLogin(t *testing.T) {
	store := newMemStore()
	srv := cookieEchoServer(t)

	rt, err := NewRuntime(RuntimeConfig{
		Headless: true, AllowPrivateHosts: true, BinPath: systemChromeForTest(),
		SessionTTL: time.Hour, Store: store,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(context.Background(), CloseReq{})
	ctx := context.Background()

	open, err := rt.Open(ctx, OpenReq{URL: srv.URL + "/set", TaskID: "t"})
	if err != nil {
		t.Fatalf("Open /set: %v", err)
	}
	sid := open.SessionID

	// 强制回收（模拟 TTL 到期）：先落盘 cookies，再释放 Context 置 nil。
	sess, _ := rt.sessions.Get(sid)
	rt.evictSession(sess)
	if got, _ := rt.sessions.Get(sid); got.Context != nil {
		t.Fatal("expected evicted (Context nil) after evictSession")
	}

	// 再次访问该会话 → 应重建 Context、恢复 cookie，导航到根路径回显 loginX。
	if _, err := rt.Open(ctx, OpenReq{URL: srv.URL + "/", SessionID: sid}); err != nil {
		t.Fatalf("re-Open after evict: %v", err)
	}

	page := mustActivePageForTest(t, rt, sid)
	el, err := page.Element("body")
	if err != nil {
		t.Fatalf("find body: %v", err)
	}
	text, err := el.Text()
	if err != nil {
		t.Fatalf("read body text: %v", err)
	}
	if !strings.Contains(text, "sid=loginX") {
		t.Fatalf("login not preserved after rebuild; body=%q", text)
	}
}
