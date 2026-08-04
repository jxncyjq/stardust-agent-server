package browser

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

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
	ScreencastFPS     int // screencast 限帧率（<=0 时 screencaster 回落到默认 8fps）
}

// Runtime 是 RuntimeAPI 的 go-rod 实现。
type Runtime struct {
	mgr           *Manager
	sessions      *SessionStore
	cfg           RuntimeConfig
	hubs          *hubRegistry
	screencasters *sync.Map // sessionID(string) → *screencaster；仅有订阅者时活跃
	// lifecycles 按会话惰性持有一把 lifecycle 锁（sessionID → *sync.Mutex）：把该会话
	// 「订阅者计数检查 + hub 增删 + screencaster Start/Store/Stop/LoadAndDelete」这整段
	// start/stop 决策串行化，杜绝并发订阅/取消导致的 double-start、miss-start 或
	// cancel 与 Store 交错留下孤儿 screencaster。零值可用，供 struct-literal 构造的测试直接用。
	lifecycles sync.Map
}

var _ RuntimeAPI = (*Runtime)(nil)

// NewRuntime 拉起底层 Manager（Chromium 进程）并返回 go-rod 运行时。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	mgr, err := NewManager(ManagerConfig{Headless: cfg.Headless, BinPath: cfg.BinPath})
	if err != nil {
		return nil, err
	}
	return &Runtime{
		mgr:           mgr,
		sessions:      NewSessionStore(),
		cfg:           cfg,
		hubs:          newHubRegistry(),
		screencasters: &sync.Map{},
	}, nil
}

// hubRegistry 按会话 id 惰性持有 Hub。并发安全。
type hubRegistry struct {
	mu   sync.Mutex
	byID map[string]*Hub
}

func newHubRegistry() *hubRegistry { return &hubRegistry{byID: make(map[string]*Hub)} }

func (hr *hubRegistry) get(sessionID string) *Hub {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	h, ok := hr.byID[sessionID]
	if !ok {
		h = NewHub(64)
		hr.byID[sessionID] = h
	}
	return h
}

func (hr *hubRegistry) drop(sessionID string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	delete(hr.byID, sessionID)
}

