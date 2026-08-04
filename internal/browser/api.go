package browser

import "context"

// OpenReq 打开页面的请求，含新建/复用的 session 与目标 URL。
type OpenReq struct {
	URL       string
	SessionID string // 空则新建 Session
	TaskID    string // 新建时绑定
}

type ReadReq struct {
	SessionID string
	Mode      string // 空=默认 a11y 树；本 Phase 只实现 a11y
}

type ClickReq struct {
	SessionID string
	Ref       string
}

type TypeReq struct {
	SessionID string
	Ref       string
	Text      string
	Submit    bool // true 则输入后回车提交
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
