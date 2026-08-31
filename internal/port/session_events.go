package port

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

// SessionEventStore 是会话事件日志的持久化契约（spec §4.4）。
//
// 三个方法的分工是有意的，不要合并：
//
//   - Append 只追加，且**整批一个事务**——半批写入会留下一个谁也修不好的日志。
//   - ReadFrom **不改库**，只读 seq >= fromSeq 的后缀。轨迹的翻页与增量拉取走它，
//     因为一次「看一眼」不该改变被看的东西。
//   - Load **会改库**：它把崩溃留下的半个 turn 补成合法的 provider transcript
//     （见 spec §4.3 不变量 2）。会话要被重新使用时才调它。
type SessionEventStore interface {
	// Append 追加一批事件。首个事件的 Seq 必须等于该会话当前的 next-seq，
	// 否则拒绝并指出期望值与实际值。
	Append(ctx context.Context, sessionID string, events []domain.SessionEvent) error

	// ReadFrom 返回 seq >= fromSeq 的事件，按 seq 升序。fromSeq 越过末尾返回空切片。
	ReadFrom(ctx context.Context, sessionID string, fromSeq int64) ([]domain.SessionEvent, error)

	// Load 返回该会话的全部事件，必要时先补出崩溃恢复的关闭事件并落盘。
	//
	// **只可对「确定没有活跃写入者」的会话调用**——进程启动时的崩溃恢复，或一个
	// 已经结束的会话。实现看得见的只有事件本身，而「崩掉的半个 turn」与「正在跑、
	// 还没收尾的 turn」在数据上完全等价，这一层没有任何办法区分：对一个活着的会话
	// 调 Load，会把那个进行中的 turn 强行收成 interrupted。判断「会话是否在跑」的
	// 信息只存在于调用方（P2 的会话生命周期）手里，所以这条约束由调用方保证。
	Load(ctx context.Context, sessionID string) ([]domain.SessionEvent, error)
}
