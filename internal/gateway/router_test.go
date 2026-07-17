package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingAdapter struct {
	name string
	sent []string
	err  error
}

func (a *recordingAdapter) Platform() string                                   { return a.name }
func (a *recordingAdapter) Start(context.Context, chan<- InboundMessage) error { return nil }
func (a *recordingAdapter) Close() error                                       { return nil }
func (a *recordingAdapter) Send(_ context.Context, _ DeliveryTarget, text string) error {
	if a.err != nil {
		return a.err
	}
	a.sent = append(a.sent, text)
	return nil
}

func TestDeliveryRouterSplitsAndRoutes(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram"}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 5) // tiny max to force splitting

	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "abcdefghij"); err != nil {
		t.Fatalf("Route() error = %v, want nil", err)
	}
	if len(adapter.sent) != 2 || adapter.sent[0] != "abcde" || adapter.sent[1] != "fghij" {
		t.Fatalf("sent = %v, want two 5-char chunks", adapter.sent)
	}
}

func TestDeliveryRouterUnknownPlatformFailsLoud(t *testing.T) {
	router := NewDeliveryRouter()
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "discord", ChatID: "1"}, "hi"); err == nil {
		t.Fatalf("Route(unknown) error = nil, want non-nil")
	}
}

func TestDeliveryRouterPropagatesSendError(t *testing.T) {
	adapter := &recordingAdapter{name: "telegram", err: errors.New("boom")}
	router := NewDeliveryRouter()
	router.RegisterAdapter(adapter, 4096)
	if err := router.Route(context.Background(), DeliveryTarget{Platform: "telegram", ChatID: "1"}, "hi"); err == nil {
		t.Fatalf("Route(send error) error = nil, want propagated")
	}
}

func TestSplitMessagePrefersNewline(t *testing.T) {
	chunks := splitMessage("aaa\nbbbbb", 5)
	if len(chunks) != 2 || chunks[0] != "aaa" || chunks[1] != "bbbbb" {
		t.Fatalf("splitMessage = %v, want [aaa bbbbb] (split at newline)", chunks)
	}
	if strings.Join(splitMessage("", 5), "") != "" {
		t.Fatalf("splitMessage(empty) non-empty")
	}
}
