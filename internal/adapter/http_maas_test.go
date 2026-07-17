package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

func TestHTTPMaasClientGeneratePostsInferenceRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var gotPath string
	var gotAuth string
	var gotReq port.InferenceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("HTTPMaasClient.Generate() method = %s, want %s", r.Method, http.MethodPost)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(port.InferenceResponse{Text: "model result", ReasoningSummary: "public reasoning"}); err != nil {
			t.Fatalf("Encode(response body) error = %v, want nil", err)
		}
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		APIKey:  "secret-token",
		Client:  server.Client(),
	})

	resp, err := client.Generate(ctx, port.InferenceRequest{
		RequestID: "task-1:run",
		Prompt:    "hello",
	})
	if err != nil {
		t.Fatalf("HTTPMaasClient.Generate() error = %v, want nil", err)
	}
	if resp.Text != "model result" {
		t.Fatalf("HTTPMaasClient.Generate().Text = %q, want %q", resp.Text, "model result")
	}
	if resp.ReasoningSummary != "public reasoning" {
		t.Fatalf("HTTPMaasClient.Generate().ReasoningSummary = %q, want public reasoning", resp.ReasoningSummary)
	}
	if gotPath != "/v1/inference/generate" {
		t.Fatalf("HTTPMaasClient.Generate() path = %q, want %q", gotPath, "/v1/inference/generate")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("HTTPMaasClient.Generate() authorization = %q, want bearer token", gotAuth)
	}
	if gotReq.RequestID != "task-1:run" || gotReq.Prompt != "hello" {
		t.Fatalf("HTTPMaasClient.Generate() request = %#v, want request id and prompt", gotReq)
	}
}

func TestHTTPMaasClientGeneratePostsOpenAIChatWhenModelConfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var gotPath string
	var gotAuth string
	var gotReq map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"deepseek result","reasoning_content":"model reasoning content","tool_calls":[{"id":"call-1","type":"function","function":{"name":"search_content","arguments":"{\"pattern\":\"cache\",\"directory\":\".\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":50,"total_tokens":55}}`))
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		APIKey:  "secret-token",
		Model:   "example-model",
		Client:  server.Client(),
	})

	resp, err := client.Generate(ctx, port.InferenceRequest{
		RequestID: "task-1:run",
		Prompt:    "hello",
		Tools: []port.InferenceTool{{
			Name:        "search_content",
			Description: "search files",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("HTTPMaasClient.Generate(openai chat) error = %v, want nil", err)
	}
	if resp.Text != "deepseek result" {
		t.Fatalf("HTTPMaasClient.Generate(openai chat).Text = %q, want deepseek result", resp.Text)
	}
	if resp.ReasoningSummary != "model reasoning content" {
		t.Fatalf("HTTPMaasClient.Generate(openai chat).ReasoningSummary = %q, want model reasoning content", resp.ReasoningSummary)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "search_content" || resp.ToolCalls[0].Arguments["pattern"] != "cache" {
		t.Fatalf("HTTPMaasClient.Generate(openai chat).ToolCalls = %#v, want parsed search_content call", resp.ToolCalls)
	}
	if resp.PromptTokens != 5 || resp.CompletionTokens != 50 || resp.TotalTokens != 55 {
		t.Fatalf("HTTPMaasClient.Generate(openai chat) tokens = %d/%d/%d, want 5/50/55", resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("HTTPMaasClient.Generate(openai chat) path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("HTTPMaasClient.Generate(openai chat) auth = %q, want bearer token", gotAuth)
	}
	if gotReq["model"] != "example-model" {
		t.Fatalf("OpenAI chat model = %#v, want example-model; req=%#v", gotReq["model"], gotReq)
	}
	messages, ok := gotReq["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("OpenAI chat messages = %#v, want one user message", gotReq["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" || message["content"] != "hello" {
		t.Fatalf("OpenAI chat first message = %#v, want user prompt", messages[0])
	}
	tools, ok := gotReq["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("OpenAI chat tools = %#v, want one tool descriptor", gotReq["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("OpenAI chat tool = %#v, want object", tools[0])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("OpenAI chat tool function = %#v, want object", tool["function"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("OpenAI chat tool parameters = %#v, want type object", fn["parameters"])
	}
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Fatalf("OpenAI chat tool parameters.properties = %#v, want object", params["properties"])
	}
}

func TestOpenAIChatRequestInjectsCacheControlAtStablePrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var gotReq map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`))
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL:           server.URL,
		Model:             "example-model",
		Client:            server.Client(),
		EnablePromptCache: true,
	})

	stable := "STABLE_SYSTEM_PROMPT_FRAMING"
	prompt := stable + "volatile task tail"
	if _, err := client.Generate(ctx, port.InferenceRequest{
		RequestID:       "task-1:run",
		Prompt:          prompt,
		StablePrefixLen: len([]rune(stable)),
	}); err != nil {
		t.Fatalf("HTTPMaasClient.Generate() error = %v, want nil", err)
	}

	messages, ok := gotReq["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one user message", gotReq["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}
	parts, ok := message["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want two content parts (stable + volatile)", message["content"])
	}
	stablePart, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("stable part = %#v, want object", parts[0])
	}
	if stablePart["type"] != "text" || stablePart["text"] != stable {
		t.Fatalf("stable part = %#v, want text part carrying stable prefix", stablePart)
	}
	cacheControl, ok := stablePart["cache_control"].(map[string]any)
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("stable part cache_control = %#v, want {type: ephemeral}", stablePart["cache_control"])
	}
	volatilePart, ok := parts[1].(map[string]any)
	if !ok || volatilePart["text"] != "volatile task tail" {
		t.Fatalf("volatile part = %#v, want remaining prompt without cache_control", parts[1])
	}
	if _, present := volatilePart["cache_control"]; present {
		t.Fatalf("volatile part carried cache_control = %#v, want none", volatilePart["cache_control"])
	}
}

