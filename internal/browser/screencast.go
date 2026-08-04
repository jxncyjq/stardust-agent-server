package browser

import (
	"context"
	"encoding/base64"
	"log/slog"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// screencaster 把一个 page 的 CDP screencast 帧节流后发到会话 Hub。
// 仅在有订阅者时运行；Stop 后可再次 Start（换页时）。并发安全。
type screencaster struct {
	mu          sync.Mutex
	running     bool
	stop        func()
	minInterval time.Duration // 限帧率：两帧之间最小间隔
	lastSent    time.Time
	sessionID   string // 仅用于日志定位（fail-loud 记 Warn 时带上会话）
}

// newScreencaster 建一个节流到 fps 帧率的 screencaster；fps<=0 时回落到默认 8fps。
// sessionID 仅用于 fail-loud 日志定位。
func newScreencaster(fps int, sessionID string) *screencaster {
	if fps <= 0 {
		fps = 8
	}
	return &screencaster{minInterval: time.Second / time.Duration(fps), sessionID: sessionID}
}

// Start 在 page 上开 CDP screencast，把节流后的帧作为 EventFrame 发到 hub。
// 已在运行则幂等返回 nil。start 失败按 CONTEXT_EVICTED 包装上报（fail-loud）。
//
// go-rod 的 page.EachEvent(cb)() 返回的 wait 函数才是真正消费事件的循环体：
// 它 for-range 事件通道，直到某回调返回 true 或所在 page 的 context 被取消。
// 因此这里在独立 goroutine 里跑 wait，并用一个可取消的派生 context 作为停止开关——
// 取消它会关掉事件通道、令 goroutine 退出（绝不能把 wait 当作同步 stop 调用，
// 那会永远阻塞等一个永不返回 true 的回调）。
func (s *screencaster) Start(page *rod.Page, hub *Hub) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if err := (proto.PageStartScreencast{
		Format:        proto.PageStartScreencastFormatJpeg,
		Quality:       gInt(60),
		EveryNthFrame: gInt(2),
	}).Call(page); err != nil {
		return NewBrowserErrorWrap(CodeContextEvicted, "start screencast", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	evPage := page.Context(ctx) // 帧消费循环绑到可取消 ctx；取消即停
	wait := evPage.EachEvent(func(e *proto.PageScreencastFrame) {
		// 先 ack，让 CDP 继续推下一帧（不 ack 会卡住流）。ack 走原始 page（ctx 未取消）。
		// ack 失败不致命（下一帧的 ack 或换页重启可恢复），但按 fail-loud 铁律不吞错，记 Warn。
		if err := (proto.PageScreencastFrameAck{SessionID: e.SessionID}).Call(page); err != nil {
			slog.Warn("browser: screencast frame ack failed", "session", s.sessionID, "err", err)
		}
		s.mu.Lock()
		now := time.Now()
		if now.Sub(s.lastSent) < s.minInterval {
			s.mu.Unlock()
			return // 丢帧限速：帧可丢（spec §3.2）
		}
		s.lastSent = now
		s.mu.Unlock()
		// PageScreencastFrame.Data 是 []byte（原始压缩图字节），base64 编码后塞进 SSE。
		hub.Publish(StreamEvent{Type: EventFrame, Data: map[string]any{
			"mime": "image/jpeg",
			"b64":  base64.StdEncoding.EncodeToString(e.Data),
		}})
	})
	go wait() // 后台消费帧事件，直到 ctx 取消

	s.stop = func() {
		_ = proto.PageStopScreencast{}.Call(page) // 先停止产帧（原始 page，ctx 仍活）
		cancel()                                  // 关闭事件通道 → wait goroutine 退出
	}
	s.running = true
	return nil
}

// Stop 停止 screencast 并解绑事件消费；未运行则幂等返回。
func (s *screencaster) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	if s.stop != nil {
		s.stop()
	}
	s.stop = nil
	s.running = false
}

// gInt 是 go-rod proto 里 *int 可选字段（Quality/EveryNthFrame）的便捷构造。
func gInt(v int) *int { return &v }
