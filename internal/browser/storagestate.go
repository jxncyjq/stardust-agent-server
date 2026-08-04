package browser

import (
	"encoding/json"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// CookieState 是一条持久化 cookie（go-rod NetworkCookie 的可序列化子集）。
// 只保留还原登录态所需字段；实验性/派生字段（Size/Session/Priority/Partition 等）
// 不落盘——重建时由浏览器按域重新计算。
type CookieState struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

// marshalStorageState 把 cookies 序列化成 JSON 字符串（落 SQLite storage_state 列）。
// 空 cookies 返回空串（而非 "null"/"[]"），与 unmarshalStorageState 的空串语义对称。
func marshalStorageState(cookies []CookieState) (string, error) {
	if len(cookies) == 0 {
		return "", nil
	}
	b, err := json.Marshal(cookies)
	if err != nil {
		return "", NewBrowserErrorWrap(CodeContextEvicted, "marshal storage state", err)
	}
	return string(b), nil
}

// unmarshalStorageState 把持久化的 JSON 还原成 cookies。空串表示无快照，返回 nil。
func unmarshalStorageState(js string) ([]CookieState, error) {
	if js == "" {
		return nil, nil
	}
	var cookies []CookieState
	if err := json.Unmarshal([]byte(js), &cookies); err != nil {
		return nil, NewBrowserErrorWrap(CodeContextEvicted, "unmarshal storage state", err)
	}
	return cookies, nil
}

// captureCookies 从会话的 incognito browser 抓当前 cookies，转成可序列化的 CookieState。
// 无 Context/browser（已回收）返回 (nil, nil)——抓不到不是错误，调用方按空快照处理。
//
// 并发约定：读 sess.Context/browser，调用方须已持有 sess.mu（生产路径 evictSession 在锁下调用）；
// 本函数自身不加锁，避免与持锁调用方再入死锁。
func (r *Runtime) captureCookies(sess *Session) ([]CookieState, error) {
	if sess == nil || sess.Context == nil || sess.Context.browser == nil {
		return nil, nil
	}
	raw, err := sess.Context.browser.GetCookies()
	if err != nil {
		return nil, NewBrowserErrorWrap(CodeContextEvicted, "get cookies", err)
	}
	out := make([]CookieState, 0, len(raw))
	for _, c := range raw {
		out = append(out, CookieState{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Expires:  float64(c.Expires),
			HTTPOnly: c.HTTPOnly,
			Secure:   c.Secure,
			SameSite: string(c.SameSite),
		})
	}
	return out, nil
}

// restoreCookies 把持久化 cookies 注入一个新 incognito browser（重建 Context 时用）。
// cookies 为空或 browser 为 nil 时是无操作（返回 nil）。
func (r *Runtime) restoreCookies(ctxBrowser *rod.Browser, cookies []CookieState) error {
	if ctxBrowser == nil || len(cookies) == 0 {
		return nil
	}
	params := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		params = append(params, &proto.NetworkCookieParam{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Expires:  proto.TimeSinceEpoch(c.Expires),
			HTTPOnly: c.HTTPOnly,
			Secure:   c.Secure,
			SameSite: proto.NetworkCookieSameSite(c.SameSite),
		})
	}
	if err := ctxBrowser.SetCookies(params); err != nil {
		return NewBrowserErrorWrap(CodeContextEvicted, "set cookies", err)
	}
	return nil
}
