package browser

import "time"

// SessionRecord 是持久化一个浏览器会话所需的字段（与 storage.BrowserSessionRecord 对应，
// 但 browser 包不依赖 storage 包——由 cli 侧适配器桥接，避免层反向依赖）。
type SessionRecord struct {
	ID           string
	TaskID       string
	ActiveURL    string
	StorageState string // cookies JSON
	CreatedAt    time.Time
	LastUsedAt   time.Time
	Evicted      bool
}

// BrowserSessionStore 是可选的持久化端口。nil 表示纯内存（Phase 1/2 行为不变）。
type BrowserSessionStore interface {
	Save(rec SessionRecord) error                         // 完整快照（含 storage_state）
	Touch(id, activeURL string, lastUsed time.Time) error // 字段级：只动 url/时间，不碰 storage_state
	Get(id string) (SessionRecord, bool, error)
	List() ([]SessionRecord, error)
	Delete(id string) error
}
