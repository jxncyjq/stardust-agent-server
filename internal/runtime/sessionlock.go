package runtime

import (
	"context"
	"fmt"
	"sync"
)

// sessionRunLocks 串行化**同一会话上的任务执行**。
//
// 为什么必须有它（C-1，final-review.md §4 A-1）：会话事件日志的 seq 是一个
// 「读-改-写」不变量，而 P1 的 store（internal/storage.appendLocked）是**校验** seq
// 而不是分配 seq。eventRecorder 只在第一次 flush 时对齐游标、之后本地递增，于是同一
// 会话上两条并发任务会这样撞车：A 首刷占 0-2、B 首刷占 3-5、A 的第二次 flush 仍从 3
// 开始，而库里已经走到 6 —— Append 硬失败。这个失败发生在 fail-closed 的屏障里，
// 于是**整条任务失败返回**，最典型的落点是屏障 2（已经写过若干轮、若干工具副作用
// 已经发生之后）。它同时还让两条任务解出同一个 turn 号（违反 spec §4.1）、让两条
// 同时未应答的 tool/call 共用一个 call_id（违反 spec §4.3.1 第 4 条，直接砸 P3 的
// 按 call_id 配对）。默认配置即可达：coordinator 的 MaxWorkers 默认 4、任务锁按
// 任务 ID 而不是会话切分、`POST /v1/tasks` 的 session_id 由客户端给。
//
// 为什么是进程内的包级单例，而不是注入的字段：serve 是单进程，但同一个进程里构造
// **多个** Runtime 是常态（默认 runner 一个、per-agent resolver 每次解析一个、
// 委派再派生子 runtime）。这把锁只有覆盖整个进程才成立；做成 Config 字段的话，任何
// 一处装配漏传就等于这条会话的保护无声消失——那正是 Config.Gate 的文档注释里列出的
// 那种错误形状。包级单例漏不掉。
//
// 【权衡：worker 槽会被等锁的任务占住】RunTask 全程持有这把锁，而 coordinator 是
// 先占 sem（MaxWorkers 默认 4）再调 RunTask 的，所以一条在等锁的任务**占着一个
// worker 槽**。最坏情况：4 条任务同时压在一条会话上，4 个槽全被占住，其余会话的
// 任务要等到队头那条跑完才排得上。这不是死锁（队头任务永远在推进），但持续往同一
// 条会话灌任务确实会让别的会话被拖慢。接受它的理由：同一会话本来就应当串行（一条
// 会话就是一段对话，P2 之前的并发行为是「其中一条整任务失败」，不是「两条都跑
// 通」），而按会话分片 worker 池是调度语义、属于协调器的面，超出 P2 边界。等锁是
// 可取消的（见 acquire 的 select），所以调用方的 ctx 一旦取消就立刻释放槽位。
//
// 【已知上界】locks 表按引用计数回收：没有任何持有者/等待者的条目会被删掉，所以它
// 不会随会话数单调增长。
var sessionRunLocks = &sessionLockSet{}

// sessionLockSet 是一组按会话号切分的、可被 ctx 取消的互斥锁。
type sessionLockSet struct {
	mu    sync.Mutex
	locks map[string]*sessionLockEntry
}

// sessionLockEntry 用带缓冲的 channel 当互斥锁（而不是 sync.Mutex），因为等锁必须
// 能被 ctx 取消：sync.Mutex.Lock 一旦开始等就再也回不来，那会让一条已经被取消的任务
// 把 worker 槽一直占着。refs 是「持有者 + 等待者」的计数，归零时条目可回收。
type sessionLockEntry struct {
	ch   chan struct{}
	refs int
}

// acquire 取得该会话的执行锁，返回释放函数。
//
// ctx 在等待期间被取消时返回包装过的 ctx.Err()，不返回一个「没拿到锁但假装拿到了」
// 的释放函数——那正是 CLAUDE.md §0 说的兜底。
func (s *sessionLockSet) acquire(ctx context.Context, session string) (func(), error) {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sessionLockEntry)
	}
	entry, ok := s.locks[session]
	if !ok {
		entry = &sessionLockEntry{ch: make(chan struct{}, 1)}
		s.locks[session] = entry
	}
	entry.refs++
	s.mu.Unlock()

	select {
	case entry.ch <- struct{}{}:
	case <-ctx.Done():
		s.drop(session)
		return nil, fmt.Errorf("acquire session run lock for %q: %w", session, ctx.Err())
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-entry.ch
			s.drop(session)
		})
	}, nil
}

// drop 减一次引用，归零就把条目摘掉。
//
// 计数掉到负数说明释放次数多于获取次数（释放函数被调了两次、或者手写了一次释放），
// 那是编程错误：继续跑下去会让下一个持有者拿到一把已经被别人释放的锁。panic 而不是
// 修正计数。
func (s *sessionLockSet) drop(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.locks[session]
	if !ok {
		panic(fmt.Sprintf("runtime: release session run lock for %q: no lock is registered", session))
	}
	entry.refs--
	if entry.refs < 0 {
		panic(fmt.Sprintf("runtime: release session run lock for %q: reference count went negative", session))
	}
	if entry.refs == 0 {
		delete(s.locks, session)
	}
}

// heldSessionRunLocksKey 是 ctx 里「本调用栈已经持有哪些会话锁」的键。
type heldSessionRunLocksKey struct{}

// withHeldSessionRunLock 把一个已持有的会话号记进 ctx。
//
// 存的是一份拷贝而不是原地追加：ctx 会分叉（并行委派把同一个父 ctx 传给多个子任务），
// 共享同一个底层数组会让兄弟之间互相看见对方的持有集。
func withHeldSessionRunLock(ctx context.Context, session string) context.Context {
	held := heldSessionRunLocks(ctx)
	next := make([]string, len(held), len(held)+1)
	copy(next, held)
	next = append(next, session)
	return context.WithValue(ctx, heldSessionRunLocksKey{}, next)
}

// heldSessionRunLocks 读出本调用栈已经持有的会话号。
func heldSessionRunLocks(ctx context.Context) []string {
	held, ok := ctx.Value(heldSessionRunLocksKey{}).([]string)
	if !ok {
		return nil
	}
	return held
}

// holdsSessionRunLock 说明本调用栈是否已经持有该会话的执行锁。
func holdsSessionRunLock(ctx context.Context, session string) bool {
	for _, s := range heldSessionRunLocks(ctx) {
		if s == session {
			return true
		}
	}
	return false
}

// acquireSessionRunLock 是 RunTask 的入口：取得该会话的执行锁，并把它登记进返回的
// ctx，供更深处的嵌套 RunTask（委派子任务）识别。
//
// 嵌套是真实存在的：delegation.go 在父任务的 RunTask 里同步调子任务的 RunTask。今天
// 子任务的 SessionID 一律留空（决定 D-A/D-B），会话号退到子任务自己的 ID，所以两者
// 永远不是同一把锁。但这把锁不可重入，一旦将来有人给子任务塞上父会话号，就会**自己
// 等自己**——那是一个没有任何日志、没有任何错误的死锁。所以在这里直接判出来并报错：
// 卡住的进程比一条错误难查得多。
func acquireSessionRunLock(ctx context.Context, session string) (context.Context, func(), error) {
	if holdsSessionRunLock(ctx, session) {
		return nil, nil, fmt.Errorf("acquire session run lock for %q: this call stack already holds it; "+
			"a nested run on the same session would deadlock (the lock is not reentrant)", session)
	}
	release, err := sessionRunLocks.acquire(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	return withHeldSessionRunLock(ctx, session), release, nil
}
