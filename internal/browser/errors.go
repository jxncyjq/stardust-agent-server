package browser

import "fmt"

// Code 是回给 Agent 的可自恢复语义错误码（见 spec §5.1）。
type Code string

const (
	CodeElementNotFound   Code = "ELEMENT_NOT_FOUND"   // ref 失效，建议重新 read
	CodeNavigationTimeout Code = "NAVIGATION_TIMEOUT"  // 导航超时，建议重试或换策略
	CodeBlockedByCaptcha  Code = "BLOCKED_BY_CAPTCHA"  // 遇验证码
	CodeDownloadTooLarge  Code = "DOWNLOAD_TOO_LARGE"  // 超文件上限
	CodeContextEvicted    Code = "CONTEXT_EVICTED"     // Context 被回收，需重建 Session
	CodeProtocolBlocked   Code = "PROTOCOL_BLOCKED"    // 危险协议被拦（file://、chrome://、data:）
	CodePrivateHostBlocked Code = "PRIVATE_HOST_BLOCKED" // 私网地址被 SSRF 拦截
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
