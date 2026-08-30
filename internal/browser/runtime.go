package browser

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
	ScreencastFPS     int           // screencast 限帧率（<=0 时 screencaster 回落到默认 8fps）
	SessionTTL        time.Duration // 会话空闲超过此时长即回收物理 Context；<=0 关闭 TTL 回收
	ReapInterval      time.Duration // reaper 后台扫描间隔；<=0 回落到默认 60s
	// Store 是可选的会话持久化端口：非 nil 时装配写穿并在启动时从盘加载已存会话
	// （Context=nil 懒重建）；nil = 纯内存，Phase 1/2 行为不变。
	Store BrowserSessionStore
	// 快照降级（Task: browser-snapshot-degradation）。
	Extractor             SnapshotExtractor // 可空：nil 则超阈快照只截断不抽取
	Archive               SnapshotArchive   // 可空：nil 时 NewRuntime 装配 fileSnapshotArchive
	SnapshotRuneThreshold int               // 渲染文本超此 rune 数触发降级；<=0 关闭降级
	SnapshotTTL           time.Duration     // 落盘全文保留时长；<=0 不清理
	SnapshotArchiveDir    string            // 相对工具根的落盘子目录；空=默认

	// RequireSandbox：这台机器上没有外层隔离时宁可不启动浏览器（见 ManagerConfig）。
	RequireSandbox bool

	// MaxProcesses / MaxContextsPerProcess / ProcessMemoryLimitBytes 透传给进程池
	// （见 poolConfig）。默认等价于池出现之前的形状：一个进程。
	MaxProcesses            int
	MaxContextsPerProcess   int
	ProcessMemoryLimitBytes uint64

	// MinFreeMemoryBytes 是**新建**浏览器会话的可用内存下限；<=0 关闭这条策略。
	//
	// 只挡新建：已经开着的会话继续可用——把用户正在看的页面掐掉，比多占一点内存
	// 更糟；复用同一个 chat session 的也照旧。
	MinFreeMemoryBytes uint64

	// Logger 记录沙箱、出网与内存三类决定。一次被出网策略挡下的导航在页面上只是
	// 一条 403，运维要能在日志里看到它挡的是什么地址——否则「这个站点打不开」就
	// 成了一个查不动的问题。nil 时丢弃。
	Logger *slog.Logger
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
	// reaperCancel 取消后台 TTL reaper goroutine 的 ctx；全量 Close（SessionID=="" 分支）时调用。
	// struct-literal 构造的测试为 nil，Close 全量分支对 nil 安全。
	reaperCancel context.CancelFunc
	// availableMemory 读整机可用物理内存。它是一个字段而不是直接调 PAL，因为
	// 「内存不够时拒绝新建」这条策略无法靠真实机器复现：要么这台机器真的没内存
	// （测试环境不可控），要么这条分支永远测不到。
	availableMemory func() (uint64, error)
}

// admitNewSession 决定现在能不能再开一个浏览器会话。
//
// 三条：没配下限就一律放行；读不到内存**不拦**（一个读不出的仪表不该变成一次停摆，
// 这是安全余量而不是授权检查）；低于下限就带 RESOURCE_EXHAUSTED 拒绝——让 Agent
// 能把它与「页面坏了」分开，前者该稍后再试，后者该换个做法。
func (r *Runtime) admitNewSession() error {
	if r.cfg.MinFreeMemoryBytes == 0 {
		return nil
	}
	read := r.availableMemory
	if read == nil {
		// mgr 为 nil 的只有 struct-literal 构造的测试，而它们会自己注入 read。
		if r.mgr == nil {
			return nil
		}
		read = r.mgr.pal.AvailableSystemMemory
	}
	available, err := read()
	if err != nil {
		r.logger().Warn("cannot read available memory before opening a browser session",
			"component", "browser",
			"error", err,
			"consequence", "the memory floor is not enforced for this session")
		return nil
	}
	if available >= r.cfg.MinFreeMemoryBytes {
		return nil
	}
	return NewBrowserError(CodeResourceExhausted, fmt.Sprintf(
		"only %d bytes of memory are available and this deployment requires %d before opening a new browser session",
		available, r.cfg.MinFreeMemoryBytes))
}

