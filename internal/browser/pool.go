package browser

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// poolInstance 是池里的一个浏览器进程。
//
// 抽成接口只为一件事：池的逻辑（什么时候复用、什么时候再开一个、什么时候收掉）
// 必须能在**没有 Chromium** 的情况下测——那些分支里最要紧的一条是「正在被用的进程
// 不许回收」，而它在真机上几乎不可能稳定复现。
type poolInstance interface {
	AcquireContext() (*BrowserContext, error)
	ReleaseContext(*BrowserContext) error
	// Contexts 是这个进程上还开着几个 context：它同时是「还能不能再塞」与
	// 「能不能回收它」的依据。
	Contexts() int
	// MemoryBytes 是这个进程当前占的内存；0 表示读不到（此时按「不回收」处理，
	// 一个读不出的仪表不该变成一次回收）。
	MemoryBytes() uint64
	Close()
	String() string
}

// poolConfig 是池的三个上限。
type poolConfig struct {
	// MaxProcesses 是同时存在的浏览器进程数上限。1 就是这个池出现之前的形状。
	MaxProcesses int
	// MaxContextsPerProcess 是一个进程上并存的 context 数上限。
	//
	// 它是把鸡蛋分篮子的那个数：同一进程里的会话共命运——一个页面把渲染进程搞崩、
	// 或者把内存吃光，同进程的别的会话一起完蛋。
	MaxContextsPerProcess int
	// ProcessMemoryLimitBytes 是单个进程的内存上限，超过它的**空闲**进程会被换掉。
	// 0 = 这个部署不做这件事（不是「用一个我们猜的上限」）。
	ProcessMemoryLimitBytes uint64
	Logger                  *slog.Logger
}

// browserPool 管一组浏览器进程。
//
// 它只做三件事，并且每一件都有一条不能越过的线：
//
//   - 复用：优先把新 context 放进已经开着的进程（冷启动 300~800ms，而且每个进程
//     几百 MB）。
//   - 扩容：现有进程都满了才开新的，直到 MaxProcesses。
//   - 回收：**只回收没有 context 的进程**。杀掉一个正在被人看着的浏览器，用户看到
//     的是页面凭空消失、接管中断、登录态没了——比多占一点内存糟得多。
type browserPool struct {
	mu        sync.Mutex
	cfg       poolConfig
	instances []poolInstance
	// owners 记住每个 context 属于哪个进程：释放时要还给它，而不是随便找一个。
	owners map[*BrowserContext]poolInstance
	// newInstance 是造一个新进程的工厂。测试替换它以避开 Chromium。
	newInstance func() (poolInstance, error)
	logger      *slog.Logger
}

// newBrowserPool 建一个空池（第一个进程在第一次 Acquire 时才起）。
func newBrowserPool(cfg poolConfig) *browserPool {
	if cfg.MaxProcesses <= 0 {
		cfg.MaxProcesses = 1
	}
	if cfg.MaxContextsPerProcess <= 0 {
		cfg.MaxContextsPerProcess = defaultMaxContextsPerProcess
	}
	logger := cfg.Logger
	if logger == nil {
		logger = discardLogger()
	}
	return &browserPool{cfg: cfg, owners: map[*BrowserContext]poolInstance{}, logger: logger}
}

// discardLogger 是「没配 logger」时用的那个：丢弃一切。
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// defaultMaxContextsPerProcess 是一个进程上默认放几个会话。
//
// 8 是按「一个会话几十 MB、一个进程几百 MB」估的：再多，一次崩溃波及的会话就太多；
// 再少，进程数（与冷启动）涨得太快。
const defaultMaxContextsPerProcess = 8