// lifecycleMu 返回该会话的 lifecycle 锁（惰性创建）。所有对该会话 screencast 的
// start/stop 决策都必须在这把锁下完成，使订阅者计数检查与 Store/LoadAndDelete 不可交错。
func (r *Runtime) lifecycleMu(sessionID string) *sync.Mutex {
	m, _ := r.lifecycles.LoadOrStore(sessionID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Subscribe 实现 RuntimeAPI：会话必须存在。第一个订阅者接入时开 screencast，
// 最后一个断开（取消后订阅者数归零）时停 screencast——不看视图不推帧。
//
// 整段「hub.Subscribe + 计数检查 + start」以及取消函数里的「hubCancel + 计数检查 + stop」
// 都在会话 lifecycle 锁下串行执行（TOCTOU 修复）：两个并发订阅者不会都看到 count==1 而
// double-start，取消也不会与另一路的 Store 交错留下孤儿 screencaster。
func (r *Runtime) Subscribe(sessionID string) (<-chan StreamEvent, func(), error) {
	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return nil, nil, NewBrowserError(CodeContextEvicted, "unknown session "+sessionID)
	}
	hub := r.hubs.get(sessionID)
	lm := r.lifecycleMu(sessionID)
	lm.Lock()
	ch, hubCancel := hub.Subscribe()
	if hub.SubscriberCount() == 1 {
		r.startScreencastLocked(sess, hub)
	}
	lm.Unlock()
	cancel := func() {
		lm.Lock()
		defer lm.Unlock()
		hubCancel()
		if hub.SubscriberCount() == 0 {
			r.stopScreencast(sessionID)
		}
	}
	return ch, cancel, nil
}

// startScreencastLocked 为会话活跃 page 开 screencast 并登记。调用方须已持有该会话的
// lifecycle 锁且已判定这是第一个订阅者。无 screencasters（struct-literal 测试构造）或
// 无活跃 page 时静默跳过。screencast 只服务于「看」这个视图，start 失败不致命：观测/进度
// 仍走 SSE，只是暂时没有帧——但按 fail-loud 铁律不再吞错，记 Warn 后继续。
func (r *Runtime) startScreencastLocked(sess *Session, hub *Hub) {
	if r.screencasters == nil {
		return
	}
	page := r.pageOf(sess)
	if page == nil {
		return
	}
	sc := newScreencaster(r.cfg.ScreencastFPS, sess.ID)
	r.screencasters.Store(sess.ID, sc)
	if err := sc.Start(page, hub); err != nil {
		slog.Warn("browser: start screencast failed", "session", sess.ID, "err", err)
	}
}

// stopScreencast 停止并释放会话的 screencaster（无则幂等）。
func (r *Runtime) stopScreencast(sessionID string) {
	if r.screencasters == nil {
		return
	}
	if v, ok := r.screencasters.LoadAndDelete(sessionID); ok {
		v.(*screencaster).Stop()
	}
}

// restartScreencastIfActive 在会话仍有订阅者时按新活跃 page 重启 screencast。
// 换页（Open 复用会话再导航）后旧 page 的帧流失效，需绑到新 page。无订阅者则不动。
//
// 与 Subscribe/cancel 共用同一把会话 lifecycle 锁：把「计数检查 + stop 旧 + start 新」
// 整段串行化，避免与并发的订阅/取消交错（TOCTOU 修复）。
func (r *Runtime) restartScreencastIfActive(sess *Session) {
	if r.screencasters == nil {
		return
	}
	hub := r.hubs.get(sess.ID)
	lm := r.lifecycleMu(sess.ID)
	lm.Lock()
	defer lm.Unlock()
	if hub.SubscriberCount() == 0 {
		return
	}
	r.stopScreencast(sess.ID)
	page := r.pageOf(sess)
	if page == nil {
		return
	}
	sc := newScreencaster(r.cfg.ScreencastFPS, sess.ID)
	r.screencasters.Store(sess.ID, sc)
	if err := sc.Start(page, hub); err != nil {
		slog.Warn("browser: restart screencast failed", "session", sess.ID, "err", err)
	}
}

// pageOf 取会话活跃页的 *rod.Page（与 activePage 的断言一致），无则 nil。
// M2：ActivePage 由 Open 在 sess 锁下写，这里在同一把锁下读，避免与在途 Open 竞态。
func (r *Runtime) pageOf(sess *Session) *rod.Page {
	if sess == nil {
		return nil
	}
	var p *rod.Page
	sess.WithLock(func() {
		if sess.ActivePage == nil || sess.ActivePage.page == nil {
			return
		}
		p, _ = sess.ActivePage.page.(*rod.Page)
	})
	return p
}

// ReplaySince 返回会话 Hub 中 seq>lastID 的缓冲 status 事件，供 SSE 断线重连补发。
// 未知会话返回 nil（补发是尽力而为：会话不存在则无可补，实时订阅另经 Subscribe 报错）。
// 该方法不属于 RuntimeAPI 接口，仅让 *Runtime 满足 server.BrowserStreamer。
func (r *Runtime) ReplaySince(sessionID string, lastID uint64) []StreamEvent {
	if _, ok := r.sessions.Get(sessionID); !ok {
		return nil
	}
	return r.hubs.get(sessionID).ReplaySince(lastID)
}

// emitProgress 往会话 Hub 发一条 progress 事件（无订阅者也安全——Hub 仍分配 seq/缓冲）。
func (r *Runtime) emitProgress(sessionID, action, status, ref string) {
	r.hubs.get(sessionID).Publish(StreamEvent{
		Type: EventProgress,
		Data: map[string]any{"action": action, "status": status, "ref": ref},
	})
}

// emitObservation 往会话 Hub 发一条 observation 事件。
func (r *Runtime) emitObservation(sessionID string, obs Observation) {
	r.hubs.get(sessionID).Publish(StreamEvent{Type: EventObservation, Data: obs})
}

// Open 导航到 req.URL：复用或新建 Session + incognito Context，返回首次观测与 session id。
func (r *Runtime) Open(ctx context.Context, req OpenReq) (OpenObservation, error) {
	if err := r.checkURL(req.URL); err != nil {
		return OpenObservation{}, err
	}
	// I4 fail-loud：只有 SessionID 为空才新建。传了非空但查无此 Session 说明它已被
	// 回收/失效，静默 mint 一个新 Session 会掩盖状态漂移——按 CONTEXT_EVICTED 报错。
	var sess *Session
	if req.SessionID == "" {
		sess = r.sessions.Create(req.TaskID)
		c, err := r.mgr.AcquireContext(ContextOpts{})
		if err != nil {
			return OpenObservation{}, err
		}
		sess.Context = c
	} else {
		s, ok := r.sessions.Get(req.SessionID)
		if !ok {
			return OpenObservation{}, NewBrowserError(CodeContextEvicted, "unknown session "+req.SessionID)
		}
		sess = s
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
		// I5：复用 Session 再次导航时，先关掉旧活跃页，否则旧 page 泄漏。
		if sess.ActivePage != nil {
			if old, ok := sess.ActivePage.page.(*rod.Page); ok && old != nil {
				_ = old.Close() // best-effort；新页已是活跃目标，旧页关闭失败不影响本次导航
			}
		}
		sess.ActivePage = &pageHandle{page: page}
		obs, opErr = r.observe(page, sess)
	})
	if opErr != nil {
		return OpenObservation{}, opErr
	}
	r.emitObservation(sess.ID, obs)
	r.emitProgress(sess.ID, "open", "done", "")
	// 换页后活跃 page 变了：若已有订阅者，把 screencast 重绑到新 page。
	r.restartScreencastIfActive(sess)
	return OpenObservation{SessionID: sess.ID, Observation: obs}, nil
}