// logger 返回配置的 logger，没有配就返回一个丢弃一切的。struct-literal 构造的测试
// 走的是后者。
func (r *Runtime) logger() *slog.Logger {
	if r.cfg.Logger != nil {
		return r.cfg.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var _ RuntimeAPI = (*Runtime)(nil)

// NewRuntime 拉起底层 Manager（Chromium 进程）并返回 go-rod 运行时。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	mgr, err := NewManager(ManagerConfig{
		Headless:                cfg.Headless,
		BinPath:                 cfg.BinPath,
		AllowPrivateHosts:       cfg.AllowPrivateHosts,
		RequireSandbox:          cfg.RequireSandbox,
		Logger:                  cfg.Logger,
		MaxProcesses:            cfg.MaxProcesses,
		MaxContextsPerProcess:   cfg.MaxContextsPerProcess,
		ProcessMemoryLimitBytes: cfg.ProcessMemoryLimitBytes,
	})
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		mgr:           mgr,
		sessions:      NewSessionStore(),
		cfg:           cfg,
		hubs:          newHubRegistry(),
		screencasters: &sync.Map{},
	}
	// 快照降级：开启阈值且未显式注入 Archive 时，装配默认文件落盘（相对工具根子目录）。
	// 赋给 r.cfg（副本）而非局部 cfg，确保 observe 经 r.cfg.Archive 读到。
	if cfg.SnapshotRuneThreshold > 0 && r.cfg.Archive == nil {
		r.cfg.Archive = newFileSnapshotArchive(cfg.SnapshotArchiveDir)
	}
	// 可选持久化：装配写穿端口 + 从盘加载已存会话（Context=nil，懒重建），
	// 使进程重启后仍能识别历史会话 id（否则会误判 CONTEXT_EVICTED）。nil 时全跳过（Phase 1/2）。
	if cfg.Store != nil {
		r.sessions.SetPersist(cfg.Store)
		if recs, err := cfg.Store.List(); err != nil {
			// 加载失败不致命：退化成「历史会话未知」，但仍可服务新会话——记 Warn 便于排查。
			slog.Warn("browser: load persisted sessions failed", "err", err)
		} else {
			for _, rec := range recs {
				r.sessions.Adopt(rec)
			}
		}
	}
	// 起后台 TTL reaper：用可取消 ctx，全量 Close 时 cancel。SessionTTL<=0 时 startReaper
	// 内部直接返回（不起 goroutine），但 reaperCancel 仍登记，Close 调用对其安全。
	reaperCtx, cancel := context.WithCancel(context.Background())
	r.reaperCancel = cancel
	r.startReaper(reaperCtx)
	return r, nil
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
		return nil, nil, NewBrowserError(CodeSessionNotFound, "unknown session "+sessionID)
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

// activeURLOf 在会话锁下读会话当前地址（Open 成功导航后写入 sess.ActiveURL）。
// 供 evictSession 落盘 storageState 时带上 active_url，重启恢复时据此重新导航。
// nil 会话返回空串。
func (r *Runtime) activeURLOf(sess *Session) string {
	if sess == nil {
		return ""
	}
	var u string
	sess.WithLock(func() { u = sess.ActiveURL })
	return u
}

