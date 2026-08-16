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

// ReleaseContext 释放整个 incognito BrowserContext（连同其内的所有 page）。
//
// 只调 c.browser.Close()：go-rod 对 incognito 浏览器（BrowserContextID 非空）的
// Close() 发 Target.disposeBrowserContext，销毁【该 context】及其内的全部 page，
// 精确按 context 作用域，无泄漏。
//
// 绝不能再手动 c.browser.Pages() 逐页 Close：go-rod v0.116.2 的 Browser.Pages()
// 用【无 filter】的 Target.getTargets，返回的是所有 incognito context 的 page；逐个
// Close 会把其它活跃会话的 page 一并关掉，令那些会话的 page context 被取消——
// 接管注入 / 读取随后即报 "context canceled"（本会话未被回收、ActivePage 仍指向死
// page）。此前的实现正是如此，导致「多会话并发时回收一个会话会点不动另一个会话的页面」。
func (m *Manager) ReleaseContext(c *BrowserContext) error {
	if c == nil || c.browser == nil {
		return nil
	}
	if err := c.browser.Close(); err != nil {
		return fmt.Errorf("release context %s: close context: %w", c.id, err)
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
