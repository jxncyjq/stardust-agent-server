package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateLoopbackTokenUnique(t *testing.T) {
	a, err := GenerateLoopbackToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	b, err := GenerateLoopbackToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if a == "" || len(a) < 32 {
		t.Fatalf("token too short: %q", a)
	}
	if a == b {
		t.Fatal("tokens not unique across calls")
	}
}

func TestHandshakeJSON(t *testing.T) {
	h := Handshake{BaseURL: "http://127.0.0.1:54321", Token: "abc"}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"baseURL"`) || !strings.Contains(string(b), `"token"`) {
		t.Fatalf("handshake json shape: %s", b)
	}
}
