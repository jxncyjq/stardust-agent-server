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

// TestParseRunStatusRefusesAnUnknownValue：未知值必须报错。
//
// 这个枚举的四个值是「一次运行可能处在的全部状态」的完整清单，而它要从数据库的
// TEXT 列读回来——那一列的内容可以被手改，也可能是更新的二进制写下的。猜一个默认
// 值（比如当成 running）会让一行来历不明的记录被下一次启动扫成 interrupted，
// 也就是拿一个编造的状态覆盖掉真实的那个。
func TestParseRunStatusRefusesAnUnknownValue(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "RUNNING", "done", "interupted"} {
		if got, err := ParseRunStatus(in); err == nil {
			t.Errorf("ParseRunStatus(%q) = %q, want an error", in, got)
		}
	}
}

// TestParseRunStatusRoundTripsEveryValue：四个值都认得，且 String 与解析互逆。
func TestParseRunStatusRoundTripsEveryValue(t *testing.T) {
	t.Parallel()

	all := []RunStatus{RunStatusRunning, RunStatusCompleted, RunStatusFailed, RunStatusInterrupted}
	for _, want := range all {
		got, err := ParseRunStatus(want.String())
		if err != nil {
			t.Fatalf("ParseRunStatus(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseRunStatus(%q) = %q", want, got)
		}
	}
	// 四个值两两不同：复制粘贴写重了一个字面量，这里会响。
	seen := map[RunStatus]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("两个常量的字面值都是 %q", s)
		}
		seen[s] = true
	}
}

// TestRunStatusConstantsKeepTheirContractValues：这四个字面值是对外契约——它们要
// 落进 SQLite 的运行状态列。改常量名可以，改值会悄悄改掉已落盘数据的含义：库里
// 那些 "running" 会突然变成 ParseRunStatus 不认得的值。
//
// 所以这里的断言写死字面量，不能引用常量本身，否则改常量等于同时改了断言。与
// TestStopReasonConstantsKeepTheirContractValues 同理。
func TestRunStatusConstantsKeepTheirContractValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  RunStatus
		want string
	}{
		{"running", RunStatusRunning, "running"},
		{"completed", RunStatusCompleted, "completed"},
		{"failed", RunStatusFailed, "failed"},
		{"interrupted", RunStatusInterrupted, "interrupted"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("%s run status = %q, want %q", tc.name, string(tc.got), tc.want)
		}
	}
}
