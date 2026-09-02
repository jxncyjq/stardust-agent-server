package eventbridge

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// 一条 session_event 翻译出来必须带 session_id / seq / event_type：这三个字段是
// spec §7 给前端的全部依据——按 seq 连续性判断漏没漏帧，漏了回到
// GET /v1/sessions/{id}/events 从断点补拉。
//
// seq 用 0：0 是一条真实的 seq（日志的第一条），任何「零值即缺省」的省略写法都会
// 让第一帧丢掉 seq，而前端会把「没有 seq」和「seq=0」当成两回事。这条用例就是钉
// 这一点的，别把它改成非零值。
func TestTranslateCarriesTheSessionEventAddress(t *testing.T) {
	envelope := translate(domain.RuntimeEvent{
		Type:             domain.RuntimeEventSessionEvent,
		TaskID:           "task-1",
		SessionID:        "sess-1",
		Seq:              0,
		SessionEventType: "turn/start",
	})

	if envelope.Type != "session_event" {
		t.Errorf("envelope.Type = %q, want %q", envelope.Type, "session_event")
	}
	if envelope.SubjectID != "sess-1" {
		t.Errorf("envelope.SubjectID = %q, want the session %q: the session is what this frame is about",
			envelope.SubjectID, "sess-1")
	}
	if got := envelope.Data["session_id"]; got != "sess-1" {
		t.Errorf("data[session_id] = %v, want %q", got, "sess-1")
	}
	seq, present := envelope.Data["seq"]
	if !present {
		t.Fatalf("data 里没有 seq：seq=0 是日志第一条的真实 seq，不能当成缺省省略掉\ndata=%v", envelope.Data)
	}
	if seq != int64(0) {
		t.Errorf("data[seq] = %v (%T), want int64(0)", seq, seq)
	}
	if got := envelope.Data["event_type"]; got != "turn/start" {
		t.Errorf("data[event_type] = %v, want %q", got, "turn/start")
	}
}

// 别的事件不该长出会话字段：它们没有 seq，凭空多一个 "seq":0 会让订阅者以为
// 每一条生命周期事件都在某条会话日志里有位置。
func TestTranslateLeavesNonSessionEventsWithoutSessionFields(t *testing.T) {
	envelope := translate(domain.RuntimeEvent{
		Type:    "task_completed",
		TaskID:  "task-1",
		Message: "done",
	})

	for _, key := range []string{"session_id", "seq", "event_type"} {
		if _, present := envelope.Data[key]; present {
			t.Errorf("task_completed 的 data 里出现了 %q：会话字段只属于 session_event\ndata=%v", key, envelope.Data)
		}
	}
	if envelope.SubjectID != "task-1" {
		t.Errorf("envelope.SubjectID = %q, want the task %q (unchanged)", envelope.SubjectID, "task-1")
	}
}

// 判据必须是 Type，不是 SessionID 是否非空：SessionID 是个通用名字的公开字段，
// 未来任何人在非 session_event 的事件上顺手填了 SessionID（字段名本身就诱导这么做），
// 若判据看 SessionID != ""，该事件会凭空长出 "seq":0 与 "event_type":""，SubjectID
// 也会被错误改写成会话号——这正是 review 里点名的「唯一还能发出错误 seq 的口子」。
// 这条用例专门覆盖 TestTranslateLeavesNonSessionEventsWithoutSessionFields 看不见的
// 方向：那条用例的 SessionID 是空字符串，测的其实是当前判据本身。
func TestTranslateIgnoresSessionIDWhenTypeIsNotSessionEvent(t *testing.T) {
	envelope := translate(domain.RuntimeEvent{
		Type:             "task_completed",
		TaskID:           "task-1",
		SessionID:        "sess-1",
		Seq:              7,
		SessionEventType: "turn/start",
	})

	for _, key := range []string{"session_id", "seq", "event_type"} {
		if _, present := envelope.Data[key]; present {
			t.Errorf("task_completed 携带了非空 SessionID，但它不是 session_event：data 里不该出现 %q\ndata=%v", key, envelope.Data)
		}
	}
	if envelope.SubjectID != "task-1" {
		t.Errorf("envelope.SubjectID = %q, want the task %q:非 session_event 事件即便带了 SessionID，subject 也不能被改写成会话号", envelope.SubjectID, "task-1")
	}
}
