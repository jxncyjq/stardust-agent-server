package browser

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// 池此前只被**顺序**地测过：一条 goroutine 依次拿、依次还。而它在生产里的形状恰恰
// 相反——多个任务同时开浏览器会话、reaper 与用户的关闭动作同时落下、进程回收在任何
// 时刻插进来。上限只在无人竞争时成立，等于没有上限。
//
// 这一组是并发下的压测（配 -race 跑）。它们要钉的是三件在负载下才暴露的事：
//
//  1. 同时抢时上限仍然是上限（越界的代价是机器上多出一串几百 MB 的 Chromium）；
//  2. 取还交替之后账是平的（漏记一次 = 一个进程永远显得满，池提前拒绝服务）；
//  3. 关闭与获取撞在一起时不漏进程（漏掉的那个不属于任何池，没人再会去关它）。

// concurrentPool 与 newFakePool 同形，但成员自己加锁：Release 在池锁**之外**调用
// instance.ReleaseContext，因此成员的计数会被并发触碰。真的 chromiumInstance 正是
// 这么做的（c.mu 守 contexts），假成员必须同样成立，否则测出来的是假象。
type concurrentInstance struct {
	mu       sync.Mutex
	id       int
	contexts int
	peak     int
	closed   bool
	handed   []string
}

func (c *concurrentInstance) AcquireContext() (*BrowserContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contexts++
	if c.contexts > c.peak {
		c.peak = c.contexts
	}
	id := contextID(c.id, len(c.handed))
	c.handed = append(c.handed, id)
	return &BrowserContext{id: id}, nil
}

func (c *concurrentInstance) ReleaseContext(*BrowserContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contexts--
	return nil
}

func (c *concurrentInstance) Contexts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.contexts
}

func (c *concurrentInstance) MemoryBytes() uint64 { return 0 }

func (c *concurrentInstance) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *concurrentInstance) String() string { return "concurrent-" + itoa(c.id) }

func (c *concurrentInstance) snapshot() (contexts, peak int, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.contexts, c.peak, c.closed
}

// hasBrowserCode 说明这个错误链里带着某个语义码。
func hasBrowserCode(err error, code Code) bool {
	var be *BrowserError
	return errors.As(err, &be) && be.Code == code
}

