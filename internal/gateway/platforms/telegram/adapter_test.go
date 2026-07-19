package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/gateway"
)

func TestTelegramStartReceivesUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"chat":{"id":42},"from":{"id":7},"text":"hi"}}]}`))
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	adapter := newForTest("tok", server.URL, 0)
	ctx := t.Context()
	inbound := make(chan gateway.InboundMessage, 1)
	go func() { _ = adapter.Start(ctx, inbound) }()

	select {
	case msg := <-inbound:
		if msg.Platform != "telegram" || msg.ChatID != "42" || msg.Text != "hi" {
			t.Fatalf("inbound = %+v, want telegram/42/hi", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no inbound message received")
	}
}

func TestTelegramSendPostsMessage(t *testing.T) {
	var gotChatID, gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendMessage") {
			var body struct {
				ChatID any    `json:"chat_id"`
				Text   string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotChatID, gotText = toStr(body.ChatID), body.Text
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	adapter := newForTest("tok", server.URL, 0)
	if err := adapter.Send(context.Background(), gateway.DeliveryTarget{Platform: "telegram", ChatID: "42"}, "hello"); err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if gotChatID != "42" || gotText != "hello" {
		t.Fatalf("sendMessage got chat=%q text=%q, want 42/hello", gotChatID, gotText)
	}
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), "0"), ".")
	default:
		return ""
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
