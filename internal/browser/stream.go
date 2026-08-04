package browser

import "sync"

// EventType 是流事件的三类（spec §4.3）。
type EventType string

const (
	EventObservation EventType = "observation" // 观测更新（JSON）
	EventFrame       EventType = "frame"       // 截图帧（base64 JPEG）
	EventProgress    EventType = "progress"    // 动作进度/状态
)

// isStatus 报告该类型是否为「不可丢」的状态事件（进缓冲、可 replay）。frame 可丢。
func (t EventType) isStatus() bool { return t == EventObservation || t == EventProgress }

// StreamEvent 是推给前端的一条事件。Seq 由 Hub 分配，全会话单调。
type StreamEvent struct {
	Type EventType `json:"type"`
	Seq  uint64    `json:"seq"`
	Data any       `json:"data,omitempty"`
}

// Hub 是一个浏览器会话的事件广播器：向所有订阅者扇出，分配单调 seq，
// 并把最近 N 条 status 事件留在环形缓冲里供 Last-Event-ID 重连补发。
// frame 不入缓冲（可丢）。并发安全。
type Hub struct {
	mu      sync.Mutex
	seq     uint64
	subs    map[int]chan StreamEvent
	nextSub int
	ring    []StreamEvent // 只存 status
	ringCap int
}

// NewHub 建一个 status 环形缓冲容量为 ringCap 的 Hub。
func NewHub(ringCap int) *Hub {
	if ringCap <= 0 {
		ringCap = 64
	}
	return &Hub{subs: make(map[int]chan StreamEvent), ringCap: ringCap}
}

// Subscribe 返回一个新订阅通道与取消函数。通道有缓冲，满则丢最旧帧级事件由发送方决定——
// 这里用带缓冲通道 + 非阻塞发送（Publish 里），慢订阅者不拖垮广播。
func (h *Hub) Subscribe() (<-chan StreamEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextSub
	h.nextSub++
	ch := make(chan StreamEvent, 64)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

// Publish 分配 seq、缓冲 status、非阻塞扇出给所有订阅者。返回分配的 seq。
func (h *Hub) Publish(ev StreamEvent) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	if ev.Type.isStatus() {
		h.ring = append(h.ring, ev)
		if len(h.ring) > h.ringCap {
			h.ring = h.ring[len(h.ring)-h.ringCap:]
		}
	}
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default: // 慢订阅者：丢这条给它（帧可丢；status 它可靠 ReplaySince 补）
		}
	}
	return h.seq
}

// ReplaySince 返回缓冲里 seq>lastID 的 status 事件，供重连补发。
func (h *Hub) ReplaySince(lastID uint64) []StreamEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []StreamEvent
	for _, ev := range h.ring {
		if ev.Seq > lastID {
			out = append(out, ev)
		}
	}
	return out
}

// SubscriberCount 返回当前订阅者数（screencast 按它开关）。
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
