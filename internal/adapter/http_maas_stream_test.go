package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

// streamServer replays a fixed SSE chunk sequence as a chat-completions stream.
func streamServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("request stream = %v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range lines {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestGenerateOpenAIChatStreamStreamsTextAndUsage(t *testing.T) {
	srv := streamServer(t, []string{
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":" world"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	})
	t.Cleanup(srv.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-chat", Client: srv.Client()})

	var deltas []string
	resp, err := client.generateOpenAIChatStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "hi"}}},
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("generateOpenAIChatStream error = %v, want nil", err)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Fatalf("deltas = %v, want [Hello, ` world`]", deltas)
	}
	if resp.Text != "Hello world" {
		t.Fatalf("resp.Text = %q, want the full accumulated text", resp.Text)
	}
	if resp.PromptTokens != 10 || resp.CompletionTokens != 2 || resp.TotalTokens != 12 {
		t.Fatalf("usage = %d/%d/%d, want 10/2/12", resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens)
	}
}

func TestGenerateOpenAIChatStreamAssemblesToolCallsFromDeltas(t *testing.T) {
	srv := streamServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"a.txt\"}"}}]}}]}`,
	})
	t.Cleanup(srv.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-chat", Client: srv.Client()})

	resp, err := client.generateOpenAIChatStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "read it"}}}, nil)
	if err != nil {
		t.Fatalf("generateOpenAIChatStream error = %v, want nil", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("resp.ToolCalls = %#v, want one assembled call", resp.ToolCalls)
	}
	c := resp.ToolCalls[0]
	if c.ID != "call_1" || c.Name != "read_file" || c.Arguments["path"] != "a.txt" {
		t.Fatalf("assembled tool call = %#v, want read_file path=a.txt id=call_1", c)
	}
}
