package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	// MaxProcesses / MaxContextsPerProcess / ProcessMemoryLimitBytes 是进程池的
	// 三个上限，见 poolConfig。默认（0）等价于池出现之前的形状：一个进程。
	MaxProcesses            int
	MaxContextsPerProcess   int
	ProcessMemoryLimitBytes uint64
}

// Manager 是浏览器的两级池：进程池（browserPool）+ 每个进程里的 incognito
// context。出口代理是**整池共用**的一个——它按请求判「连去哪」，与是哪个进程无关。
type Manager struct {
	pool   *browserPool
	egress *egressProxy
	pal    PlatformAdapter
	logger *slog.Logger
}

// chromiumInstance 是池里的一个真 Chromium 进程。
type chromiumInstance struct {
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
	// launched 是我们自己起的那个进程。留着它是为了能关掉它：launcher 现在只负责
	// 拼参数，进程的生死由这里管。
	launched *launchedBrowser
	// contexts 是这个进程上还开着几个 incognito context：池据它决定还能不能再塞、
	// 以及能不能回收它。
	contexts int
	// userDataDir 是 launcher 替我们挑的临时用户目录。自管启动之后它的清理也归我们：
	// launcher.Cleanup() 会等一个**只有 Launch() 才会关**的 channel，不再调用它。
	userDataDir string
}

// chromiumStartTimeout 是等 Chromium 宣告 DevTools 地址的上限。冷启动（首次解压、
// 杀毒软件扫描）可以慢到十几秒，而超过这个数基本意味着它根本起不来——那时带着
// 浏览器自己写的 stderr 报错，比继续等有用。
const chromiumStartTimeout = 45 * time.Second

// chromiumDistFor 把配置与系统探测结果拼成分发优先级的输入。
//
// 抽出来是因为「配置里的字段有没有真的走到解析器」这件事，在装配代码里测不到——
// 少接一个字段不会让任何东西报错，浏览器照常起来，只是用的另一个 Chromium。
// BundledChromiumPath 此前正是这么在两个结构体之间掉进缝里的。
func chromiumDistFor(cfg RuntimeConfig, systemPath string) ChromiumDist {
	return ChromiumDist{
		ConfigBinPath: cfg.BinPath,
		BundledPath:   cfg.BundledChromiumPath,
		SystemPath:    systemPath,
	}
}

// NewManager 建起浏览器的两级池，并**立即起一个进程**。
//
// 立即起而不是等第一次用：装配期的失败（沙箱要求得不到满足、Chromium 找不到、
// 起来了不宣告地址）要在 serve 启动时就说出来，而不是等到第一个用户点了「浏览」
// 才炸——那时它看起来像那次操作的问题。
func NewManager(cfg ManagerConfig) (*Manager, error) {
	pal := NewPlatformAdapter()
	logger := cfg.Logger
	if logger == nil {
		logger = discardLogger()
	}
	// 出口代理整池共用：它按请求判「连去哪」（见 egressproxy.go），与是哪个进程
	// 发出的无关。每进程一个只会多占端口，并让日志更难读。
	egress, err := startEgressProxy(egressProxyConfig{
		AllowPrivateHosts: cfg.AllowPrivateHosts,
		Logger:            logger,
	})
	if err != nil {
		return nil, err
	}

	pool := newBrowserPool(poolConfig{
		MaxProcesses:            cfg.MaxProcesses,
		MaxContextsPerProcess:   cfg.MaxContextsPerProcess,
		ProcessMemoryLimitBytes: cfg.ProcessMemoryLimitBytes,
		Logger:                  logger,
	})
	pool.newInstance = func() (poolInstance, error) {
		return startChromiumInstance(cfg, pal, egress.URL(), logger)
	}

	manager := &Manager{pool: pool, egress: egress, pal: pal, logger: logger}
	// 先起一个：见上面「立即起」的理由。
	first, err := pool.newInstance()
	if err != nil {
		_ = egress.Close()
		return nil, err
	}
	if err := pool.adopt(first); err != nil {
		return nil, err
	}
	return manager, nil
}

