package domain

import "testing"

func TestTaskCancelledStatus(t *testing.T) {
	if TaskCancelled != "cancelled" {
		t.Fatalf("TaskCancelled = %q, want cancelled", TaskCancelled)
	}
}

// 这四个字面值是对外契约：它们落进 SQLite 的 stop_reason 列，也出现在
// delegate_task 回给模型的 JSON 里。改常量名可以，改值会悄悄改掉已落盘数据与
// 输出契约的含义——所以这里断言写死的字面量，不能引用常量本身，否则改常量等于
// 同时改了断言。
func TestStopReasonConstantsKeepTheirContractValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  StopReason
		want string
	}{
		{"completed", StopReasonCompleted, "completed"},
		{"max rounds", StopReasonMaxRounds, "max_rounds"},
		{"tool loop cap", StopReasonToolLoopCap, "tool_loop_cap"},
		{"repeat loop broken", StopReasonRepeatLoopBroken, "repeat_loop_broken"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("%s stop reason = %q, want %q", tc.name, string(tc.got), tc.want)
		}
	}
	// 空串绝不是这四个里的任何一个：零值不许被读成 completed。
	if StopReason("") == StopReasonCompleted {
		t.Error("the zero StopReason equals StopReasonCompleted; an unrecorded reason would read as a finished run")
	}
}
