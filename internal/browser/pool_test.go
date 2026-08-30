package browser

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 一个 Chromium 进程 + 多个 incognito context 是最省内存的形状，也是最脆的：一个
// 页面把渲染进程搞崩、或者把内存吃到几个 G，同一进程里的**别的会话**跟着一起完蛋。
// 而此前只有一个进程，连「换一个」这个动作都不存在。
//
// 进程池要回答三件事，这些测试逐条钉：什么时候复用、什么时候再开一个、什么时候把
// 一个进程收掉。收掉这条最要紧——回收一个**正在被人看着**的浏览器，比多占一点内存
// 糟得多。

// fakeInstance 是一个不需要 Chromium 的池成员。
type fakeInstance struct {
	id       int
	contexts int
	closed   bool
	memory   uint64
}

func (f *fakeInstance) Contexts() int { return f.contexts }

// newFakePool 建一个用假成员的池：工厂按顺序发号，采样读成员自己记的数。
func newFakePool(t *testing.T, cfg poolConfig) (*browserPool, *[]*fakeInstance) {
	t.Helper()

	made := &[]*fakeInstance{}
	next := 0
	pool := newBrowserPool(cfg)
	pool.newInstance = func() (poolInstance, error) {
		next++
		inst := &fakeInstance{id: next}
		*made = append(*made, inst)
		return &fakeInstanceAdapter{inst}, nil
	}
	t.Cleanup(pool.Close)
	return pool, made
}

// fakeInstanceAdapter 把 fakeInstance 接到池要的接口上。
type fakeInstanceAdapter struct{ inst *fakeInstance }

func (a *fakeInstanceAdapter) AcquireContext() (*BrowserContext, error) {
	a.inst.contexts++
	return &BrowserContext{id: fmt.Sprintf("fake-ctx-%d-%d", a.inst.id, a.inst.contexts)}, nil
}

func (a *fakeInstanceAdapter) ReleaseContext(*BrowserContext) error {
	a.inst.contexts--
	return nil
}

func (a *fakeInstanceAdapter) Contexts() int                  { return a.inst.contexts }
func (a *fakeInstanceAdapter) MemoryBytes() uint64            { return a.inst.memory }
func (a *fakeInstanceAdapter) Close()                         { a.inst.closed = true }
func (a *fakeInstanceAdapter) String() string                 { return fmt.Sprintf("fake-%d", a.inst.id) }
func (a *fakeInstanceAdapter) instanceForTest() *fakeInstance { return a.inst }

func TestThePoolReusesAProcessUntilItIsFull(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{MaxProcesses: 4, MaxContextsPerProcess: 3})

	for i := 0; i < 3; i++ {
		if _, err := pool.Acquire(); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if len(*made) != 1 {
		t.Errorf("started %d processes for 3 contexts with a cap of 3, want 1", len(*made))
	}
}

func TestThePoolStartsAnotherProcessWhenTheFirstIsFull(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{MaxProcesses: 4, MaxContextsPerProcess: 2})

	for i := 0; i < 3; i++ {
		if _, err := pool.Acquire(); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if len(*made) != 2 {
		t.Errorf("started %d processes for 3 contexts with a cap of 2, want 2", len(*made))
	}
}

// TestThePoolRefusesRatherThanOverfilling: 越过上限继续塞，换来的是一台开始换页的
// 机器，而机器上跑的**别的**东西一起被拖垮。拒绝要带得起自恢复的语义。
func TestThePoolRefusesRatherThanOverfilling(t *testing.T) {
	t.Parallel()

	pool, _ := newFakePool(t, poolConfig{MaxProcesses: 1, MaxContextsPerProcess: 1})

	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_, err := pool.Acquire()
	if err == nil {
		t.Fatal("the pool kept handing out contexts past both caps")
	}
	var be *BrowserError
	if !errors.As(err, &be) || be.Code != CodeResourceExhausted {
		t.Errorf("error = %v, want %s so the agent knows to retry later rather than re-read a page",
			err, CodeResourceExhausted)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("error = %v, want it to name the limit that was hit", err)
	}
}

// TestReleasingMakesRoomAgain: 上限是并发上限，不是总量上限。
func TestReleasingMakesRoomAgain(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{MaxProcesses: 1, MaxContextsPerProcess: 1})

	ctx, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := pool.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := pool.Acquire(); err != nil {
		t.Errorf("Acquire after Release: %v", err)
	}
	if len(*made) != 1 {
		t.Errorf("started %d processes, want the released one to be reused", len(*made))
	}
}

// TestABloatedProcessIsRecycledOnlyWhenNobodyIsUsingIt 是这一层里最要紧的一条。
//
// 一个吃到几个 G 的浏览器该被换掉；但把一个**正在被人看着**的浏览器杀掉，用户看到
// 的是页面凭空消失、接管中断、登录态没了——比多占一点内存糟得多。
func TestABloatedProcessIsRecycledOnlyWhenNobodyIsUsingIt(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{
		MaxProcesses: 2, MaxContextsPerProcess: 4, ProcessMemoryLimitBytes: 1 << 30,
	})

	busy, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	(*made)[0].memory = 4 << 30 // 远超上限，但它还开着一个 context

	pool.RecycleBloated()
	if (*made)[0].closed {
		t.Fatal("a process with a live context was recycled; someone's page just vanished")
	}

	if err := pool.Release(busy); err != nil {
		t.Fatalf("Release: %v", err)
	}
	pool.RecycleBloated()
	if !(*made)[0].closed {
		t.Error("an idle process well over the memory limit was kept; the limit does nothing")
	}
}

func TestRecyclingLeavesProcessesUnderTheLimitAlone(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{
		MaxProcesses: 2, MaxContextsPerProcess: 4, ProcessMemoryLimitBytes: 1 << 30,
	})
	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	(*made)[0].memory = 100 << 20

	pool.RecycleBloated()

	if (*made)[0].closed {
		t.Error("a process under the limit was recycled; restarting a healthy browser costs a cold start " +
			"and every logged-in session in it")
	}
}

// TestNoMemoryLimitMeansNoRecycling：没配上限就是「这个部署不做这件事」，不是
// 「用一个我们猜的上限」。
func TestNoMemoryLimitMeansNoRecycling(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{MaxProcesses: 2, MaxContextsPerProcess: 4})
	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	(*made)[0].memory = 64 << 30

	pool.RecycleBloated()

	if (*made)[0].closed {
		t.Error("a process was recycled with no memory limit configured")
	}
}

func TestClosingThePoolClosesEveryProcess(t *testing.T) {
	t.Parallel()

	pool, made := newFakePool(t, poolConfig{MaxProcesses: 3, MaxContextsPerProcess: 1})
	for i := 0; i < 3; i++ {
		if _, err := pool.Acquire(); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}

	pool.Close()

	for _, inst := range *made {
		if !inst.closed {
			t.Errorf("process %d was left running after the pool closed", inst.id)
		}
	}
}