// touch 记录一次成功动作：在会话锁下刷新 LastUsedAt，随后（装配了持久层时）尽力把
// active_url + 最近使用时间写穿到 DB，让 reaper 按「空闲时长」而非「创建至今」判定回收，
// 从而不再误回收正在使用的会话。落盘 best-effort，失败记 Warn 不致命。
//
// 并发约定：调用方不得在已持有 sess.mu 时调用——本函数内部自锁刷新字段（若再入会死锁）。
// DB 写穿在锁外执行，只用不可变的 sess.ID 与锁内快照下来的 url/now，缩短持锁时长。
func (r *Runtime) touch(sess *Session) {
	if sess == nil {
		return
	}
	now := time.Now()
	var activeURL string
	sess.WithLock(func() {
		sess.LastUsedAt = now
		activeURL = sess.ActiveURL
	})
	if store := r.sessions.persist; store != nil {
		if err := store.Touch(sess.ID, activeURL, now); err != nil {
			slog.Warn("browser: touch persist failed", "session", sess.ID, "err", err)
		}
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
	// I4 fail-loud：只有 SessionID 为空才新建。传了非空但查无此 Session（内存无且 DB 无）
	// 说明它已被彻底回收/失效，静默 mint 一个新 Session 会掩盖状态漂移——按 CONTEXT_EVICTED
	// 报错。启动时 Adopt 已把持久会话纳入内存，故「内存有但 Context==nil」是被 TTL 回收或
	// 重启后的懒态，走下方 rebuildContext 重建（恢复登录 cookies），而非报错。
	var sess *Session
	switch {
	case req.SessionID != "":
		s, ok := r.sessions.Get(req.SessionID)
		if !ok {
			return OpenObservation{}, NewBrowserError(CodeSessionNotFound, "unknown session "+req.SessionID)
		}
		sess = s
	case req.ChatSessionID != "":
		// 复用绑定到该 chat session 的既有浏览器会话（Context 可能为 nil，下方懒重建），
		// 使同一对话内浏览器会话 id 不随每条新消息自增、人工接管态延续；无绑定则新建并绑定。
		if s, ok := r.sessions.FindByChatSession(req.ChatSessionID); ok {
			sess = s
		} else {
			if err := r.admitNewSession(); err != nil {
				return OpenObservation{}, err
			}
			sess = r.sessions.Create(req.TaskID)
			r.sessions.BindChat(sess.ID, req.ChatSessionID)
		}
	default:
		if err := r.admitNewSession(); err != nil {
			return OpenObservation{}, err
		}
		sess = r.sessions.Create(req.TaskID)
	}
	// 接管门控（在会话解析之后）：接管中的会话拒绝 Agent 的写动作——包括经 ChatSessionID
	// 复用到一个正被人工接管的会话时（read/screenshot 不受此限）。
	if r.takeoverOf(sess) {
		return OpenObservation{}, NewBrowserError(CodeTakeover, "session "+sess.ID+" under manual takeover")
	}
	// 确保有物理 Context 并完成导航：整段「Context==nil 判定 → rebuild（获取+恢复 cookies）→
	// 读 sess.Context.browser 导航」都在同一把会话锁下执行，使 Context 的判定与使用观察到一致状态，
	// 与 reaper 的 evictSession（同样在会话锁下置 Context=nil）严格串行，杜绝「判定非 nil 后被回收
	// 置 nil 再解引用」的竞态崩溃。rebuildContextLocked 在锁内经 go-rod 获取 Context 属有意为之
	// （相对 evict 串行化）。
	var obs Observation
	var opErr error
	sess.WithLock(func() {
		if sess.Context == nil {
			if err := r.rebuildContextLocked(sess); err != nil {
				opErr = err
				return
			}
		}
		if sess.Context == nil || sess.Context.browser == nil {
			opErr = NewBrowserError(CodeContextEvicted, "session "+sess.ID+" has no browser context")
			return
		}
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
		sess.ActiveURL = req.URL // 记录当前地址：TTL 回收落盘、重启恢复时用它重新导航
		obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot)
	})
	if opErr != nil {
		return OpenObservation{}, opErr
	}
	r.touch(sess) // 成功动作：刷新 LastUsedAt（+落盘），使 reaper 按空闲时长而非创建至今判定
	r.emitObservation(sess.ID, obs)
	r.emitProgress(sess.ID, "open", "done", "")
	// 换页后活跃 page 变了：若已有订阅者，把 screencast 重绑到新 page。
	r.restartScreencastIfActive(sess)
	return OpenObservation{SessionID: sess.ID, Observation: obs}, nil
}

