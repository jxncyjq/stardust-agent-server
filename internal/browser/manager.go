package browser

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	// AllowPrivateHosts 透传给出口代理：它决定「哪些地址可以连」，不决定是否走代理。
	// 浏览器的全部流量在任何情况下都经过代理，因为钉住拨号（解析一次、连那一次的
	// 结果）是防 DNS rebinding 的机制本身，与放不放行私网无关。
	AllowPrivateHosts bool

	// BundledChromiumPath 指向随 App 打包的内置固定版 Chromium（4C 打包时填）；
	// 默认空，此时分发优先级退到系统探测再退到 go-rod 自动下载。
	BundledChromiumPath string

	// RequireSandbox 让部署表态：这台机器上没有外层隔离时，宁可**不启动浏览器**。
	//
	// 默认 false，因为 Linux/macOS 目前没有实现（见各 platform 文件），打开它等于
	// 在那两个平台上关掉浏览器功能。设为 true 换来的是：要么被收束，要么明确失败，
	// 不存在「以为自己被收束」的第三种状态。
	RequireSandbox bool

	// Logger 记录出口代理的拒绝与收束的成功/缺席。nil 时丢弃。
	Logger *slog.Logger
}

// Manager 是单进程 + 多 incognito Context 的两级池的最小实现（spec §3.4）。
type Manager struct {
	mu       sync.Mutex
	launcher *launcher.Launcher
	browser  *rod.Browser // 一条 CDP 连接 = 一个 Chromium 进程
	pal      PlatformAdapter
	seq      int
	// egress 是这个 Chromium 的唯一出网口（见 egressproxy.go）。它随进程一起起、
	// 一起关：代理先死会让浏览器的每个请求都连不上，进程先死则代理无人可服务。
	egress *egressProxy
	// confinement 是这个 Chromium 进程的外层隔离（Windows: Job Object）。关闭它会
	// 带走 job 里的**全部**进程——Chromium 是多进程的，杀主进程带不走 renderer/GPU，
	// 此前 agent 崩一次就在机器上留下一串孤儿，直到用户自己去任务管理器里清。
	confinement io.Closer
	logger      *slog.Logger
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

	// 出口代理必须在启动参数定下来之前就位：它的端口是随机的，而 --proxy-server
	// 只在进程启动时读一次。
	egress, err := startEgressProxy(egressProxyConfig{
		AllowPrivateHosts: cfg.AllowPrivateHosts,
		Logger:            cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	l = l.Set(flags.Flag("proxy-server"), egress.URL())
	// 默认情况下 Chromium 会绕过代理直连 localhost/127.0.0.1——那恰好是 SSRF 最想
	// 去的地方。"<-loopback>" 是 Chromium 的显式写法：把回环也交给代理，由代理按
	// 策略决定放不放行。
	l = l.Set(flags.Flag("proxy-bypass-list"), "<-loopback>")

	controlURL, err := l.Launch()
	if err != nil {
		_ = egress.Close()
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	b := rod.New().ControlURL(controlURL)
	if err := b.Connect(); err != nil {
		_ = egress.Close()
		return nil, fmt.Errorf("connect chromium: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// 收束**已经起来的**进程。此前的 WrapWithSandbox 接一个 *exec.Cmd，而 Chromium
	// 的进程是 go-rod 的 launcher 自己起的——那个 Cmd 从来不存在，于是三个平台把它
	// 实现完，浏览器照样一点约束都没有。按 pid 才有调用方。
	confinement, confineErr := pal.ConfineProcess(l.PID())
	switch {
	case confineErr == nil:
		logger.Info("browser process confined", "component", "browser", "pid", l.PID())
	case errors.Is(confineErr, ErrConfinementUnsupported):
		if cfg.RequireSandbox {
			_ = b.Close()
			_ = egress.Close()
			return nil, fmt.Errorf("browser sandbox required but unavailable on this platform: %w", confineErr)
		}
		logger.Warn("browser process is not confined",
			"component", "browser",
			"pid", l.PID(),
			"reason", confineErr.Error(),
			"consequence", "the browser has no outer isolation, and a crash of this agent can leave "+
				"Chromium processes behind")
	default:
		// 平台有实现却失败了是真正的异常：宁可不带一个自以为存在的隔离继续跑。
		_ = b.Close()
		_ = egress.Close()
		return nil, fmt.Errorf("confine the browser process: %w", confineErr)
	}
	// TODO(phase6): Reap/健康检查阶段用 m.pal.KillProcess(pid, false) 终止僵死
	// Chromium 进程（配合上面的 Job Object / 信号）。
	return &Manager{
		launcher:    l,
		browser:     b,
		pal:         pal,
		egress:      egress,
		confinement: confinement,
		logger:      logger,
	}, nil
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
	// 隔离在浏览器之后、代理之前关：关它会杀掉 job 里剩下的一切，那是「正常关不
	// 干净时」的兜底，而不是常规路径。
	if m.confinement != nil {
		if err := m.confinement.Close(); err != nil {
			m.logger.Warn("close browser confinement", "component", "browser", "error", err)
		}
	}
	// 代理最后关：浏览器进程还在往外发请求时抽掉出口，只会得到一串连不上的错误。
	if m.egress != nil {
		if err := m.egress.Close(); err != nil {
			m.logger.Warn("close browser egress proxy", "component", "browser", "error", err)
		}
	}
}
