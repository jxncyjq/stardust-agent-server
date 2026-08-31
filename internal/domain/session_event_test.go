package domain

import (
	"strings"
	"testing"
)

// 事件类型是**闭集**：未知类型必须被拒绝，并且错误里要指名是哪一个。
//
// 这条守的是 spec §4.3 不变量 4。它防的不是手滑，而是**版本漂移**：一个新版本
// 写进去的事件类型，被旧版本读到时如果被静默忽略，那条会话的历史就在旧版本眼里
// 少了一段——而少的那段恰好是新功能产生的。
func TestAnUnknownEventTypeIsRefusedByName(t *testing.T) {
	err := ValidateSessionEventType("tool/telepathy")
	if err == nil {
		t.Fatal("未知事件类型被接受了")
	}
	if !strings.Contains(err.Error(), "tool/telepathy") {
		t.Errorf("错误里没有指名那个类型：%v", err)
	}
}

func TestEveryKnownEventTypeIsAccepted(t *testing.T) {
	for _, typ := range []SessionEventType{
		SessionEventTurnStart, SessionEventUserMessage,
		SessionEventStepStart, SessionEventAssistantMessage,
		SessionEventToolCall, SessionEventToolResult,
		SessionEventStepEnd, SessionEventTurnEnd,
	} {
		if err := ValidateSessionEventType(typ); err != nil {
			t.Errorf("ValidateSessionEventType(%q) = %v, want nil", typ, err)
		}
	}
}

// 空类型单独一条：它是「字段忘了填」的形状，与「填了个没见过的」是两回事，
// 错误信息也该不同——否则排查的人会去找一个根本不存在的类型名。
func TestAnEmptyEventTypeSaysItIsEmpty(t *testing.T) {
	err := ValidateSessionEventType("")
	if err == nil {
		t.Fatal("空事件类型被接受了")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("错误没说它是空的：%v", err)
	}
}