// rebuildContextLocked 为一个 Context==nil 的会话获取新 incognito Context 并（若有持久快照）
// 恢复登录 cookies。cookies 用 SetCookies 灌到 browser（Context）级——调用方随后导航到同域
// 时浏览器即带上它们，登录态恢复。恢复失败（快照损坏/SetCookies 报错）记 Warn 不致命：
// 登录态尽力恢复，恢复不了也应给出一个可用的空白 Context，不能让整个 Open 失败。
// AcquireContext 失败则返回 error（无 Context 无法继续导航）。
//
// 并发约定：调用方（Open）须已持有 sess.mu——本函数直接写 sess.Context、读 sess.ActiveURL，
// 不再自锁（若自锁会与持锁调用方再入死锁）。在锁内经 go-rod 获取 Context 属有意为之，使重建
// 与 evict 串行。
func (r *Runtime) rebuildContextLocked(sess *Session) error {
	c, err := r.mgr.AcquireContext(ContextOpts{})
	if err != nil {
		return err
	}
	if store := r.sessions.persist; store != nil {
		rec, ok, err := store.Get(sess.ID)
		switch {
		case err != nil:
			slog.Warn("browser: rebuild load storage state failed", "session", sess.ID, "err", err)
		case ok && rec.StorageState != "":
			cookies, err := unmarshalStorageState(rec.StorageState)
			if err != nil {
				slog.Warn("browser: rebuild bad storage state", "session", sess.ID, "err", err)
			} else if err := r.restoreCookies(c.browser, cookies); err != nil {
				slog.Warn("browser: rebuild restore cookies failed", "session", sess.ID, "err", err)
			}
		}
	}
	sess.Context = c
	// MINOR：重建成功即把 DB 行的 evicted 标记清掉（TouchBrowserSession 置 evicted=0），
	// 否则重启恢复的会话被重建后 DB 仍显示 evicted=true。best-effort，失败记 Warn 不致命。
	if store := r.sessions.persist; store != nil {
		if err := store.Touch(sess.ID, sess.ActiveURL, time.Now()); err != nil {
			slog.Warn("browser: rebuild touch (clear evicted) failed", "session", sess.ID, "err", err)
		}
	}
	return nil
}

// Read 只读地重新抽取当前活跃页的 a11y 观测。
func (r *Runtime) Read(ctx context.Context, req ReadReq) (Observation, error) {
	sess, page, err := r.activePage(req.SessionID)
	if err != nil {
		return Observation{}, err
	}
	var obs Observation
	var opErr error
	sess.WithLock(func() { obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot) })
	if opErr != nil {
		return Observation{}, opErr
	}
	r.touch(sess) // 成功动作：刷新 LastUsedAt（+落盘），避免正在使用的会话被 reaper 回收
	r.emitObservation(req.SessionID, obs)
	return obs, nil
}

// Click 点击 ref 指向的元素，等待可能的导航后返回新观测。
func (r *Runtime) Click(ctx context.Context, req ClickReq) (Observation, error) {
	// 接管门控：与 Open 一致，先于 activePage（activePage 无活跃页时返回 nil sess，
	// 此时才检查会读不到接管标志），直接查会话表判断。
	if sess, ok := r.sessions.Get(req.SessionID); ok && r.takeoverOf(sess) {
		return Observation{}, NewBrowserError(CodeTakeover, "session "+req.SessionID+" under manual takeover")
	}
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
		obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	r.touch(sess) // 成功动作：刷新 LastUsedAt（+落盘），避免正在使用的会话被 reaper 回收
	r.emitProgress(req.SessionID, "click", "done", req.Ref)
	r.emitObservation(req.SessionID, obs)
	return obs, nil
}