// Read 只读地重新抽取当前活跃页的 a11y 观测。
func (r *Runtime) Read(ctx context.Context, req ReadReq) (Observation, error) {
	sess, page, err := r.activePage(req.SessionID)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	var opErr error
	sess.WithLock(func() { obs, opErr = r.observe(page, sess) })
	if opErr != nil {
		return Observation{}, opErr
	}
	r.emitObservation(req.SessionID, obs)
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
		el, err := r.elementByRef(page, sess, req.Ref)
		if err != nil {
			opErr = err
			return
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			opErr = NewBrowserError(CodeElementNotFound, "click ref "+req.Ref+": "+err.Error())
			return
		}
		_ = page.WaitLoad() // best-effort：点击可能不触发导航，等不到 load 不算失败（M1 有意保留）
		obs, opErr = r.observe(page, sess)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	r.emitProgress(req.SessionID, "click", "done", req.Ref)
	r.emitObservation(req.SessionID, obs)
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
		el, err := r.elementByRef(page, sess, req.Ref)
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
			_ = page.WaitLoad() // best-effort：提交可能不触发导航（M1 有意保留）
		}
		obs, opErr = r.observe(page, sess)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	r.emitProgress(req.SessionID, "type", "done", req.Ref)
	r.emitObservation(req.SessionID, obs)
	return obs, nil
}