// Acquire 拿一个 context：先找有空位的进程，都满了再开一个新的。
func (p *browserPool) Acquire() (*BrowserContext, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, inst := range p.instances {
		if inst.Contexts() < p.cfg.MaxContextsPerProcess {
			return p.acquireOn(inst)
		}
	}
	if len(p.instances) >= p.cfg.MaxProcesses {
		return nil, NewBrowserError(CodeResourceExhausted, fmt.Sprintf(
			"all %d browser processes are full (%d contexts each); close a browser session and try again",
			p.cfg.MaxProcesses, p.cfg.MaxContextsPerProcess))
	}
	if p.newInstance == nil {
		return nil, fmt.Errorf("browser pool has no way to start a process")
	}
	inst, err := p.newInstance()
	if err != nil {
		return nil, err
	}
	p.instances = append(p.instances, inst)
	p.logger.Info("browser process started",
		"component", "browser", "instance", inst.String(), "processes", len(p.instances))
	return p.acquireOn(inst)
}

// acquireOn 在指定进程上开 context 并记下归属。调用方持锁。
func (p *browserPool) acquireOn(inst poolInstance) (*BrowserContext, error) {
	ctx, err := inst.AcquireContext()
	if err != nil {
		return nil, err
	}
	p.owners[ctx] = inst
	return ctx, nil
}

// Release 把 context 还给**开出它的那个**进程。
//
// 归属记在 owners 里而不是靠遍历猜：go-rod 的 incognito context 属于某一条 CDP
// 连接，还错进程就是对着别人的浏览器发 disposeBrowserContext。
func (p *browserPool) Release(ctx *BrowserContext) error {
	if ctx == nil {
		return nil
	}
	p.mu.Lock()
	inst, ok := p.owners[ctx]
	delete(p.owners, ctx)
	p.mu.Unlock()

	if !ok {
		// 契约允许：会话被回收两次（reaper 与显式 Close 撞上）是正常的，第二次
		// 无事可做。不是错误，也不该悄悄去别的进程上关一个同名 context。
		return nil
	}
	return inst.ReleaseContext(ctx)
}

// RecycleBloated 关掉那些**空闲且超出内存上限**的进程。
//
// 两个条件缺一不可，而且顺序是有意的：先问「有没有人在用」。回收一个正在被人看着
// 的浏览器，代价是页面凭空消失、接管中断、登录态丢失——那比多占几百 MB 糟得多。
func (p *browserPool) RecycleBloated() {
	if p.cfg.ProcessMemoryLimitBytes == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	kept := p.instances[:0]
	for _, inst := range p.instances {
		used := inst.MemoryBytes()
		switch {
		case inst.Contexts() > 0:
			// 有人在用：只记一笔，不动它。
			if used > p.cfg.ProcessMemoryLimitBytes {
				p.logger.Warn("browser process is over its memory limit but still in use",
					"component", "browser", "instance", inst.String(),
					"bytes", used, "limit", p.cfg.ProcessMemoryLimitBytes,
					"consequence", "it will be recycled once its last session closes")
			}
			kept = append(kept, inst)
		case used > p.cfg.ProcessMemoryLimitBytes:
			p.logger.Info("recycling an idle browser process over its memory limit",
				"component", "browser", "instance", inst.String(),
				"bytes", used, "limit", p.cfg.ProcessMemoryLimitBytes)
			inst.Close()
		default:
			kept = append(kept, inst)
		}
	}
	p.instances = kept
}

// Close 关掉池里的全部进程。
func (p *browserPool) Close() {
	p.mu.Lock()
	instances := p.instances
	p.instances = nil
	p.owners = map[*BrowserContext]poolInstance{}
	p.mu.Unlock()

	for _, inst := range instances {
		inst.Close()
	}
}

// adopt 把一个已经起好的进程放进池里。
//
// 它存在是因为第一个进程在装配期就起（见 NewManager 的理由），而那时池还不该
// 自己去起——否则「装配失败」与「第一次用时失败」的错误路径会各写一遍。
func (p *browserPool) adopt(inst poolInstance) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instances = append(p.instances, inst)
}

// firstInstance 返回池里的第一个进程；空池返回 nil。真机测试用它去看那个真的
// Chromium（收束、出口代理、进程本身），而不必把这些字段挂到 Manager 上。
func (p *browserPool) firstInstance() poolInstance {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.instances) == 0 {
		return nil
	}
	return p.instances[0]
}

// Size 是当前进程数（供诊断与测试）。
func (p *browserPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.instances)
}