// Type 向 ref 指向的元素输入文本；Submit 为真则输入后按回车提交。
func (r *Runtime) Type(ctx context.Context, req TypeReq) (Observation, error) {
	// 接管门控：与 Open/Click 一致，先于 activePage（activePage 无活跃页时返回 nil sess，
	// 此时才检查会读不到接管标志），直接查会话表判断。
	if sess, ok := r.sessions.Get(req.SessionID); ok && r.takeoverOf(sess) {
		return Observation{}, NewBrowserError(CodeTakeover, "session "+req.SessionID+" under manual takeover")
	}
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
		obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot)
	})
	if opErr != nil {
		return Observation{}, opErr
	}
	r.touch(sess) // 成功动作：刷新 LastUsedAt（+落盘），避免正在使用的会话被 reaper 回收
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
			// Context 的读取 + 置 nil 在会话锁下进行，与 reaper 的 evictSession 串行，
			// 避免与并发回收交错读写 Context 造成数据竞争。
			var relErr error
			sess.WithLock(func() {
				if sess.Context != nil {
					relErr = r.mgr.ReleaseContext(sess.Context)
				}
				sess.Context = nil
				sess.ActivePage = nil
				sess.takeover = false // 关闭即退接管，防标志悬挂
			})
			r.sessions.Delete(req.SessionID)
			r.stopScreencast(req.SessionID) // 停帧流，须在 drop hub 前
			r.hubs.drop(req.SessionID)
			if relErr != nil {
				return NewBrowserErrorWrap(CodeContextEvicted, "release session "+req.SessionID, relErr)
			}
			return nil
		}
		return NewBrowserError(CodeSessionNotFound, "unknown session "+req.SessionID)
	}
	// 全量关闭：先停后台 reaper（避免它在进程 Close 后仍访问已释放的 Manager）。
	if r.reaperCancel != nil {
		r.reaperCancel()
	}
	// 装配了持久层时，把仍有 live Context 的会话逐个 evict（抓 cookies → 落盘 Evicted 记录 →
	// 释放 Context），使干净关停（未经 TTL 回收）的活跃会话的登录态也能落盘，重启后可恢复；
	// 否则这些会话的 cookies 从未落盘，重启即丢登录。纯内存（persist==nil）无处落盘，跳过。
	// evictSession 自身在会话锁下执行且此时 reaper 已停，不与其它路径竞争。
	if r.sessions.persist != nil {
		for _, sess := range r.sessions.Snapshot() {
			r.evictSession(sess)
		}
	}
	r.mgr.Close()
	return nil
}

// ---- 内部 ----

