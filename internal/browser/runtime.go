package browser

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// RuntimeConfig 配置运行时。
type RuntimeConfig struct {
	Headless          bool
	BinPath           string
	AllowPrivateHosts bool // 仅测试放开；生产默认 false（SSRF 基础拦截）
	MaxElements       int
}

// Runtime 是 RuntimeAPI 的 go-rod 实现。
type Runtime struct {
	mgr      *Manager
	sessions *SessionStore
	cfg      RuntimeConfig
}

var _ RuntimeAPI = (*Runtime)(nil)

// NewRuntime 拉起底层 Manager（Chromium 进程）并返回 go-rod 运行时。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	mgr, err := NewManager(ManagerConfig{Headless: cfg.Headless, BinPath: cfg.BinPath})
	if err != nil {
		return nil, err
	}
	return &Runtime{mgr: mgr, sessions: NewSessionStore(), cfg: cfg}, nil
}

// Open 导航到 req.URL：复用或新建 Session + incognito Context，返回首次观测与 session id。
func (r *Runtime) Open(ctx context.Context, req OpenReq) (OpenObservation, error) {
	if err := r.checkURL(req.URL); err != nil {
		return OpenObservation{}, err
	}
	sess, _ := r.sessions.Get(req.SessionID)
	if sess == nil {
		sess = r.sessions.Create(req.TaskID)
		c, err := r.mgr.AcquireContext(ContextOpts{})
		if err != nil {
			return OpenObservation{}, err
		}
		sess.Context = c
	}
	if sess.Context == nil || sess.Context.browser == nil {
		return OpenObservation{}, NewBrowserError(CodeContextEvicted, "session "+sess.ID+" has no browser context")
	}
	var obs Observation
	var opErr error
	sess.WithLock(func() {
		page, err := sess.Context.browser.Page(proto.TargetCreateTarget{URL: req.URL})
		if err != nil {
			opErr = NewBrowserError(CodeNavigationTimeout, "open "+req.URL+": "+err.Error())
			return
		}
		if err := page.WaitLoad(); err != nil {
			opErr = NewBrowserError(CodeNavigationTimeout, "wait load "+req.URL+": "+err.Error())
			return
		}
		sess.ActivePage = &pageHandle{page: page}
		obs = r.observe(page)
	})
	if opErr != nil {
		return OpenObservation{}, opErr
	}
	return OpenObservation{SessionID: sess.ID, Observation: obs}, nil
}

// Read 只读地重新抽取当前活跃页的 a11y 观测。
func (r *Runtime) Read(ctx context.Context, req ReadReq) (Observation, error) {
	sess, page, err := r.activePage(req.SessionID)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	sess.WithLock(func() { obs = r.observe(page) })
	return obs, nil
}