// startChromiumInstance 起一个 Chromium 进程并连上它。Chromium 可执行文件经 PAL 按
// 分发优先级定位（config BinPath > 内置捆绑 > 系统 Chrome/Edge > go-rod 自动下载），
// 除 PAL 外不出现任何 runtime.GOOS 分支（spec §11.2）。
func startChromiumInstance(cfg ManagerConfig, pal PlatformAdapter, egressURL string,
	logger *slog.Logger) (*chromiumInstance, error) {
	binPath := resolveChromiumBin(chromiumDistFor(RuntimeConfig{
		BinPath:             cfg.BinPath,
		BundledChromiumPath: cfg.BundledChromiumPath,
	}, pal.ResolveChromiumPath()))

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

	l = l.Set(flags.Flag("proxy-server"), egressURL)
	// 默认情况下 Chromium 会绕过代理直连 localhost/127.0.0.1——那恰好是 SSRF 最想
	// 去的地方。"<-loopback>" 是 Chromium 的显式写法：把回环也交给代理，由代理按
	// 策略决定放不放行。
	//
	// 这不是预防性的：删掉这一行，TestLoopbackIsNotExemptFromTheEgressPolicy 会红
	// ——那台回环上的服务器真的会被直连到。
	l = l.Set(flags.Flag("proxy-bypass-list"), "<-loopback>")

	// 自己起进程，而不是 l.Launch()：go-rod 的 launcher 在内部 exec，我们只拿得到
	// 一个 pid，于是「进程创建时」这个时刻——外层沙箱唯一能建立的时刻、进程池唯一
	// 能决定起几个的时刻——根本不在我们手上。launcher 仍然用来拼参数（那套 flag
	// 处理没有必要重写）。
	//
	// 端口给 0 让系统分配：固定端口在同机跑两个 agent 时会撞，而撞上的表现是
	// 「连到了别人的浏览器」，比连不上更难查。
	l = l.Set(flags.RemoteDebuggingPort, "0")
	userDataDir := l.Get(flags.UserDataDir)
	// 把 Crashpad 的落点显式钉在 profile 里。
	//
	// 不设它时，Chromium 从 $HOME 推一个崩溃数据库路径；在只读根的沙箱里那条路推
	// 不出可写目录，chrome_crashpad_handler 拿到空的 --database 直接 CHECK 失败
	// （"--database is required"），浏览器**启动即崩**。profile 是沙箱里唯一可写的
	// 地方，也是这些文件本来就该待的地方。
	if userDataDir != "" {
		l = l.Set(flags.Flag("crash-dumps-dir"), filepath.Join(userDataDir, "crashpad"))
	}
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), chromiumStartTimeout)
	defer cancelLaunch()
	launched, err := launchChromium(launchCtx, launchSpec{
		Bin:  binPath,
		Args: l.FormatArgs(),
		PAL:  pal,
	})
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	b := rod.New().ControlURL(launched.controlURL)
	if err := b.Connect(); err != nil {
		_ = pal.KillProcess(launched.PID(), false)
		_ = launched.Wait()
		return nil, fmt.Errorf("connect chromium: %w", err)
	}
	// 收束**已经起来的**进程。此前的 WrapWithSandbox 接一个 *exec.Cmd，而 Chromium
	// 的进程是 go-rod 的 launcher 自己起的——那个 Cmd 从来不存在，于是三个平台把它
	// 实现完，浏览器照样一点约束都没有。按 pid 才有调用方。
	confinement, confineErr := pal.ConfineProcess(launched.PID())
	switch {
	case confineErr == nil:
		logger.Info("browser process confined", "component", "browser", "pid", launched.PID())
	case errors.Is(confineErr, ErrConfinementUnsupported):
		if cfg.RequireSandbox {
			_ = b.Close()
			_ = pal.KillProcess(launched.PID(), false)
			return nil, fmt.Errorf("browser sandbox required but unavailable on this platform: %w", confineErr)
		}
		logger.Warn("browser process is not confined",
			"component", "browser",
			"pid", launched.PID(),
			"reason", confineErr.Error(),
			"consequence", "the browser has no outer isolation, and a crash of this agent can leave "+
				"Chromium processes behind")
	default:
		// 平台有实现却失败了是真正的异常：宁可不带一个自以为存在的隔离继续跑。
		_ = b.Close()
		_ = pal.KillProcess(launched.PID(), false)
		return nil, fmt.Errorf("confine the browser process: %w", confineErr)
	}
	return &chromiumInstance{
		launcher:    l,
		launched:    launched,
		userDataDir: userDataDir,
		browser:     b,
		pal:         pal,
		confinement: confinement,
		logger:      logger,
	}, nil
}

// AcquireContext 从池里要一个隔离 incognito Context：先填已开着的进程，满了才起
// 新进程，都满了就带 RESOURCE_EXHAUSTED 拒绝（见 browserPool.Acquire）。
func (m *Manager) AcquireContext(_ ContextOpts) (*BrowserContext, error) {
	return m.pool.Acquire()
}

// RecycleBloated 关掉那些空闲且超出内存上限的浏览器进程（健康检查的动作）。
func (m *Manager) RecycleBloated() { m.pool.RecycleBloated() }

