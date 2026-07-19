package gateway

import (
	"context"
	"fmt"
)

// DeliveryRouter sends an outbound reply through the adapter for its target
// platform, splitting over-length text into platform-sized chunks.
type DeliveryRouter struct {
	adapters map[string]ChannelAdapter
	maxLen   map[string]int
}

// NewDeliveryRouter returns an empty router.
func NewDeliveryRouter() *DeliveryRouter {
	return &DeliveryRouter{
		adapters: make(map[string]ChannelAdapter),
		maxLen:   make(map[string]int),
	}
}

// RegisterAdapter binds an adapter and its max message length to its platform.
// maxMessageLength <= 0 disables splitting for that platform.
func (r *DeliveryRouter) RegisterAdapter(a ChannelAdapter, maxMessageLength int) {
	r.adapters[a.Platform()] = a
	r.maxLen[a.Platform()] = maxMessageLength
}

// Route delivers text to target. An unknown platform is a loud error. Over-length
// text is split; the first Send error aborts and is returned wrapped.
func (r *DeliveryRouter) Route(ctx context.Context, target DeliveryTarget, text string) error {
	adapter, ok := r.adapters[target.Platform]
	if !ok {
		return fmt.Errorf("route delivery: platform %q has no adapter", target.Platform)
	}
	for _, chunk := range splitMessage(text, r.maxLen[target.Platform]) {
		if err := adapter.Send(ctx, target, chunk); err != nil {
			return fmt.Errorf("route delivery to %s:%s: %w", target.Platform, HashID(target.ChatID), err)
		}
	}
	return nil
}

// splitMessage breaks text into chunks no longer than max runes, preferring to
// break at the last newline within a chunk so replies are not cut mid-line. max
// <= 0 returns text as a single chunk. Empty text yields no chunks.
func splitMessage(text string, max int) []string {
	if text == "" {
		return nil
	}
	if max <= 0 {
		return []string{text}
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		end := min(max, len(runes))
		if end < len(runes) {
			// Prefer a newline break within [0, end).
			for i := end - 1; i > 0; i-- {
				if runes[i] == '\n' {
					end = i
					break
				}
			}
		}
		chunk := string(runes[:end])
		chunks = append(chunks, chunk)
		// Skip a single boundary newline so it is not re-emitted as a blank chunk.
		if end < len(runes) && runes[end] == '\n' {
			end++
		}
		runes = runes[end:]
	}
	return chunks
}