// Click 点击 ref 指向的元素，等待可能的导航后返回新观测。
func (r *Runtime) Click(ctx context.Context, req ClickReq) (Observation, error) {
	sess, page, err := r.activePage(req.SessionID)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	var opErr error
	sess.WithLock(func() {
		el, err := r.elementByRef(page, req.Ref)
		if err != nil {
			opErr = err
			return
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			opErr = NewBrowserError(CodeElementNotFound, "click ref "+req.Ref+": "+err.Error())
			return
		}
		_ = page.WaitLoad()
		obs = r.observe(page)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	return obs, nil
}

// Type 向 ref 指向的元素输入文本；Submit 为真则输入后按回车提交。
func (r *Runtime) Type(ctx context.Context, req TypeReq) (Observation, error) {
	sess, page, err := r.activePage(req.SessionID)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	var opErr error
	sess.WithLock(func() {
		el, err := r.elementByRef(page, req.Ref)
		if err != nil {
			opErr = err
			return
		}
		if err := el.Input(req.Text); err != nil {
			opErr = NewBrowserError(CodeElementNotFound, "type into ref "+req.Ref+": "+err.Error())
			return
		}
		if req.Submit {
			if err := page.Keyboard.Type(input.Enter); err != nil {
				opErr = NewBrowserError(CodeElementNotFound, "submit ref "+req.Ref+": "+err.Error())
				return
			}
			_ = page.WaitLoad()
		}
		obs = r.observe(page)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	return obs, nil
}

// Close 关闭指定 Session（释放其 Context）；SessionID 为空则关闭整个运行时进程。
func (r *Runtime) Close(ctx context.Context, req CloseReq) error {
	if req.SessionID != "" {
		if sess, ok := r.sessions.Get(req.SessionID); ok {
			_ = r.mgr.ReleaseContext(sess.Context)
			r.sessions.Delete(req.SessionID)
			return nil
		}
		return NewBrowserError(CodeContextEvicted, "unknown session "+req.SessionID)
	}
	r.mgr.Close()
	return nil
}

// ---- 内部 ----

// activePage 取出 Session 与其活跃 go-rod 页；缺 Session 或缺活跃页均按 CONTEXT_EVICTED 报错。
func (r *Runtime) activePage(sessionID string) (*Session, *rod.Page, error) {
	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return nil, nil, NewBrowserError(CodeContextEvicted, "unknown session "+sessionID)
	}
	if sess.ActivePage == nil || sess.ActivePage.page == nil {
		return nil, nil, NewBrowserError(CodeContextEvicted, "session "+sessionID+" has no active page")
	}
	page, ok := sess.ActivePage.page.(*rod.Page)
	if !ok {
		return nil, nil, NewBrowserError(CodeContextEvicted, "session "+sessionID+" active page has wrong type")
	}
	return sess, page, nil
}

// observe 抽 CDP a11y 树 → 裁剪观测。只保留未被 Ignored 的节点，交互性按 role 近似判定。
func (r *Runtime) observe(page *rod.Page) Observation {
	tree, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		return Observation{Text: "(a11y unavailable)"}
	}
	var raw []RawA11yNode
	for _, n := range tree.Nodes {
		if n.Ignored {
			continue
		}
		role := axValueString(n.Role)
		name := axValueString(n.Name)
		raw = append(raw, RawA11yNode{
			Role:        role,
			Name:        name,
			Value:       axValueString(n.Value),
			Interactive: isInteractiveRole(role),
			Visible:     true, // Phase 1 近似：未被 Ignored 视为可见
		})
	}
	return BuildObservation(raw, ObservationBudget{MaxElements: r.cfg.MaxElements})
}

// axValueString 把 CDP AX 属性值渲染成纯字符串（gson.JSON.Str() 对字符串值最贴切）。
func axValueString(v *proto.AccessibilityAXValue) string {
	if v == nil {
		return ""
	}
	return v.Value.Str()
}

func isInteractiveRole(role string) bool {
	switch role {
	case "button", "link", "textbox", "checkbox", "radio", "combobox", "menuitem", "tab", "searchbox":
		return true
	}
	return false
}

// elementByRef 把观测里的 ref 映射回页面元素。Phase 1 近似实现：按 ref 序号在
// 当前可交互元素列表里取第 n 个（与 observe 的顺序一致）。Phase 2 换成
// backendNodeID 稳定映射（见 spec §3.5，需在 observe 时记 BackendDOMNodeID）。
func (r *Runtime) elementByRef(page *rod.Page, ref string) (*rod.Element, error) {
	obs := r.observe(page)
	idx := -1
	for i, e := range obs.Elements {
		if e.Ref == ref {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, NewBrowserError(CodeElementNotFound, "ref "+ref+" not found; re-read")
	}
	els, err := page.Elements("a, button, input, textarea, select, [role=button], [role=link]")
	if err != nil {
		return nil, NewBrowserError(CodeElementNotFound, "ref "+ref+" lookup failed: "+err.Error())
	}
	if idx >= len(els) {
		return nil, NewBrowserError(CodeElementNotFound, "ref "+ref+" stale; re-read")
	}
	return els[idx], nil
}

// checkURL 做协议白名单 + 私网/回环/链路本地地址的 SSRF 基础拦截（AllowPrivateHosts 放开后者）。
func (r *Runtime) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return NewBrowserError(CodeNavigationTimeout, "parse url: "+err.Error())
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return NewBrowserError(CodeProtocolBlocked, "scheme "+u.Scheme+" blocked")
	}
	if r.cfg.AllowPrivateHosts {
		return nil
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return NewBrowserError(CodePrivateHostBlocked, "resolve "+host+": "+err.Error())
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return NewBrowserError(CodePrivateHostBlocked, "host "+host+" resolves to private ip "+ip.String())
		}
	}
	return nil
}
