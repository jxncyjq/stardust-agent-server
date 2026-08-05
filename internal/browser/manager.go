package browser

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
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
	BinPath  string // 空则经 PAL 分发优先级定位（config > 内置 > 系统 > go-rod 下载）

	// BundledChromiumPath 指向随 App 打包的内置固定版 Chromium（4C 打包时填）；
	// 默认空，此时分发优先级退到系统探测再退到 go-rod 自动下载。
	BundledChromiumPath string
}

// Manager 是单进程 + 多 incognito Context 的两级池的最小实现（spec §3.4）。
type Manager struct {
	mu       sync.Mutex
	launcher *launcher.Launcher
	browser  *rod.Browser // 一条 CDP 连接 = 一个 Chromium 进程
	pal      PlatformAdapter
	seq      int
}

// NewManager 拉起一个 Chromium 进程并连接。Chromium 可执行文件经 PAL 按分发
// 优先级定位（config BinPath > 内置捆绑 > 系统 Chrome/Edge > go-rod 自动下载），
// 除 PAL 外不出现任何 runtime.GOOS 分支（spec §11.2）。
func NewManager(cfg ManagerConfig) (*Manager, error) {
	pal := NewPlatformAdapter()
	binPath := resolveChromiumBin(ChromiumDist{
		ConfigBinPath: cfg.BinPath,
		BundledPath:   cfg.BundledChromiumPath,
		SystemPath:    pal.ResolveChromiumPath(),
	})

	l := launcher.New()
	// 先注入平台相关启动参数，再由 Headless 收尾，使显式 Headless 开关对
	// DefaultLaunchArgs 里可能存在的 --headless 具有最终决定权（Set 覆盖既有值）。
	for _, arg := range pal.DefaultLaunchArgs() {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if hasValue {
			l = l.Set(flags.Flag(name), value)
		} else {
			l = l.Set(flags.Flag(name))
		}
	}
	l = l.HeadlessNew(cfg.Headless)
	if binPath != "" {
		l = l.Bin(binPath)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	b := rod.New().ControlURL(controlURL)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("connect chromium: %w", err)
	}
	// TODO(phase6): Reap/健康检查阶段用 m.pal.KillProcess(pid, false) 终止僵死
	// Chromium 进程（配合 Job Object / 信号），本 Phase 仅 launcher.Cleanup() 清临时目录。
	return &Manager{launcher: l, browser: b, pal: pal}, nil
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

// ReleaseContext 关闭 Context 的所有 page 并释放整个 incognito Context。
//
// 关键：仅关 page 不会释放 incognito BrowserContext —— go-rod v0.116.2 里
// incognito 浏览器的 Browser.Close() 才会 dispose 其 BrowserContext，遗漏它就会
// 每个 Session 泄漏一个上下文。故关完 page 后必须 c.browser.Close()。
// 不静默吞错（CLAUDE.md §0 fail-loud）：Pages() 报错也照常 Close，并把两处错误合并返回。
func (m *Manager) ReleaseContext(c *BrowserContext) error {
	if c == nil || c.browser == nil {
		return nil
	}
	pages, pagesErr := c.browser.Pages()
	if pagesErr == nil {
		for _, p := range pages {
			_ = p.Close() // best-effort；随后 Close 整个 Context 会一并回收
		}
	}
	closeErr := c.browser.Close()
	switch {
	case pagesErr != nil && closeErr != nil:
		return fmt.Errorf("release context %s: list pages: %v; close context: %w", c.id, pagesErr, closeErr)
	case pagesErr != nil:
		return fmt.Errorf("release context %s: list pages: %w", c.id, pagesErr)
	case closeErr != nil:
		return fmt.Errorf("release context %s: close context: %w", c.id, closeErr)
	default:
		return nil
	}
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
