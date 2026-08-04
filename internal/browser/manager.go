package browser

import (
	"fmt"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// ContextOpts 见 spec §3.4（本 Phase 只用零值；Proxy/UA/Stealth 后续 Phase）。
type ContextOpts struct {
	Proxy     string
	UserAgent string
	Stealth   bool
}

// BrowserContext 对应 go-rod 的 incognito browser（隔离上下文）。
type BrowserContext struct {
	id      string
	browser *rod.Browser // incognito browser
}

// ManagerConfig 配置进程。本 Phase 单进程。
type ManagerConfig struct {
	Headless bool
	BinPath  string // 空则由 go-rod launcher 定位/下载
}

// Manager 是单进程 + 多 incognito Context 的两级池的最小实现（spec §3.4）。
type Manager struct {
	mu       sync.Mutex
	launcher *launcher.Launcher
	browser  *rod.Browser // 一条 CDP 连接 = 一个 Chromium 进程
	seq      int
}

// NewManager 拉起一个 Chromium 进程并连接。
func NewManager(cfg ManagerConfig) (*Manager, error) {
	l := launcher.New().Headless(cfg.Headless)
	if cfg.BinPath != "" {
		l = l.Bin(cfg.BinPath)
	}
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	b := rod.New().ControlURL(controlURL)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("connect chromium: %w", err)
	}
	return &Manager{launcher: l, browser: b}, nil
}

// AcquireContext 开一个隔离 incognito Context。本 Phase 不复用、不排队。
func (m *Manager) AcquireContext(_ ContextOpts) (*BrowserContext, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	incog, err := m.browser.Incognito()
	if err != nil {
		return nil, fmt.Errorf("create incognito context: %w", err)
	}
	m.seq++
	return &BrowserContext{id: fmt.Sprintf("ctx-%d", m.seq), browser: incog}, nil
}

// ReleaseContext 关闭 Context 的所有 page 并释放。
func (m *Manager) ReleaseContext(c *BrowserContext) error {
	if c == nil || c.browser == nil {
		return nil
	}
	pages, err := c.browser.Pages()
	if err == nil {
		for _, p := range pages {
			_ = p.Close()
		}
	}
	return nil
}

// Close 关闭浏览器进程。
func (m *Manager) Close() {
	if m.browser != nil {
		_ = m.browser.Close()
	}
	if m.launcher != nil {
		m.launcher.Cleanup()
	}
}
