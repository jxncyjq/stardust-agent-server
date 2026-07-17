// Package gateway is Legion's IM multi-channel gateway framework: a
// platform-agnostic adapter model plus routing, session binding, and a core
// API/SSE client, so messaging platforms can be bridged to the agent runtime
// without changing the core.
package gateway

import (
	"context"
	"time"
)

// InboundMessage is a platform-agnostic incoming message pushed by an adapter.
type InboundMessage struct {
	Platform   string
	ChatID     string
	UserID     string
	Text       string
	Images     []string
	ReceivedAt time.Time
}

// DeliveryTarget addresses an outbound reply. Its string form mirrors hermes:
// "<platform>:<chatID>[:<thread>]".
type DeliveryTarget struct {
	Platform string
	ChatID   string
	Thread   string
}

// PlatformConfig is the per-platform runtime configuration handed to an adapter
// factory. Token is the platform credential; PollTimeoutSeconds bounds a long
// poll where applicable.
type PlatformConfig struct {
	Token              string
	PollTimeoutSeconds int
}

// ChannelAdapter is one messaging-platform integration. It is self-driven: Start
// runs its own ingress loop until ctx is cancelled, pushing InboundMessage to
// inbound; Send delivers a reply; Close releases resources.
type ChannelAdapter interface {
	Platform() string
	Start(ctx context.Context, inbound chan<- InboundMessage) error
	Send(ctx context.Context, target DeliveryTarget, text string) error
	Close() error
}