// Processes 是当前浏览器进程数（诊断用）。
func (m *Manager) Processes() int { return m.pool.Size() }

// AcquireContext 在这个进程上开一个 incognito Context。
func (c *chromiumInstance) AcquireContext() (*BrowserContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	incog, err := c.browser.Incognito()
	if err != nil {
		return nil, fmt.Errorf("create incognito context: %w", err)
	}
	c.seq++
	c.contexts++
	return &BrowserContext{id: fmt.Sprintf("%s-ctx-%d", c.String(), c.seq), browser: incog}, nil
}

// Contexts 是这个进程上还开着几个 context。
func (c *chromiumInstance) Contexts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.contexts
}

// MemoryBytes 是这个进程现在占多少内存；读不到时返回 0，池据此按「不回收」处理
// ——一个读不出的仪表不该变成一次回收。
func (c *chromiumInstance) MemoryBytes() uint64 {
	used, err := c.pal.SampleProcessMemory(c.launched.PID())
	if err != nil {
		c.logger.Warn("cannot sample browser memory", "component", "browser",
			"pid", c.launched.PID(), "error", err,
			"consequence", "this process is not considered for recycling")
		return 0
	}
	return used
}

// String 认得出是哪个进程（日志里按 pid 认人）。
func (c *chromiumInstance) String() string {
	return fmt.Sprintf("chromium-%d", c.launched.PID())
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
func (m *Manager) ReleaseContext(c *BrowserContext) error { return m.pool.Release(c) }

// ReleaseContext 释放这个进程上的一个 context（上面那段注释讲的就是这一句为什么
// 只调 c.browser.Close()）。
func (c *chromiumInstance) ReleaseContext(ctx *BrowserContext) error {
	if ctx == nil || ctx.browser == nil {
		return nil
	}
	c.mu.Lock()
	if c.contexts > 0 {
		c.contexts--
	}
	c.mu.Unlock()
	if err := ctx.browser.Close(); err != nil {
		return fmt.Errorf("release context %s: close context: %w", ctx.id, err)
	}
	return nil
}

// Close 关掉整池与共用的出口代理。
func (m *Manager) Close() {
	m.pool.Close()
	// 代理最后关：浏览器进程还在往外发请求时抽掉出口，只会得到一串连不上的错误。
	if m.egress != nil {
		if err := m.egress.Close(); err != nil {
			m.logger.Warn("close browser egress proxy", "component", "browser", "error", err)
		}
	}
}

// Close 关掉这一个 Chromium 进程。
func (c *chromiumInstance) Close() {
	if c.browser != nil {
		_ = c.browser.Close()
	}
	// 进程是我们起的，就由我们送走：browser.Close() 关的是 CDP 连接，Chromium 未必
	// 因此退出（尤其是已经卡住的那种）。Kill 之后 Wait，避免留下僵尸。
	//
	// 走 PAL 而不是 cmd.Process.Kill()：Chromium 是多进程的，杀主进程带不走
	// renderer/GPU。PAL 在 unix 上按**进程组**杀（组由 PrepareCommand 的 Setpgid
	// 建立），在 Windows 上由 Job Object 兜住整棵树。
	if c.launched != nil && c.launched.cmd.Process != nil {
		if err := c.pal.KillProcess(c.launched.PID(), false); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.logger.Warn("kill browser process", "component", "browser",
				"pid", c.launched.PID(), "error", err)
		}
		_ = c.launched.Wait()
	}
	// 绝不调用 m.launcher.Cleanup()：它等的是一个只有 launcher.Launch() 才会关闭的
	// channel（那个 goroutine 由 Launch 建立），而我们自己起进程之后它永远等不到，
	// Close 就永远返回不了。这不是理论——它挂住了整个 chromium 测试套件。
	//
	// 目录由我们清：SafeDelete 是给 Windows 的（文件常被刚退出的进程按着，要重试）。
	if c.userDataDir != "" {
		if err := c.pal.SafeDelete(c.userDataDir); err != nil {
			c.logger.Warn("remove browser user data dir", "component", "browser",
				"dir", c.userDataDir, "error", err)
		}
	}
	// 隔离最后关：关它会杀掉 job 里剩下的一切，那是「正常关不干净时」的兜底，
	// 而不是常规路径。出口代理不在这里——它整池共用，由 Manager.Close 关。
	if c.confinement != nil {
		if err := c.confinement.Close(); err != nil {
			c.logger.Warn("close browser confinement", "component", "browser", "error", err)
		}
	}
}
