package runtime

import (
	"sync"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// eventRecorder 把一次任务执行写成会话事件日志（spec §5）。
//
// 它是**每次 RunTask 一个**：seq 游标、缓冲区都属于这一次执行，不跨任务共享。
// 真正的串行化与 seq 连续性由 P1 的 store 保证（它在事务内查 next-seq 并持 per-session
// 写锁），这里只负责「发什么、什么时候必须落盘」。
type eventRecorder struct {
	store   port.SessionEventStore
	session string

	mu      sync.Mutex
	pending []domain.SessionEvent
	nextSeq int64
	// seqKnown 表示 nextSeq 已经与库对齐过。第一次 flush 之前不知道库里走到哪，
	// 所以 seq 在 flush 时才最终确定（见 flush）。
	seqKnown bool
	turn     int
	step     int
}

// newEventRecorder 建一次任务执行的记录器。
//
// 会话号取 task.SessionID，为空时退到 task.ID（决定 D-A：单次任务与委派子任务没有
// 会话号，让它们各自成为一条短日志，比加特例分支更简单，轨迹也一样看得到）。
// 两者都空说明这条任务没有任何身份——写出来的事件谁也认不回去，直接 panic：
// 这是编程错误，不是运行期状况。
func newEventRecorder(store port.SessionEventStore, task domain.Task) *eventRecorder {
	session := task.SessionID
	if session == "" {
		session = task.ID
	}
	if session == "" {
		panic("runtime: event recorder needs a session id or a task id; a task with neither cannot own a log")
	}
	return &eventRecorder{store: store, session: session}
}

// sessionID 是这次执行写入的会话日志。
func (e *eventRecorder) sessionID() string { return e.session }

// enabled 说明这个部署是否记录会话事件。
//
// 没有配 store 是**契约允许的可选**（见 Config.SessionEvents），不是错误：内存后端与
// 大量测试构造都不配。它与「配了但写不进去」是两回事——后者由 flush 硬失败。
func (e *eventRecorder) enabled() bool { return e != nil && e.store != nil }
