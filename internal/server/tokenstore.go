package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
)

// TokenStore 持有当前有效的 bearer token，并在它被换掉时**通知所有在场的长连接**。
//
// 它存在是因为一条 SSE 只在建立时鉴权一次，然后可以挂几个小时。没有这个通知，一个
// 泄露出去的 token 被吊销之后，用它开着的那条流仍然在继续接收事件——泄露一次凭证
// 等于泄露从此以后的全部事件流，而运维手上除了重启进程没有别的办法，重启会把正在
// 跑的任务一起打断。
//
// 零值不可用：用 NewTokenStore 构造。nil 的 *TokenStore 是「这个部署没有 token」，
// 所有方法对它安全（见各方法）——那是本机开放 serve 的正常形态，不是错误。
type TokenStore struct {
	mu      sync.RWMutex
	current string
	// changed 在每次轮换时被关闭并换新。关闭一个 channel 是**广播**：所有在 select
	// 它的流同时醒来，不需要注册表，也不会漏掉任何一条。
	changed chan struct{}
}

// NewTokenStore 以 token 为当前凭证建一个存储。空 token 表示这个部署不要求鉴权，
// 此时轮换是没有意义的（RotateAllowed 报 false）。
func NewTokenStore(token string) *TokenStore {
	return &TokenStore{current: token, changed: make(chan struct{})}
}

// Current 返回当前 token。nil store 返回空串。
func (s *TokenStore) Current() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Valid 判断一个 token 是不是当前那个。
//
// 用 subtle.ConstantTimeCompare 而不是 ==：这是一个可以被无限次尝试的本机端点，
// 而字符串比较在第一个不同的字节就返回，逐字节的时间差足以把一个 token 猜出来。
func (s *TokenStore) Valid(token string) bool {
	if s == nil {
		return false
	}
	current := s.Current()
	if current == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(current)) == 1
}

// RotateAllowed 表示这个部署有没有可轮换的凭证。
func (s *TokenStore) RotateAllowed() bool { return s != nil && s.Current() != "" }

// Rotate 铸一个新 token、让旧的立刻失效，并广播这次变更，返回新 token。
//
// 铸新而不是单纯清空，是因为吊销的动机通常是「这把钥匙掉了」而不是「这扇门不要了」：
// 清空会把服务变成无鉴权，比泄露更糟。
func (s *TokenStore) Rotate() string {
	next, err := GenerateLoopbackToken()
	if err != nil {
		// 熵取不到是不可恢复的：此时唯一安全的动作是让旧 token 立刻失效，而不是
		// 继续用它。调用方（HTTP handler）把这条错误报给运维。
		panic(fmt.Sprintf("generate a replacement token: %v", err))
	}
	s.Replace(next)
	return next
}

// Replace 把当前 token 换成 next 并广播。next 为空即「此后不再接受任何 token」，
// 这不是一个 handler 会做的事，但装配期（换成部署配置的 AdminToken）会用到。
func (s *TokenStore) Replace(next string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	previous := s.changed
	s.current = next
	s.changed = make(chan struct{})
	s.mu.Unlock()
	// 在锁外关闭：被唤醒的流会立刻去读 Current()，在锁内关闭等于让它们排在自己
	// 的唤醒者后面。
	close(previous)
}

// Changed 返回一个「当前 token 被换掉时关闭」的 channel。
//
// 每次调用拿到的是**当下这一代**的 channel：拿到之后发生的轮换会关闭它，而再次
// 调用会拿到下一代。长连接应当在进入循环前取一次，然后 select 它。
func (s *TokenStore) Changed() <-chan struct{} {
	if s == nil {
		// 一个永不关闭的 channel：没有 token 的部署里没有任何东西会被吊销，
		// select 它就是永远不触发的那个分支。
		return make(chan struct{})
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

// writeReauth 在一条 SSE 上写下「你的凭证已经失效，拿新的重连」。
//
// 它是一个**事件**而不是直接断开，因为断开在客户端看来与网络抖动没有区别：客户端
// 会以为是掉线，然后带着那个已经作废的 token 无限重试。收到 reauth 的客户端知道
// 该去取新凭证，也知道这不是它的网络的问题。
func writeReauth(w http.ResponseWriter) {
	_, _ = fmt.Fprintf(w, "event: reauth\ndata: {\"reason\":\"the credential this stream was opened with has been revoked\"}\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
