package browser

import (
	"testing"
)

// 接管中的人只能看和点：地址栏、后退、刷新这些**导航**动作此前没有任何入口——
// 浏览器视图因此只是一个能点击的录像，用户想回上一页都得让 Agent 去做。
//
// 这些动作与 Agent 的 open 是同一件事，所以不能各写一份：它们走同一个会话、同一
// 把会话锁、同一套 URL 策略（出口代理仍然在路径上）。区别只在**谁在开车**——
// 因此人工导航只在接管中允许，否则两边会互相把页面开走。

func TestHumanNavigationRequiresTakeover(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	sess := rt.sessions.Create("task-1")

	err := rt.NavigateTakeover(sess.ID, NavigateReq{URL: "https://example.com/"})
	if err == nil {
		t.Fatal("navigation succeeded outside takeover: the human and the agent would be driving at once")
	}
	if got := codeOf(t, err); got != CodeTakeoverRequired {
		t.Errorf("code = %s, want %s", got, CodeTakeoverRequired)
	}
}

func TestHumanNavigationRefusesAnUnknownSession(t *testing.T) {
	t.Parallel()

	err := newRuntimeWithNoSessions(t).NavigateTakeover("nope", NavigateReq{URL: "https://example.com/"})
	if got := codeOf(t, err); got != CodeSessionNotFound {
		t.Errorf("code = %s, want %s", got, CodeSessionNotFound)
	}
}

// TestHumanNavigationIsSubjectToTheSameURLPolicy: 接管不是提权。一个人在地址栏里
// 敲 169.254.169.254 与 Agent 打开它，风险完全相同。
func TestHumanNavigationIsSubjectToTheSameURLPolicy(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	sess := rt.sessions.Create("task-1")
	if err := rt.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}

	err := rt.NavigateTakeover(sess.ID, NavigateReq{URL: "file:///etc/passwd"})
	if got := codeOf(t, err); got != CodeProtocolBlocked {
		t.Errorf("code = %s, want %s: takeover is not an escalation", got, CodeProtocolBlocked)
	}
}

// TestOnlyKnownNavigationActionsAreAccepted: 猜一个动作名（把 "reolad" 当 reload）
// 会让一次「刷新」变成什么也没发生，而界面上看不出任何区别。
func TestOnlyKnownNavigationActionsAreAccepted(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	sess := rt.sessions.Create("task-1")
	if err := rt.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}

	err := rt.NavigateTakeover(sess.ID, NavigateReq{Action: "teleport"})
	if got := codeOf(t, err); got != CodeInvalidInput {
		t.Errorf("code = %s, want %s", got, CodeInvalidInput)
	}
}

func TestANavigationNeedsEitherAURLOrAnAction(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	sess := rt.sessions.Create("task-1")
	if err := rt.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}

	if got := codeOf(t, rt.NavigateTakeover(sess.ID, NavigateReq{})); got != CodeInvalidInput {
		t.Errorf("code = %s, want %s for an empty request", got, CodeInvalidInput)
	}
}

// 地址栏要显示「现在在哪」。此前没有任何地方能回答这个问题：观测事件里没有 URL，
// 会话状态也没有出口——于是界面只能显示一个 session id，而用户想知道的是自己在
// 看哪个网站。
func TestSessionInfoAnswersWhereTheBrowserIs(t *testing.T) {
	t.Parallel()

	rt := newRuntimeWithNoSessions(t)
	sess := rt.sessions.Create("task-1")
	sess.WithLock(func() { sess.ActiveURL = "https://example.com/page" })

	info, err := rt.SessionInfo(sess.ID)
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	if info.URL != "https://example.com/page" {
		t.Errorf("URL = %q, want the page the session is on", info.URL)
	}
	if info.Takeover {
		t.Error("Takeover = true on a session nobody took over")
	}

	if err := rt.SetTakeover(sess.ID, true); err != nil {
		t.Fatalf("SetTakeover: %v", err)
	}
	info, err = rt.SessionInfo(sess.ID)
	if err != nil {
		t.Fatalf("SessionInfo after takeover: %v", err)
	}
	if !info.Takeover {
		t.Error("Takeover = false after taking over; the toolbar would show the wrong state")
	}
}

func TestSessionInfoRefusesAnUnknownSession(t *testing.T) {
	t.Parallel()

	_, err := newRuntimeWithNoSessions(t).SessionInfo("nope")
	if got := codeOf(t, err); got != CodeSessionNotFound {
		t.Errorf("code = %s, want %s", got, CodeSessionNotFound)
	}
}
