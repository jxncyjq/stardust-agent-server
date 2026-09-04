package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/taskgate"
)

// 收尾的 turn/end 必须落盘，**哪怕这次运行正是被取消才收的尾**。
//
// closeTurnOnError 是八个错误返回点共用的收尾：记一条 turn/end 再 flush。它以前
// 直接把任务的 ctx 交给 flush——而最常见的收尾原因恰恰是那个 ctx 被取消了。于是
// Append 一进去就因 ctx.Err() 失败，turn/end 不进库，只留下一条 Warn。
//
// 后果不是「少一条事件」这么轻：事件日志里那个 turn 永远开着。
//   - 投影按 turn 边界折叠，一个没有 turn/end 的 turn 会把它之后的东西一起卷进来；
//   - 崩溃恢复（Load）靠「有没有 turn/end」判断要不要补，一个活着的进程留下的开口
//     和一次真崩溃留下的开口在数据上完全一样，谁也分不出来。
//
// 取消是**常规路径**（用户按了停止、上游超时），不是异常，所以这条不能靠「下次
// flush 补上」——这次运行不会再有下一次。
func TestTheClosingTurnEndIsPersistedEvenWhenTheRunWasCancelled(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          &scriptedMaas{},
		Audit:         adapter.NewMemoryAuditLog(),
		Events:        adapter.NewMemoryEventBus(),
		SessionEvents: store,
	})
	task := domain.Task{ID: "t-cancel", SessionID: "s-cancel", AgentID: "a1", Input: "go"}

	rec := newEventRecorder(store, task, nil)
	rec.recordTurnStart(0)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush turn/start: %v", err)
	}

	// 这次运行被取消了——收尾拿到的就是这个已经取消的 ctx。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt.closeTurnOnError(ctx, task, rec, context.Canceled)

	events, err := store.ReadFrom(context.Background(), "s-cancel", 0, 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	var sawTurnEnd bool
	for _, e := range events {
		if e.Type == domain.SessionEventTurnEnd {
			sawTurnEnd = true
		}
	}
	if !sawTurnEnd {
		t.Errorf("被取消的运行没有把 turn/end 落盘（日志里有 %d 条事件）："+
			"那个 turn 在日志里永远开着——投影会把后面的东西卷进来，"+
			"崩溃恢复也分不出它和一次真崩溃留下的开口", len(events))
	}
}

// 取消之外的错误路径同样要落盘——修法不能只对 context.Canceled 生效。
func TestTheClosingTurnEndIsPersistedOnAnOrdinaryError(t *testing.T) {
	t.Parallel()

	store := &captureEventStore{}
	rt := NewRuntime(Config{
		Gate:          taskgate.NewTaskGate(),
		Maas:          &scriptedMaas{},
		Audit:         adapter.NewMemoryAuditLog(),
		Events:        adapter.NewMemoryEventBus(),
		SessionEvents: store,
	})
	task := domain.Task{ID: "t-err", SessionID: "s-err", AgentID: "a1", Input: "go"}

	rec := newEventRecorder(store, task, nil)
	rec.recordTurnStart(0)
	if err := rec.flush(context.Background()); err != nil {
		t.Fatalf("flush turn/start: %v", err)
	}

	rt.closeTurnOnError(context.Background(), task, rec, errors.New("模型挂了"))

	events, err := store.ReadFrom(context.Background(), "s-err", 0, 0)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	for _, e := range events {
		if e.Type == domain.SessionEventTurnEnd {
			return
		}
	}
	t.Errorf("普通错误路径也没把 turn/end 落盘（%d 条事件）", len(events))
}
