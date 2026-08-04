package cli

import (
	"context"
	"time"

	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/storage"
)

// sqliteBrowserStore 把 browser.BrowserSessionStore 端口桥到 storage.SQLiteRepository。
// browser 包不依赖 storage 包；桥接在 cli 层，方向正确（避免层反向依赖）。
type sqliteBrowserStore struct {
	repo *storage.SQLiteRepository
	ctx  context.Context
}

// 编译期断言：sqliteBrowserStore 满足 browser.BrowserSessionStore 端口。
var _ browser.BrowserSessionStore = (*sqliteBrowserStore)(nil)

// newSQLiteBrowserStore 构造一个把浏览器会话持久化落到 agent.db 的适配器。
func newSQLiteBrowserStore(repo *storage.SQLiteRepository) *sqliteBrowserStore {
	return &sqliteBrowserStore{repo: repo, ctx: context.Background()}
}

// Save upsert 一条完整会话快照（含 storage_state）到 browser_sessions。
func (s *sqliteBrowserStore) Save(rec browser.SessionRecord) error {
	return s.repo.SaveBrowserSession(s.ctx, storage.BrowserSessionRecord{
		ID: rec.ID, TaskID: rec.TaskID, ActiveURL: rec.ActiveURL, StorageState: rec.StorageState,
		CreatedAt: rec.CreatedAt, LastUsedAt: rec.LastUsedAt, Evicted: rec.Evicted,
	})
}

// Touch 字段级写穿：只更新 active_url/last_used_at，绝不触碰 storage_state。
func (s *sqliteBrowserStore) Touch(id, activeURL string, lastUsed time.Time) error {
	return s.repo.TouchBrowserSession(s.ctx, id, activeURL, lastUsed)
}

// Get 按 id 读取一条会话快照；第二个返回值 false 表示不存在（非错误）。
func (s *sqliteBrowserStore) Get(id string) (browser.SessionRecord, bool, error) {
	rec, ok, err := s.repo.GetBrowserSession(s.ctx, id)
	if err != nil || !ok {
		return browser.SessionRecord{}, ok, err
	}
	return toBrowserRecord(rec), true, nil
}

// List 返回所有持久化会话快照（供 Runtime 启动时加载重建）。
func (s *sqliteBrowserStore) List() ([]browser.SessionRecord, error) {
	recs, err := s.repo.ListBrowserSessions(s.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]browser.SessionRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, toBrowserRecord(r))
	}
	return out, nil
}

// Delete 删除一条持久化会话快照。
func (s *sqliteBrowserStore) Delete(id string) error { return s.repo.DeleteBrowserSession(s.ctx, id) }

// toBrowserRecord 把 storage 层记录映射为 browser 端口记录（跨包桥接）。
func toBrowserRecord(r storage.BrowserSessionRecord) browser.SessionRecord {
	return browser.SessionRecord{
		ID: r.ID, TaskID: r.TaskID, ActiveURL: r.ActiveURL, StorageState: r.StorageState,
		CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Evicted: r.Evicted,
	}
}
