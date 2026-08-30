package browser

import "fmt"

// Code 是回给 Agent 的可自恢复语义错误码（见 spec §5.1）。
type Code string

const (
	CodeElementNotFound    Code = "ELEMENT_NOT_FOUND"      // ref 失效，建议重新 read
	CodeNavigationTimeout  Code = "NAVIGATION_TIMEOUT"     // 导航超时，建议重试或换策略
	CodeBlockedByCaptcha   Code = "BLOCKED_BY_CAPTCHA"     // 遇验证码
	CodeDownloadTooLarge   Code = "DOWNLOAD_TOO_LARGE"     // 超文件上限
	CodeContextEvicted     Code = "CONTEXT_EVICTED"        // Context 被回收，需重建 Session
	CodeProtocolBlocked    Code = "PROTOCOL_BLOCKED"       // 危险协议被拦（file://、chrome://、data:）
	CodePrivateHostBlocked Code = "PRIVATE_HOST_BLOCKED"   // 私网地址被 SSRF 拦截
	CodeTakeover           Code = "SESSION_UNDER_TAKEOVER" // 会话被人工接管，Agent 写动作暂挡

	// CodeInvalidInput 是「这次**请求**写错了」，与页面上有什么无关：空批次、
	// 越界坐标、认不出的键名、越界视口。
	//
	// 它存在是因为这些错误此前一律回 ELEMENT_NOT_FOUND，也就是在建议调用方
	// 「重新 read 一次页面」——照做是白做，页面没有任何问题。重试同一个请求也
	// 永远不会成功，改请求才会。
	CodeInvalidInput Code = "INVALID_INPUT"

	// CodeSessionNotFound 是「这个 id 没有对应的会话」，与 CodeContextEvicted
	// 分开：后者的意思是「你**曾经**有的那个会话，它的浏览器上下文被回收了，
	// 重建一个」，对一个从未存在过的 id 说这句话是在编造一段历史。两者的补救
	// 动作恰好相同，但一个是「你打错了」，另一个是「它超时了」，排查方向相反。
	CodeSessionNotFound Code = "SESSION_NOT_FOUND"

	// CodeResourceExhausted 是「这台机器现在开不下一个新的浏览器会话」。
	//
	// 与「页面坏了」分开是因为补救动作完全不同：Agent 收到它应当稍后再试或改用
	// 不开浏览器的办法，而不是重读页面或换一个 URL。
	CodeResourceExhausted Code = "RESOURCE_EXHAUSTED"

	// CodeTakeoverRequired 是「这个会话**没有**在接管，先进接管再注入」。
	//
	// 它与 CodeTakeover 互为反面，而此前两种状态共用后者一个码：注入被拒时回的
	// 是 SESSION_UNDER_TAKEOVER——字面意思正是它拒绝的理由的反面，只能靠后面的
	// 散文分辨。
	CodeTakeoverRequired Code = "TAKEOVER_REQUIRED"
)

// BrowserError 携带语义码，供工具层映射到 domain.ToolResult.Error。
// Err 为可选的底层原因；非空时保留错误链（errors.Is/As 可穿透），符合 fail-loud。
type BrowserError struct {
	Code Code
	Msg  string
	Err  error // 底层原因，可为 nil
}

func (e *BrowserError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// Unwrap 暴露底层原因，使 errors.Is/As 能穿透语义码错误。
func (e *BrowserError) Unwrap() error { return e.Err }

// NewBrowserError 构造一个带语义码的错误。
func NewBrowserError(code Code, msg string) *BrowserError {
	return &BrowserError{Code: code, Msg: msg}
}

// NewBrowserErrorWrap 构造一个带语义码且包住底层原因的错误（等价于 %w 包装），
// 供 fail-loud 场景保留完整错误链。
func NewBrowserErrorWrap(code Code, msg string, err error) *BrowserError {
	return &BrowserError{Code: code, Msg: msg, Err: err}
}