// activePage 取出 Session 与其活跃 go-rod 页；缺 Session 或缺活跃页均按 CONTEXT_EVICTED 报错。
//
// 并发不变量：sess.ActivePage 的读取在会话锁下进行——reaper 的 evictSession 在同一把锁下把
// ActivePage 置 nil，故此处不会与回收交错读到撕裂状态或 nil 解引用。取到的 *rod.Page 交回后，
// 调用方（Read/Click/Type）会在自己的会话锁区间内使用它；若期间被回收，最坏是页面已关、动作
// 如实返回 error（fail-loud），而非数据竞争。
func (r *Runtime) activePage(sessionID string) (*Session, *rod.Page, error) {
	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return nil, nil, NewBrowserError(CodeSessionNotFound, "unknown session "+sessionID)
	}
	var page *rod.Page
	var err error
	sess.WithLock(func() {
		if sess.ActivePage == nil || sess.ActivePage.page == nil {
			err = NewBrowserError(CodeContextEvicted, "session "+sessionID+" has no active page")
			return
		}
		p, ok := sess.ActivePage.page.(*rod.Page)
		if !ok {
			err = NewBrowserError(CodeContextEvicted, "session "+sessionID+" active page has wrong type")
			return
		}
		page = p
	})
	if err != nil {
		return nil, nil, err
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
func (r *Runtime) observe(ctx context.Context, page *rod.Page, sess *Session, userTask, toolRoot string) (Observation, error) {
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
	return DegradeObservation(ctx, obs, userTask, toolRoot,
		DegradeDeps{Extractor: r.cfg.Extractor, Archive: r.cfg.Archive},
		r.cfg.SnapshotRuneThreshold)
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

// SetTakeover 置/清会话的人工接管标志（会话锁下）。enabled=false 恢复 Agent。
// 未知会话按 CONTEXT_EVICTED 报错（不静默成功——fail-loud）。
func (r *Runtime) SetTakeover(sessionID string, enabled bool) error {
	sess, ok := r.sessions.Get(sessionID)
	if !ok {
		return NewBrowserError(CodeSessionNotFound, "unknown session "+sessionID)
	}
	sess.WithLock(func() { sess.takeover = enabled })
	return nil
}

// takeoverOf 在会话锁下读接管标志。nil 会话视为未接管。
func (r *Runtime) takeoverOf(sess *Session) bool {
	if sess == nil {
		return false
	}
	var v bool
	sess.WithLock(func() { v = sess.takeover })
	return v
}

// InjectInput 把一批归一化输入事件注入会话活跃页（接管必须先开，否则拒）。
// 每批先整体校验，再读一次当前视口宽高，把 0..1 坐标 × px 后经 go-rod 派发。
// 与 Read/Click 等一样在会话锁下取活跃页，但注入本身在锁外执行（go-rod 调用可能阻塞，
// 不宜久持会话锁）。这不是「无并发写者」的强保证：接管期间 Agent 写动作已被门控，
// reaper 也会跳过接管中的会话（reapIdle）、Close 会清接管标志（Close 的 per-session
// 分支），常见的并发写者路径已被排除；但显式对本会话调用 Close（并发的
// Close(sessionID)）在本批次注入进行中仍可能发生——不会造成内存不安全，只会让批次里
// 下一次 go-rod 调用因 Context/页已释放而 fail-loud 报错（错误被包装上报，不会静默）。
func (r *Runtime) InjectInput(sessionID string, events []InputEvent) error {
	if err := validateInputEvents(events); err != nil {
		return NewBrowserErrorWrap(CodeInvalidInput, "invalid input batch", err)
	}
	sess, page, err := r.activePage(sessionID)
	if err != nil {
		return err
	}
	if !r.takeoverOf(sess) {
		return NewBrowserError(CodeTakeoverRequired,
			"session "+sessionID+" is not under takeover; enable takeover before injecting")
	}
	vw, vh, err := viewportSize(page)
	if err != nil {
		return NewBrowserErrorWrap(CodeContextEvicted, "read viewport for injection", err)
	}
	// 串行化本会话的注入：injectOne 顺序改写共享的 page.Mouse（坐标/按下的键），
	// 并发批次交错会打乱 press/release 顺序，令 Chrome 合成不出 click（接管点击失效）。
	// inputMu 只挡并发注入，不碰会话锁，故不阻塞 observe/reaper。
	sess.inputMu.Lock()
	defer sess.inputMu.Unlock()
	for i, ev := range events {
		if err := injectOne(page, ev, vw, vh); err != nil {
			return NewBrowserErrorWrap(CodeContextEvicted, fmt.Sprintf("inject event %d (%s)", i, ev.Type), err)
		}
	}
	r.touch(sess) // 人工也算活跃：刷新 LastUsedAt，避免接管中会话被 reaper 回收
	return nil
}

// viewport 尺寸边界：下界避免退化视口，上界是防呆上限（面板尺寸算错或恶意调用
// 不得请求超大渲染面。）
const (
	minViewportPx = 100
	maxViewportPx = 8192
)

// SetViewport 把会话视口覆盖为 width×height CSS px，使 screencast 帧的宽高比与 GUI
// 面板一致、填满面板而无 letterbox（帧面随设备度量变化，screencaster 未设
// MaxWidth/MaxHeight）。活跃页在会话锁下读取（同 InjectInput），CDP 调用在锁外执行。
// 若 screencast 正在运行则重启一次，让下一帧立即反映新尺寸。
func (r *Runtime) SetViewport(sessionID string, width, height int) error {
	if width < minViewportPx || width > maxViewportPx || height < minViewportPx || height > maxViewportPx {
		return NewBrowserError(CodeInvalidInput, fmt.Sprintf("viewport %dx%d out of range [%d,%d]", width, height, minViewportPx, maxViewportPx))
	}
	// activePage 拿一次会话锁返回活跃页（同 InjectInput）。绝不能改回 sess.WithLock(pageOf)：
	// pageOf 内部自己拿会话锁，再套一层 WithLock 会重入死锁（Go 互斥锁不可重入）。
	sess, page, err := r.activePage(sessionID)
	if err != nil {
		return err
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             width,
		Height:            height,
		DeviceScaleFactor: 1,
	}); err != nil {
		return NewBrowserErrorWrap(CodeContextEvicted, "set viewport", err)
	}
	r.restartScreencastIfActive(sess)
	return nil
}

// viewportSize 读当前视口的 CSS 像素宽高（window.innerWidth/innerHeight）。
func viewportSize(page *rod.Page) (float64, float64, error) {
	res, err := page.Eval("() => ({w: window.innerWidth, h: window.innerHeight})")
	if err != nil {
		return 0, 0, err
	}
	w := res.Value.Get("w").Num()
	h := res.Value.Get("h").Num()
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("non-positive viewport %vx%v", w, h)
	}
	return w, h, nil
}

