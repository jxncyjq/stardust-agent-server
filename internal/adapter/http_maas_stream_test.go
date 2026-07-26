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

// TestGenerateOpenAIChatStreamAssemblesInterleavedToolCallsByIndex verifies
// that fragments for two different tool_call indexes arriving interleaved
// (index0 first-chunk, index1 first-chunk, index0 arg-chunk, index1
// arg-chunk) are accumulated independently per index and assembled into two
// distinct, correctly-reconstructed tool calls, ordered by first appearance.
func TestGenerateOpenAIChatStreamAssemblesInterleavedToolCallsByIndex(t *testing.T) {
	srv := streamServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"list_files","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"dir\":\"/tmp\"}"}}]}}]}`,
	})
	t.Cleanup(srv.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-chat", Client: srv.Client()})

	resp, err := client.generateOpenAIChatStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "list then read"}}}, nil)
	if err != nil {
		t.Fatalf("generateOpenAIChatStream error = %v, want nil", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("resp.ToolCalls = %#v, want two assembled calls", resp.ToolCalls)
	}
	first, second := resp.ToolCalls[0], resp.ToolCalls[1]
	if first.ID != "call_0" || first.Name != "read_file" || first.Arguments["path"] != "a.txt" {
		t.Fatalf("first tool call = %#v, want read_file path=a.txt id=call_0", first)
	}
	if second.ID != "call_1" || second.Name != "list_files" || second.Arguments["dir"] != "/tmp" {
		t.Fatalf("second tool call = %#v, want list_files dir=/tmp id=call_1", second)
	}
}

// TestGenerateOpenAIChatStreamFailsLoudOnInvalidToolCallArguments verifies
// that when the concatenated tool_call arguments fragments do not form valid
// JSON (here only "{\"path\":" is ever sent, so the object is never closed),
// generateOpenAIChatStream returns an error instead of silently degrading to
// an empty arguments map.
func TestGenerateOpenAIChatStreamFailsLoudOnInvalidToolCallArguments(t *testing.T) {
	srv := streamServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
	})
	t.Cleanup(srv.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-chat", Client: srv.Client()})

	_, err := client.generateOpenAIChatStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "read it"}}}, nil)
	if err == nil {
		t.Fatal("generateOpenAIChatStream error = nil, want error for invalid tool call arguments JSON (fail-loud)")
	}
}

func TestGenerateStreamUsesStreamingForOpenAIChat(t *testing.T) {
	srv := streamServer(t, []string{`{"choices":[{"delta":{"content":"hi"}}]}`})
	t.Cleanup(srv.Close)
	var client port.MaasStreamingClient = NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-chat", Client: srv.Client()})
	var got string
	resp, err := client.GenerateStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "x"}}},
		func(d string) { got += d })
	if err != nil {
		t.Fatalf("GenerateStream error = %v, want nil", err)
	}
	if got != "hi" || resp.Text != "hi" {
		t.Fatalf("got=%q resp.Text=%q, want hi", got, resp.Text)
	}
}

// A client with no model (the non-OpenAI maas path) has no streaming protocol;
// GenerateStream must degrade to the synchronous Generate rather than fail, so
// the runtime's single streaming code path stays valid for every provider.
func TestGenerateStreamFallsBackToSyncWithoutModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(port.InferenceResponse{Text: "sync result"})
	}))
	t.Cleanup(srv.Close)
	var client port.MaasStreamingClient = NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Client: srv.Client()}) // no Model
	var deltaCount int
	resp, err := client.GenerateStream(context.Background(),
		port.InferenceRequest{Prompt: "x"}, func(string) { deltaCount++ })
	if err != nil {
		t.Fatalf("GenerateStream error = %v, want nil", err)
	}
	if resp.Text != "sync result" {
		t.Fatalf("resp.Text = %q, want sync result", resp.Text)
	}
	if deltaCount != 0 {
		t.Fatalf("deltaCount = %d, want 0 (no streaming without a model)", deltaCount)
	}
}

// TestGenerateOpenAIChatStreamAccumulatesReasoningContent verifies that
// delta.reasoning_content fragments from the stream are accumulated and
// returned in the final InferenceResponse.ReasoningSummary, matching the
// contract that streaming produces the same complete InferenceResponse that
// Generate returns (for reasoning-capable models like deepseek-reasoner).
func TestGenerateOpenAIChatStreamAccumulatesReasoningContent(t *testing.T) {
	srv := streamServer(t, []string{
		`{"choices":[{"delta":{"reasoning_content":"Let me think"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":" about this"}}]}`,
		`{"choices":[{"delta":{"content":"The answer is 42"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":" step by step"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`,
	})
	t.Cleanup(srv.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{BaseURL: srv.URL, Model: "deepseek-reasoner", Client: srv.Client()})

	resp, err := client.generateOpenAIChatStream(context.Background(),
		port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleUser, Content: "think"}}}, nil)
	if err != nil {
		t.Fatalf("generateOpenAIChatStream error = %v, want nil", err)
	}
	if resp.Text != "The answer is 42" {
		t.Fatalf("resp.Text = %q, want 'The answer is 42'", resp.Text)
	}
	expectedReasoning := "Let me think about this step by step"
	if resp.ReasoningSummary != expectedReasoning {
		t.Fatalf("resp.ReasoningSummary = %q, want %q", resp.ReasoningSummary, expectedReasoning)
	}
}