func TestOpenAIChatToolsNormalizeFunctionParameters(t *testing.T) {
	t.Parallel()

	tools := openAIChatTools([]port.InferenceTool{{
		Name: "read_file",
		InputSchema: map[string]any{
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
	}})
	if len(tools) != 1 {
		t.Fatalf("openAIChatTools() len = %d, want 1", len(tools))
	}
	params := tools[0].Function.Parameters
	if params["type"] != "object" {
		t.Fatalf("openAIChatTools().parameters.type = %#v, want object", params["type"])
	}
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Fatalf("openAIChatTools().parameters.properties = %#v, want object", params["properties"])
	}
}

func TestOpenAIChatToolsOmitNilRequired(t *testing.T) {
	t.Parallel()

	tools := openAIChatTools([]port.InferenceTool{{
		Name: "read_messages",
		InputSchema: map[string]any{
			"type":       "object",
			"required":   nil,
			"properties": map[string]any{},
		},
	}})
	if len(tools) != 1 {
		t.Fatalf("openAIChatTools() len = %d, want 1", len(tools))
	}
	params := tools[0].Function.Parameters
	if _, ok := params["required"]; ok {
		t.Fatalf("openAIChatTools().parameters.required = %#v, want omitted when nil", params["required"])
	}
}

func TestHTTPMaasClientGenerateReturnsErrorForNonOKStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "quota exceeded", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		Client:  server.Client(),
	})

	_, err := client.Generate(ctx, port.InferenceRequest{RequestID: "task-1:run", Prompt: "hello"})
	if err == nil {
		t.Fatalf("HTTPMaasClient.Generate() error = nil, want non-OK status error")
	}
}

func TestCachedTokensIsProviderNeutral(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		usage openAIChatUsage
		want  int
	}{
		{
			name:  "nested prompt_tokens_details (OpenAI/Anthropic-compatible)",
			usage: openAIChatUsage{PromptTokensDetails: &openAIChatPromptTokenInfo{CachedTokens: 1500}},
			want:  1500,
		},
		{
			name:  "flat prompt_cache_hit_tokens (DeepSeek-style)",
			usage: openAIChatUsage{PromptCacheHitTokens: 1280},
			want:  1280,
		},
		{
			name:  "nested wins when both present",
			usage: openAIChatUsage{PromptTokensDetails: &openAIChatPromptTokenInfo{CachedTokens: 900}, PromptCacheHitTokens: 100},
			want:  900,
		},
		{
			name:  "no cache reported",
			usage: openAIChatUsage{PromptTokens: 1800},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cachedTokens(tt.usage); got != tt.want {
				t.Fatalf("cachedTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}
