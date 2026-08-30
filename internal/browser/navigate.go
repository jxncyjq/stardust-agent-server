package browser

import (
	"fmt"
	"sort"
	"time"

	"github.com/go-rod/rod"
)

// NavigateReq 是一次**人工**导航：要么给一个 URL，要么给一个动作。
//
// 两者互斥而不是「URL 为空就当作 reload」：一次误发的空请求会变成一次刷新，而刷新
// 会丢掉页面上没提交的输入。空请求是错误，不是默认值。
type NavigateReq struct {
	URL string `json:"url,omitempty"`
	// Action 取 back|forward|reload。
	Action string `json:"action,omitempty"`
}

// 人工导航支持的动作。白名单而不是 switch 的 default 兜底：一个拼错的动作名
// （"reolad"）该被拒绝，而不是变成「什么也没发生」——界面上两者看不出区别。
const (
	NavigateBack    = "back"
	NavigateForward = "forward"
	NavigateReload  = "reload"
)

// NavigateTakeover 在**接管中**的会话上执行一次人工导航。
//
// 它与 Agent 的 Open 走同一个会话、同一把会话锁、同一套 URL 策略（出口代理也仍在
// 路径上）。区别只在谁在开车：只在接管中允许，否则人和 Agent 会互相把页面开走，
// 而两边都会以为是对方的 bug。
func (r *Runtime) NavigateTakeover(sessionID string, req NavigateReq) error {
	if req.URL == "" && req.Action == "" {
		return NewBrowserError(CodeInvalidInput, "a navigation needs either a url or an action")
	}
	if req.URL != "" && req.Action != "" {
		return NewBrowserError(CodeInvalidInput, "a navigation takes a url or an action, not both")
	}
	if req.Action != "" && req.Action != NavigateBack && req.Action != NavigateForward &&
		req.Action != NavigateReload {
		return NewBrowserError(CodeInvalidInput, fmt.Sprintf(
			"unknown navigation action %q: use %s, %s or %s", req.Action,
			NavigateBack, NavigateForward, NavigateReload))
	}
	if req.URL != "" {
		// 接管不是提权：一个人在地址栏里敲 169.254.169.254 与 Agent 打开它，风险
		// 完全相同。
		if err := r.checkURL(req.URL); err != nil {
			return err
		}
	}

	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return NewBrowserError(CodeSessionNotFound, "unknown session "+sessionID)
	}
	if !r.takeoverOf(sess) {
		return NewBrowserError(CodeTakeoverRequired,
			"session "+sessionID+" is not under takeover; enable takeover before navigating by hand")
	}

	_, page, err := r.activePage(sessionID)
	if err != nil {
		return err
	}
	if err := navigatePage(page, req); err != nil {
		return err
	}
	r.touch(sess) // 人工导航也算活跃，否则 reaper 会在用户读页面时把会话回收掉
	return nil
}

// navigatePage 把一次请求落到页面上，并等加载结束——不等的话，紧随其后的一次
// 观测/截图拍到的还是上一页，而用户看到的是「点了没反应」。
func navigatePage(page *rod.Page, req NavigateReq) error {
	var err error
	switch {
	case req.URL != "":
		err = page.Navigate(req.URL)
	case req.Action == NavigateBack:
		err = page.NavigateBack()
	case req.Action == NavigateForward:
		err = page.NavigateForward()
	default:
		err = page.Reload()
	}
	if err != nil {
		return NewBrowserErrorWrap(CodeNavigationTimeout, "navigate", err)
	}
	if err := page.WaitLoad(); err != nil {
		return NewBrowserErrorWrap(CodeNavigationTimeout, "wait for the navigation to finish", err)
	}
	return nil
}

// SessionInfo 是「现在在哪、谁在开车」——地址栏与接管开关据它渲染。
//
// 它读的是会话状态而不是页面：页面可能已经被 TTL 回收（Context==nil），而地址栏
// 仍然应当显示上一次去过的地方，那正是重建之后会回到的位置。
type SessionInfo struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Takeover  bool   `json:"takeover"`
	// HasPage 表示物理页还在不在。false 不是错误：会话被 TTL 回收后是懒态，下一次
	// 动作会重建它。界面据此显示「已休眠」而不是把它当成断线。
	HasPage bool `json:"has_page"`
	// ChatSessionID 是这个浏览器会话绑定的对话（空=未绑定）。标签条据它只显示
	// 当前对话的会话。
	ChatSessionID string `json:"chat_session_id,omitempty"`

	// createdAt 只用来排序，不出现在 JSON 里：标签的顺序要稳定，但「什么时候建的」
	// 不是前端要显示的东西，把它放进契约只会多一个将来要维护的字段。
	createdAt time.Time
}

// SessionInfo 返回一个会话的当前状态。
func (r *Runtime) SessionInfo(sessionID string) (SessionInfo, error) {
	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return SessionInfo{}, NewBrowserError(CodeSessionNotFound, "unknown session "+sessionID)
	}
	info := SessionInfo{SessionID: sessionID, Takeover: r.takeoverOf(sess)}
	sess.WithLock(func() {
		info.URL = sess.ActiveURL
		info.HasPage = sess.ActivePage != nil && sess.ActivePage.page != nil
	})
	return info, nil
}

// ListSessions 回答「现在有哪些浏览器会话」，可按 chat session 过滤。
//
// 它是标签式切换的前提：浏览器视图此前只认 SSE 报的最后一个会话，而一个对话里
// Agent 完全可能开过好几个（查完 A 站再查 B 站）——除了最后那个，其余都没有入口，
// 用户看不见也回不去。
//
// chatSessionID 为空表示不过滤。按对话过滤是常态：视图跟着当前对话走，把别的对话
// 的会话摆进标签条只会让人点进一个与眼前工作无关的页面。
func (r *Runtime) ListSessions(chatSessionID string) []SessionInfo {
	sessions := r.sessions.Snapshot()
	infos := make([]SessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		var (
			info    SessionInfo
			chatID  string
			created time.Time
		)
		info.SessionID = sess.ID
		info.Takeover = r.takeoverOf(sess)
		sess.WithLock(func() {
			info.URL = sess.ActiveURL
			info.HasPage = sess.ActivePage != nil && sess.ActivePage.page != nil
			chatID = sess.ChatSessionID
			created = sess.CreatedAt
		})
		if chatSessionID != "" && chatID != chatSessionID {
			continue
		}
		info.ChatSessionID = chatID
		info.createdAt = created
		infos = append(infos, info)
	}
	// 最早的排在前面：标签的顺序必须**稳定**，否则每次刷新标签都在跳，用户点到的
	// 不是上一秒看到的那个。Snapshot 走的是 map，顺序本身没有意义。
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].createdAt.Equal(infos[j].createdAt) {
			// 同一纳秒创建（测试里很容易撞上）时按 id 兜住顺序，仍然是稳定的。
			return infos[i].SessionID < infos[j].SessionID
		}
		return infos[i].createdAt.Before(infos[j].createdAt)
	})
	return infos
}