func contextID(instance, seq int) string { return "ctx-" + itoa(instance) + "-" + itoa(seq) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// concurrentPool 建一个成员线程安全的池，并把造出来的成员都记下来（收尾时要逐个
// 检查它们是不是都被关了）。
func concurrentPool(t *testing.T, cfg poolConfig) (*browserPool, func() []*concurrentInstance) {
	t.Helper()

	var mu sync.Mutex
	var made []*concurrentInstance
	pool := newBrowserPool(cfg)
	pool.newInstance = func() (poolInstance, error) {
		mu.Lock()
		defer mu.Unlock()
		inst := &concurrentInstance{id: len(made) + 1}
		made = append(made, inst)
		return inst, nil
	}
	return pool, func() []*concurrentInstance {
		mu.Lock()
		defer mu.Unlock()
		return append([]*concurrentInstance(nil), made...)
	}
}

// TestTheCapsHoldWhenEveryoneAsksAtOnce：上限只在无人竞争时成立等于没有上限。越界
// 的代价是机器上多出一串几百 MB 的 Chromium 进程——池存在的全部理由就是不让这发生。
//
// 顺带钉住被拒的**方式**：满了要报 RESOURCE_EXHAUSTED，Agent 才能把「稍后再试」与
// 「页面坏了、换个做法」分开。
func TestTheCapsHoldWhenEveryoneAsksAtOnce(t *testing.T) {
	const (
		processes    = 4
		perProcess   = 8
		askers       = 200
		totalCapacit = processes * perProcess
	)
	pool, instances := concurrentPool(t, poolConfig{
		MaxProcesses:          processes,
		MaxContextsPerProcess: perProcess,
	})
	t.Cleanup(pool.Close)

	var (
		mu        sync.Mutex
		granted   []*BrowserContext
		refusals  int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for i := 0; i < askers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽量让它们真的挤在一起，而不是排着队进来
			ctx, err := pool.Acquire()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted = append(granted, ctx)
			case hasBrowserCode(err, CodeResourceExhausted):
				refusals++
			default:
				otherErrs = append(otherErrs, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(otherErrs) > 0 {
		t.Fatalf("a full pool answered with something other than RESOURCE_EXHAUSTED: %v", otherErrs[0])
	}
	if len(granted) != totalCapacit {
		t.Errorf("granted %d contexts, want exactly %d (%d processes x %d contexts)",
			len(granted), totalCapacit, processes, perProcess)
	}
	if refusals != askers-totalCapacit {
		t.Errorf("refused %d, want %d", refusals, askers-totalCapacit)
	}
	if size := pool.Size(); size > processes {
		t.Errorf("pool grew to %d processes, want at most %d: each one is a few hundred MB",
			size, processes)
	}
	for _, inst := range instances() {
		if _, peak, _ := inst.snapshot(); peak > perProcess {
			t.Errorf("%s held %d contexts at once, want at most %d", inst, peak, perProcess)
		}
	}
	// 同一个 context 不能发给两个人：那意味着两个会话开在同一张页上。
	seen := map[string]bool{}
	for _, ctx := range granted {
		if seen[ctx.id] {
			t.Fatalf("context %s was handed out twice", ctx.id)
		}
		seen[ctx.id] = true
	}
}

// TestChurnKeepsTheBooksBalanced：取还交替跑一阵之后，账必须是平的。漏记一次释放，
// 那个进程就永远显得满——池会在还有大量空位时开始拒绝服务，而且没有任何东西会把它
// 纠正回来。
func TestChurnKeepsTheBooksBalanced(t *testing.T) {
	pool, instances := concurrentPool(t, poolConfig{MaxProcesses: 3, MaxContextsPerProcess: 4})
	t.Cleanup(pool.Close)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ctx, err := pool.Acquire()
				if err != nil {
					// 满了是正常结果（12 个位置、24 条在抢），不是失败。
					continue
				}
				if err := pool.Release(ctx); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, inst := range instances() {
		if open, _, _ := inst.snapshot(); open != 0 {
			t.Errorf("%s still shows %d open contexts after everything was released; "+
				"it will refuse new sessions forever", inst, open)
		}
	}
	pool.mu.Lock()
	leftover := len(pool.owners)
	pool.mu.Unlock()
	if leftover != 0 {
		t.Errorf("the pool still remembers %d context owners after everything was released", leftover)
	}
}

// TestClosingThePoolDoesNotLeakAProcessStartedAtTheSameMoment：关闭与获取撞在一起。
//
// 这不是编出来的时序——serve 退出、GUI 关窗时，正有任务在开浏览器会话。若关闭之后
// 还能起进程，那个进程**不属于任何池**：没人再会去关它，它带着自己的出口代理和一串
// renderer 留在机器上，直到用户自己去任务管理器里清。整套 confinement 就是为了不让
// 孤儿进程存在，从这个口子漏出去的一样是孤儿。
func TestClosingThePoolDoesNotLeakAProcessStartedAtTheSameMoment(t *testing.T) {
	pool, instances := concurrentPool(t, poolConfig{MaxProcesses: 8, MaxContextsPerProcess: 1})

	// 第一波与 Close **同时**跑：这一段是给 -race 看的，交错由调度决定。
	var wg sync.WaitGroup
	start := make(chan struct{})
	acquireOnce := func() {
		defer wg.Done()
		ctx, err := pool.Acquire()
		if err != nil {
			return // 关闭之后被拒绝是正确答案
		}
		_ = pool.Release(ctx)
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { <-start; acquireOnce() }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; pool.Close() }()
	close(start)
	wg.Wait()

	// 第二波在 Close **确实返回之后**才开始。第一波的交错靠运气，这一波不靠：
	// 少了它，「Close 忘了留下关闭标记」这个变异会在半数运行里蒙混过去，而一条
	// 半数会绿的测试等于没有测试。
	before := len(instances())
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go acquireOnce()
	}
	wg.Wait()

	if after := len(instances()); after != before {
		t.Errorf("the pool started %d browser processes after it was closed; "+
			"they belong to no pool now, and nothing will ever close them", after-before)
	}
	for _, inst := range instances() {
		if _, _, closed := inst.snapshot(); !closed {
			t.Errorf("%s was started around the close and never shut down: "+
				"it belongs to no pool now, and nothing will ever close it", inst)
		}
	}
}

// TestAcquiringAfterCloseIsRefused：上面那条的另一面，也是修法必须落在的地方——
// 关闭之后再要 context，答案是报错，而不是**起一个新的 Chromium**。
func TestAcquiringAfterCloseIsRefused(t *testing.T) {
	pool, instances := concurrentPool(t, poolConfig{MaxProcesses: 2, MaxContextsPerProcess: 2})
	pool.Close()

	ctx, err := pool.Acquire()
	if err == nil {
		t.Fatalf("a closed pool handed out context %s", ctx.id)
	}
	if len(instances()) != 0 {
		t.Errorf("a closed pool started %d browser processes", len(instances()))
	}
}

// TestTheMemoryFloorHoldsForEveryoneAtOnce：内存门槛此前只被单线程问过一次。生产里
// 「机器要没内存了」恰恰是一群任务同时开会话的时刻——如果并发能绕过它，它就只在无人
// 竞争时有效，也就是在最不需要它的时候有效。
//
// 顺带说明这条策略**不是**什么：它是逐次的安全余量，不是配额。真正的硬上限是进程池
// （MaxProcesses × MaxContextsPerProcess）；门槛之上并发放行多少个，由池决定。
func TestTheMemoryFloorHoldsForEveryoneAtOnce(t *testing.T) {
	t.Parallel()

	var reads int64
	rt := &Runtime{
		sessions: NewSessionStore(),
		hubs:     newHubRegistry(),
		cfg:      RuntimeConfig{MinFreeMemoryBytes: 8 << 30},
	}
	rt.availableMemory = func() (uint64, error) {
		atomic.AddInt64(&reads, 1)
		return 1 << 30, nil // 1GiB 可用，远低于 8GiB 门槛
	}

	const askers = 64
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		admitted int64
	)
	for i := 0; i < askers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := rt.admitNewSession(); err == nil {
				atomic.AddInt64(&admitted, 1)
			} else if !hasBrowserCode(err, CodeResourceExhausted) {
				t.Errorf("admitNewSession = %v, want RESOURCE_EXHAUSTED", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&admitted); got != 0 {
		t.Errorf("%d of %d concurrent opens got past a floor that no single one could pass", got, askers)
	}
	if got := atomic.LoadInt64(&reads); got != askers {
		t.Errorf("the gauge was read %d times for %d asks: the floor is checked per open, "+
			"and a cached reading would let a burst through on stale data", got, askers)
	}
}
