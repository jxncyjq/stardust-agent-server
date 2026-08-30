package browser

import (
	"errors"
	"testing"
)

// 这些码是**回给调用方的建议**：ELEMENT_NOT_FOUND 的意思是「ref 失效了，重新
// read 一次」。接管这条路曾经把它当成通用错误码用——一个空批次、一个越界坐标、
// 一个越界视口，都回 ELEMENT_NOT_FOUND，于是接口在告诉人「元素没找到，重新读
// 页面」，而真正的问题是这次请求本身写错了。照建议做是白做。
//
// 另外两处同形：同一个「会话不存在」在三个端点回三种状态码；而
// SESSION_UNDER_TAKEOVER 被用来表示**没有**在接管——同一个码在别处正好表示相反
// 的状态（会话正被人接管，Agent 的写动作挡下）。

func codeOf(t *testing.T, err error) Code {
	t.Helper()

	var be *BrowserError
	if !errors.As(err, &be) {
		t.Fatalf("error %v carries no BrowserError", err)
	}
	return be.Code
}

// newRuntimeWithNoSessions is a Runtime with an empty session store and no
// browser behind it: enough to exercise every refusal that happens before a
// page is ever touched, which is all of them here.
func newRuntimeWithNoSessions(t *testing.T) *Runtime {
	t.Helper()

	return &Runtime{sessions: NewSessionStore(), hubs: newHubRegistry()}
}

func TestAMalformedBatchSaysTheRequestIsWrong(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	for name, events := range map[string][]InputEvent{
		"empty batch":       {},
		"coordinate":        {{Type: "click", X: 1.5, Y: 0.5}},
		"unknown key":       {{Type: "keydown", Key: "F13"}},
		"modifier as key":   {{Type: "keydown", Key: "Control"}},
		"unknown modifier":  {{Type: "keydown", Key: "c", Modifiers: []string{"cmd"}}},
		"char with ctrl":    {{Type: "char", Text: "c", Modifiers: []string{"ctrl"}}},
		"unknown eventtype": {{Type: "teleport"}},
	} {
		err := rt.InjectInput("whatever", events)
		if got := codeOf(t, err); got != CodeInvalidInput {
			t.Errorf("%s: code = %s, want %s (the caller's request is malformed, nothing is missing from the page)",
				name, got, CodeInvalidInput)
		}
	}
}

// TestAnUnknownSessionSaysSoRatherThanClaimingItWasEvicted: CONTEXT_EVICTED
// means "the session you had is gone, rebuild it" — a truthful answer for a
// session that existed. For an id that never existed it invents a history.
func TestAnUnknownSessionSaysSoRatherThanClaimingItWasEvicted(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)

	if got := codeOf(t, rt.SetTakeover("nope", true)); got != CodeSessionNotFound {
		t.Errorf("SetTakeover(unknown) code = %s, want %s", got, CodeSessionNotFound)
	}
	if got := codeOf(t, rt.SetViewport("nope", 800, 600)); got != CodeSessionNotFound {
		t.Errorf("SetViewport(unknown) code = %s, want %s", got, CodeSessionNotFound)
	}
	err := rt.InjectInput("nope", []InputEvent{{Type: "char", Text: "a"}})
	if got := codeOf(t, err); got != CodeSessionNotFound {
		t.Errorf("InjectInput(unknown) code = %s, want %s", got, CodeSessionNotFound)
	}
}

// TestRefusingToInjectWithoutTakeoverDoesNotClaimTheOppositeState: the old code
// answered "session is under takeover" to explain that it is NOT under
// takeover. One code, two mutually exclusive meanings, told apart only by the
// prose after it.
func TestRefusingToInjectWithoutTakeoverDoesNotClaimTheOppositeState(t *testing.T) {
	t.Parallel()

	if CodeTakeoverRequired == CodeTakeover {
		t.Fatal("the two takeover codes are the same value; a caller cannot tell the states apart")
	}
}
