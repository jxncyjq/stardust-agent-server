package browser

import "context"

// OpenReq 打开页面的请求，含新建/复用的 session 与目标 URL。
type OpenReq struct {
	URL       string
	SessionID string // 空则按 ChatSessionID 复用或新建 Session
	TaskID    string // 新建时绑定
	// ChatSessionID 是发起本次 Open 的 chat/对话 session id。SessionID 为空时，Runtime
	// 优先复用绑定到该 chat session 的既有浏览器会话（无则新建并绑定），使浏览器会话 id
	// 不随同一对话内的每条新消息自增、人工接管状态跨消息存活。空则退回「每次新建」旧行为。
	ChatSessionID string
	UserTask      string // 当前 agent 任务文本，供超阈快照按任务抽取；空则跳过抽取
	ToolRoot      string // 与 read_file 同源的工具根，落盘全文使其可被翻页
}

type ReadReq struct {
	SessionID string
	Mode      string // 空=默认 a11y 树；本 Phase 只实现 a11y
	UserTask  string // 当前 agent 任务文本，供超阈快照按任务抽取；空则跳过抽取
	ToolRoot  string // 与 read_file 同源的工具根，落盘全文使其可被翻页
}

type ClickReq struct {
	SessionID string
	Ref       string
	UserTask  string // 当前 agent 任务文本，供超阈快照按任务抽取；空则跳过抽取
	ToolRoot  string // 与 read_file 同源的工具根，落盘全文使其可被翻页
}

type TypeReq struct {
	SessionID string
	Ref       string
	Text      string
	Submit    bool   // true 则输入后回车提交
	UserTask  string // 当前 agent 任务文本，供超阈快照按任务抽取；空则跳过抽取
	ToolRoot  string // 与 read_file 同源的工具根，落盘全文使其可被翻页
}

type CloseReq struct {
	SessionID string
}

// OpenObservation 在 Observation 之外带上 session id（open 可能新建会话）。
type OpenObservation struct {
	SessionID   string
	Observation Observation
}

// RuntimeAPI 是后端核心对外唯一契约（spec §3.1）。工具层与传输适配器都封装它。
// 本 Phase 只含最小闭环方法；后续 Phase 追加 Scroll/Back/Screenshot/Extract/Download。
type RuntimeAPI interface {
	Open(ctx context.Context, req OpenReq) (OpenObservation, error)
	Read(ctx context.Context, req ReadReq) (Observation, error)
	Click(ctx context.Context, req ClickReq) (Observation, error)
	Type(ctx context.Context, req TypeReq) (Observation, error)
	Close(ctx context.Context, req CloseReq) error

	// Subscribe 返回一个会话的流事件通道与取消函数。用于前端观看 Agent 浏览过程。
	// 未知会话返回错误。
	Subscribe(sessionID string) (<-chan StreamEvent, func(), error)
}