// injectOne 把一条归一化事件 × 视口 px 后派发到 go-rod。鼠标类先 MoveTo 定位再动作。
func injectOne(page *rod.Page, ev InputEvent, vw, vh float64) (err error) {
	// 修饰键在本条事件前按下、本条事件后释放，**出错路径也释放**：一次失败的注入不
	// 该把浏览器留在 Ctrl 按住的状态里给下一位使用者。go-rod 的 Keyboard 记着当前
	// 按下的键，鼠标与键盘事件都据此算出 CDP 的 modifiers 位掩码，所以按下之后
	// ctrl+click、shift+wheel 与 ctrl+c 走的是同一条路。
	release, err := holdModifiers(page, ev.Modifiers)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := release(); rerr != nil && err == nil {
			err = rerr
		}
	}()

	px := proto.Point{X: ev.X * vw, Y: ev.Y * vh}
	switch ev.Type {
	case "mousemove":
		return page.Mouse.MoveTo(px)
	case "mousedown":
		if err := page.Mouse.MoveTo(px); err != nil {
			return err
		}
		return page.Mouse.Down(mouseButton(ev.Button), 1)
	case "mouseup":
		if err := page.Mouse.MoveTo(px); err != nil {
			return err
		}
		return page.Mouse.Up(mouseButton(ev.Button), 1)
	case "click":
		if err := page.Mouse.MoveTo(px); err != nil {
			return err
		}
		return page.Mouse.Click(mouseButton(ev.Button), 1)
	case "wheel":
		if err := page.Mouse.MoveTo(px); err != nil {
			return err
		}
		return page.Mouse.Scroll(ev.DeltaX, ev.DeltaY, 1)
	case "keydown":
		k, err := keyToInputKey(ev.Key)
		if err != nil {
			return err
		}
		return page.Keyboard.Press(k)
	case "keyup":
		k, err := keyToInputKey(ev.Key)
		if err != nil {
			return err
		}
		return page.Keyboard.Release(k)
	case "char":
		return page.InsertText(ev.Text)
	default:
		// validateInputEvents 已挡掉未知类型；到这里属编程错误。
		return fmt.Errorf("unhandled input type %q", ev.Type)
	}
}

// holdModifiers 按下这条事件声明的修饰键，返回释放它们的函数。
//
// 释放按相反顺序，且**每一个都试着释放**：其中一个失败不该让其余的留在按下状态，
// 那正是「键盘坏了」的来源。返回的是第一个错误，其余记不了也不吞——它们本就是同一
// 次失败的不同表现。
//
// 名字在这里必定合法（validateModifiers 在整批注入之前就跑过），所以解析失败属编程
// 错误，照样返回而不是猜一个键。
func holdModifiers(page *rod.Page, names []string) (func() error, error) {
	if len(names) == 0 {
		return func() error { return nil }, nil
	}
	held := make([]input.Key, 0, len(names))
	release := func() error {
		var firstErr error
		for i := len(held) - 1; i >= 0; i-- {
			if err := page.Keyboard.Release(held[i]); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("release modifier: %w", err)
			}
		}
		held = held[:0]
		return firstErr
	}
	for _, name := range names {
		k, err := modifierToInputKey(name)
		if err != nil {
			_ = release()
			return nil, err
		}
		if err := page.Keyboard.Press(k); err != nil {
			_ = release()
			return nil, fmt.Errorf("press modifier %q: %w", name, err)
		}
		held = append(held, k)
	}
	return release, nil
}

// mouseButton 把事件 button 名映射到 go-rod 常量；空/未知回落 left（validate 已挡未知非空值）。
func mouseButton(name string) proto.InputMouseButton {
	switch name {
	case "right":
		return proto.InputMouseButtonRight
	case "middle":
		return proto.InputMouseButtonMiddle
	default:
		return proto.InputMouseButtonLeft
	}
}