// Close 关闭指定 Session（释放其 Context）；SessionID 为空则关闭整个运行时进程。
func (r *Runtime) Close(ctx context.Context, req CloseReq) error {
	if req.SessionID != "" {
		if sess, ok := r.sessions.Get(req.SessionID); ok {
			// 不吞 ReleaseContext 的错误（CLAUDE.md §0）：即使释放失败也把 Session
			// 从内存表删掉（进程级 Close 时进程整体回收），但错误照常上报。
			relErr := r.mgr.ReleaseContext(sess.Context)
			r.sessions.Delete(req.SessionID)
			r.stopScreencast(req.SessionID) // 停帧流，须在 drop hub 前
			r.hubs.drop(req.SessionID)
			if relErr != nil {
				return NewBrowserErrorWrap(CodeContextEvicted, "release session "+req.SessionID, relErr)
			}
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

// observe 抽 CDP a11y 树 → 裁剪观测，并把每个分配到 ref 的节点的 BackendDOMNodeID
// 记进 sess.Refs（ref → backendNodeID 字符串），供 elementByRef 在动作时精确定位。
//
// C1 fail-loud：AX 树抽取失败不再返回「(a11y unavailable)」的假观测冒充成功，而是
// 返回 error，让 Open/Read/Click/Type 如实失败。
//
// I3 精确映射：ref 的分配顺序与 BuildObservation 完全一致（interactive && visible、
// 按输入顺序），因此这里对同一 keep 过滤收集的 keptBackend 与 obs.Elements 一一对齐，
// e1、e2… 各自绑定到当时那个节点的 backendNodeID，杜绝按 DOM 位置重查导致的错位。
func (r *Runtime) observe(page *rod.Page, sess *Session) (Observation, error) {
	tree, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		return Observation{}, NewBrowserErrorWrap(CodeContextEvicted, "get a11y tree", err)
	}
	var raw []RawA11yNode
	var keptBackend []proto.DOMBackendNodeID
	for _, n := range tree.Nodes {
		if n.Ignored {
			continue
		}
		role := axValueString(n.Role)
		node := RawA11yNode{
			Role:        role,
			Name:        axValueString(n.Name),
			Value:       axValueString(n.Value),
			Interactive: isInteractiveRole(role),
			Visible:     true, // Phase 1 近似：未被 Ignored 视为可见
		}
		raw = append(raw, node)
		// 复刻 BuildObservation 的 keep 判据（interactive && visible），按同序收集
		// backendNodeID，保证与后续分配的 ref 对齐。
		if node.Interactive && node.Visible {
			keptBackend = append(keptBackend, n.BackendDOMNodeID)
		}
	}
	obs := BuildObservation(raw, ObservationBudget{MaxElements: r.cfg.MaxElements})
	if sess != nil {
		// 每次观测重建 ref 表：清掉上一轮的 ref → backendNodeID，避免旧页遗留的高号
		// ref 指向失效节点。observe 始终在会话锁下调用，重建安全。
		sess.Refs = make(map[string]string, len(obs.Elements))
		for i, e := range obs.Elements {
			if i >= len(keptBackend) {
				break // 理论不达：obs.Elements 是 keptBackend 的前缀（可能因预算截断变短）
			}
			sess.Refs[e.Ref] = strconv.Itoa(int(keptBackend[i]))
		}
	}
	return obs, nil
}

// axValueString 把 CDP AX 属性值渲染成纯字符串（gson.JSON.Str() 对字符串值最贴切）。
// M4 Phase-1 取舍：仅对字符串型 AX 值有效，非字符串值（bool/number/token 列表等）
// 会渲染成空串——可接受，后续 Phase 再按类型细化。
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

// elementByRef 把观测里的 ref 精确映射回页面元素：用上一次 observe 记进 sess.Refs 的
// BackendDOMNodeID 经 CDP DOMResolveNode 解析成远端对象再取元素。
//
// I3 修复：旧实现按「a11y 树的 keep 顺序 → 分配 ref」，动作时却用一个不同的 CSS 选择器
// 按 DOM 顺序重查并按位置取第 n 个——两者元素集合与顺序都可能不同，ref 会指到错的元素。
// 现在直接锁定当时那个节点，ref → 元素严格一一对应，不再依赖位置。
func (r *Runtime) elementByRef(page *rod.Page, sess *Session, ref string) (*rod.Element, error) {
	idStr, ok := sess.Refs[ref]
	if !ok {
		return nil, NewBrowserError(CodeElementNotFound, "ref "+ref+" not found; re-read")
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return nil, NewBrowserErrorWrap(CodeElementNotFound, "ref "+ref+" has invalid backend node id "+idStr, err)
	}
	resolved, err := proto.DOMResolveNode{BackendNodeID: proto.DOMBackendNodeID(id)}.Call(page)
	if err != nil {
		return nil, NewBrowserErrorWrap(CodeElementNotFound, "ref "+ref+" resolve node; re-read", err)
	}
	if resolved.Object == nil {
		return nil, NewBrowserError(CodeElementNotFound, "ref "+ref+" resolved to no object; re-read")
	}
	el, err := page.ElementFromObject(resolved.Object)
	if err != nil {
		return nil, NewBrowserErrorWrap(CodeElementNotFound, "ref "+ref+" element from object; re-read", err)
	}
	return el, nil
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
		// M3：IsUnspecified 覆盖 0.0.0.0 / ::（未指定地址，常被用来绕过回环/私网拦截）。
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return NewBrowserError(CodePrivateHostBlocked, "host "+host+" resolves to private ip "+ip.String())
		}
	}
	return nil
}
