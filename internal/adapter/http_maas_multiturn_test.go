package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// A multi-turn request must reach the provider as separate turns: the assistant
// turn carrying the tool call it made, and a tool turn paired to it by id. This
// is what lets the model see that it already read the file — the absence of it
// is what let one task read the same file 152 times.
func TestGenerateSendsMultiTurnMessages(t *testing.T) {
	t.Parallel()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Decode(request body) error = %v, want nil", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`)); err != nil {
			t.Errorf("Write(response) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: server.URL, Model: "deepseek-chat", Client: server.Client()})

	_, err := client.Generate(context.Background(), port.InferenceRequest{
		Messages: []port.InferenceMessage{
			{Role: port.RoleUser, Content: "read hello.txt"},
			{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: map[string]string{"path": "hello.txt"}},
			}},
			{Role: port.RoleTool, ToolCallID: "call-1", Content: "hello agent"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v, want nil", err)
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("request messages = %v, want 3 turns", body["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("second turn role = %v, want assistant", assistant["role"])
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant turn tool_calls = %v, want 1", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call-1" || call["type"] != "function" {
		t.Fatalf("tool call id/type = %v/%v, want call-1/function", call["id"], call["type"])
	}
	fn, _ := call["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if fn["name"] != "read_file" || !strings.Contains(args, "hello.txt") {
		t.Fatalf("tool call function = %v, want read_file with hello.txt in arguments", fn)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call-1" {
		t.Fatalf("third turn = %v, want a tool message paired to call-1", toolMsg)
	}
	if toolMsg["content"] != "hello agent" {
		t.Fatalf("tool message content = %v, want the tool output", toolMsg["content"])
	}
}

// Single-turn requests keep their exact previous body: one user message whose
// content is the plain prompt string.
func TestGenerateKeepsSingleTurnBodyUnchanged(t *testing.T) {
	t.Parallel()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Decode(request body) error = %v, want nil", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`)); err != nil {
			t.Errorf("Write(response) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: server.URL, Model: "deepseek-chat", Client: server.Client()})

	if _, err := client.Generate(context.Background(), port.InferenceRequest{Prompt: "just ask"}); err != nil {
		t.Fatalf("Generate() error = %v, want nil", err)
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("request messages = %v, want 1 turn", body["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "just ask" {
		t.Fatalf("single-turn message = %v, want a plain user prompt", msg)
	}
}

// The adapter is the last place that can catch an ambiguous request before it
// becomes an answer to the wrong question, so it validates rather than picking
// one field.
func TestGenerateRejectsPromptAndMessagesTogether(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Generate() sent a request, want rejection before any HTTP call")
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: server.URL, Model: "deepseek-chat", Client: server.Client()})

	_, err := client.Generate(context.Background(), port.InferenceRequest{
		Prompt:   "hi",
		Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "hi"}},
	})

	if err == nil {
		t.Fatal("Generate() error = nil, want a validation error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Generate() error = %v, want it to name the conflict", err)
	}
}
