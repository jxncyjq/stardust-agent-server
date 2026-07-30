package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

const defaultMaasInferencePath = "/v1/inference/generate"
const defaultOpenAIChatCompletionsPath = "/chat/completions"

var _ port.MaasInferenceClient = (*HTTPMaasClient)(nil)
var _ port.MaasStreamingClient = (*HTTPMaasClient)(nil)

type HTTPMaasConfig struct {
	BaseURL      string
	APIKey       string
	Model        string
	EndpointPath string
	Client       *http.Client
	// EnablePromptCache turns on provider prompt caching for the OpenAI-compatible
	// path: when set, a request carrying InferenceRequest.StablePrefixLen emits its
	// stable prefix as a cache_control content block. Optional; defaults to false,
	// keeping request bodies byte-for-byte identical for providers that would
	// reject the extra field.
	EnablePromptCache bool
}

type HTTPMaasClient struct {
	baseURL           string
	apiKey            string
	model             string
	endpointPath      string
	client            *http.Client
	enablePromptCache bool
}

func NewHTTPMaasClient(cfg HTTPMaasConfig) *HTTPMaasClient {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	endpointPath := cfg.EndpointPath
	if endpointPath == "" {
		if cfg.Model != "" {
			endpointPath = defaultOpenAIChatCompletionsPath
		} else {
			endpointPath = defaultMaasInferencePath
		}
	}
	return &HTTPMaasClient{
		baseURL:           strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:            cfg.APIKey,
		model:             cfg.Model,
		endpointPath:      endpointPath,
		client:            client,
		enablePromptCache: cfg.EnablePromptCache,
	}
}

// GenerateStream streams a chat completion when this client targets an
// OpenAI-compatible chat model; without a model (the plain maas inference path)
// there is no streaming protocol, so it degrades to the synchronous Generate and
// makes no onDelta calls. Either way it returns a complete InferenceResponse.
func (c *HTTPMaasClient) GenerateStream(ctx context.Context, req port.InferenceRequest, onDelta func(delta string)) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if c.model == "" {
		return c.Generate(ctx, req)
	}
	return c.generateOpenAIChatStream(ctx, req, onDelta)
}

func (c *HTTPMaasClient) Generate(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := ctx.Err(); err != nil {
		return port.InferenceResponse{}, err
	}
	if c.model != "" {
		return c.generateOpenAIChat(ctx, req)
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("encode maas inference request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpointPath, &body)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("create maas inference request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("call maas inference endpoint: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		msg, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		return port.InferenceResponse{}, fmt.Errorf("maas inference endpoint returned %s: %s", httpResp.Status, strings.TrimSpace(string(msg)))
	}
	var resp port.InferenceResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("decode maas inference response: %w", err)
	}
	return resp, nil
}

func (c *HTTPMaasClient) generateOpenAIChat(ctx context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("validate inference request: %w", err)
	}
	messages, err := c.openAIChatMessages(req)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("build openai chat messages: %w", err)
	}
	body, err := json.Marshal(openAIChatCompletionRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    openAIChatTools(req.Tools),
	})
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("encode openai chat request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpointPath, bytes.NewReader(body))
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("create openai chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("call openai chat endpoint: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		msg, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1024))
		return port.InferenceResponse{}, fmt.Errorf("openai chat endpoint returned %s: %s", httpResp.Status, strings.TrimSpace(string(msg)))
	}
	var resp openAIChatCompletionResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("decode openai chat response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return port.InferenceResponse{}, fmt.Errorf("openai chat response contained no choices")
	}
	message := resp.Choices[0].Message
	toolCalls, err := openAIToolCalls(message.ToolCalls)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("parse openai chat tool calls: %w", err)
	}
	return port.InferenceResponse{
		Text:             message.Content,
		ReasoningSummary: firstNonEmpty(message.ReasoningSummary, message.ReasoningContent, message.Reasoning),
		ToolCalls:        toolCalls,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CachedTokens:     cachedTokens(resp.Usage),
		TotalTokens:      resp.Usage.TotalTokens,
	}, nil
}

// openAIChatStreamChunk is one SSE "data:" payload of a streaming chat
// completion. Delta.Content carries a text increment; Delta.ToolCalls carries
// tool-call fragments keyed by index; Delta.ReasoningContent carries reasoning
// fragments for reasoning models (e.g. deepseek-reasoner). Usage is non-nil
// only on the chunk(s) requested via stream_options.include_usage.
type openAIChatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string                     `json:"content"`
			ToolCalls        []openAIChatStreamToolCall `json:"tool_calls"`
			ReasoningContent string                     `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *openAIChatUsage `json:"usage"`
}

// openAIChatStreamToolCall is one fragment of a streamed tool call. The first
// fragment for a given Index carries ID and Function.Name; every fragment
// (including the first) carries a substring of Function.Arguments that must be
// concatenated in arrival order to recover the complete JSON arguments.
type openAIChatStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// generateOpenAIChatStream runs an OpenAI/DeepSeek chat completion in streaming
// mode. It calls onDelta with each text delta while accumulating the full text,
// reassembles fragmented tool_calls by their index, and reads the trailing usage
// chunk — returning the same complete InferenceResponse the non-streaming path
// produces. onDelta may be nil.
func (c *HTTPMaasClient) generateOpenAIChatStream(ctx context.Context, req port.InferenceRequest, onDelta func(delta string)) (port.InferenceResponse, error) {
	if err := req.Validate(); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("validate inference request: %w", err)
	}
	messages, err := c.openAIChatMessages(req)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("build openai chat messages: %w", err)
	}
	body, err := json.Marshal(openAIChatCompletionRequest{
		Model:         c.model,
		Messages:      messages,
		Tools:         openAIChatTools(req.Tools),
		Stream:        true,
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("encode openai chat stream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.endpointPath, bytes.NewReader(body))
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("create openai chat stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("call openai chat stream endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return port.InferenceResponse{}, fmt.Errorf("openai chat stream endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	var textB strings.Builder
	var reasoningB strings.Builder
	// toolAcc accumulates tool-call fragments by index: id/name arrive first,
	// arguments stream as substrings that must be concatenated in order.
	type toolAccum struct {
		id, name string
		args     strings.Builder
	}
	toolByIndex := map[int]*toolAccum{}
	var order []int
	var usage *openAIChatUsage

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk openAIChatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return port.InferenceResponse{}, fmt.Errorf("decode stream chunk %q: %w", data, err)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				textB.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(ch.Delta.Content)
				}
			}
			if ch.Delta.ReasoningContent != "" {
				reasoningB.WriteString(ch.Delta.ReasoningContent)
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := toolByIndex[tc.Index]
				if !ok {
					acc = &toolAccum{}
					toolByIndex[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return port.InferenceResponse{}, fmt.Errorf("read openai chat stream: %w", err)
	}

	// Assemble accumulated fragments into the same []openAIChatToolCall the
	// non-streaming decoder yields, then reuse openAIToolCalls for identical
	// arguments parsing.
	assembled := make([]openAIChatToolCall, 0, len(order))
	for _, idx := range order {
		acc := toolByIndex[idx]
		assembled = append(assembled, openAIChatToolCall{
			ID:       acc.id,
			Type:     "function",
			Function: openAIChatCallFunction{Name: acc.name, Arguments: acc.args.String()},
		})
	}

	assembledToolCalls, err := openAIToolCalls(assembled)
	if err != nil {
		return port.InferenceResponse{}, fmt.Errorf("parse openai chat stream tool calls: %w", err)
	}
	out := port.InferenceResponse{
		Text:             textB.String(),
		ReasoningSummary: reasoningB.String(),
		ToolCalls:        assembledToolCalls,
	}
	if usage != nil {
		out.PromptTokens = usage.PromptTokens
		out.CompletionTokens = usage.CompletionTokens
		out.CachedTokens = cachedTokens(*usage)
		out.TotalTokens = usage.TotalTokens
	}
	return out, nil
}

// openAIChatMessages renders the request as OpenAI chat messages. With
// req.Messages empty it produces the historical single user message —
// byte-for-byte the previous body, prompt-cache breakpoint included. With
// req.Messages set it renders the multi-turn exchange, pairing each tool result
// with the assistant tool call that produced it, so the model can see the calls
// it already made.
func (c *HTTPMaasClient) openAIChatMessages(req port.InferenceRequest) ([]openAIChatRequestMessage, error) {
	if len(req.Messages) == 0 {
		stablePrefixLen := 0
		if c.enablePromptCache {
			stablePrefixLen = req.StablePrefixLen
		}
		content, err := openAIChatUserContent(req.Prompt, req.Images, stablePrefixLen)
		if err != nil {
			return nil, fmt.Errorf("build openai chat user content: %w", err)
		}
		return []openAIChatRequestMessage{{Role: "user", Content: content}}, nil
	}
	// A multi-turn exchange is append-only, so message[0]'s stable task framing
	// carries its own StablePrefixLen and gets an explicit cache breakpoint here;
	// every later user turn has StablePrefixLen 0 and is unaffected, staying
	// append-only for providers that cache automatically.
	out := make([]openAIChatRequestMessage, 0, len(req.Messages))
	for i, msg := range req.Messages {
		switch msg.Role {
		case port.RoleUser:
			stablePrefixLen := 0
			if c.enablePromptCache {
				stablePrefixLen = msg.StablePrefixLen
			}
			content, err := openAIChatUserContent(msg.Content, msg.Images, stablePrefixLen)
			if err != nil {
				return nil, fmt.Errorf("build user content for message %d: %w", i, err)
			}
			out = append(out, openAIChatRequestMessage{Role: "user", Content: content})
		case port.RoleAssistant:
			calls, err := openAIChatRequestToolCalls(msg.ToolCalls)
			if err != nil {
				return nil, fmt.Errorf("encode tool calls for message %d: %w", i, err)
			}
			out = append(out, openAIChatRequestMessage{Role: "assistant", Content: msg.Content, ToolCalls: calls})
		case port.RoleTool:
			out = append(out, openAIChatRequestMessage{Role: "tool", Content: msg.Content, ToolCallID: msg.ToolCallID})
		default:
			return nil, fmt.Errorf("message %d has unknown role %q", i, msg.Role)
		}
	}
	return out, nil
}

// openAIChatRequestToolCalls re-encodes tool calls the model previously emitted.
// Their arguments were decoded into a string map on the way in; marshalling that
// map back is lossless for the model's purposes — it sees the same keys and
// values — and is the shape the provider requires on the way out.
func openAIChatRequestToolCalls(calls []domain.ToolCall) ([]openAIChatToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]openAIChatToolCall, 0, len(calls))
	for _, call := range calls {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("marshal arguments of tool call %s: %w", call.ID, err)
		}
		out = append(out, openAIChatToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: openAIChatCallFunction{Name: call.Name, Arguments: string(args)},
		})
	}
	return out, nil
}

type openAIChatCompletionRequest struct {
	Model         string                     `json:"model"`
	Messages      []openAIChatRequestMessage `json:"messages"`
	Tools         []openAIChatTool           `json:"tools,omitempty"`
	Stream        bool                       `json:"stream,omitempty"`
	StreamOptions *openAIStreamOptions       `json:"stream_options,omitempty"`
}

// openAIStreamOptions requests a final usage chunk in the stream. Without it a
// streamed response carries no usage, silently dropping the token accounting a
// non-streamed response returns by default.
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIChatRequestMessage is the message shape sent in a chat-completion
// request. Content is typed as any so it can hold either a plain string (the
// text-only, backward-compatible form) or a []contentPart array (the
// multimodal/vision form). Responses are decoded into openAIChatMessage, whose
// Content stays a string, so this request/response split keeps response parsing
// unchanged.
// ToolCalls carries an assistant turn's tool calls; ToolCallID pairs a tool
// turn back to the call it answers. Both are omitted on plain user turns, so a
// single-turn request body stays exactly as it was.
type openAIChatRequestMessage struct {
	Role       string               `json:"role"`
	Content    any                  `json:"content"`
	ToolCalls  []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}

// contentPart is one element of a multimodal content array. Text is set for a
// {"type":"text"} part; ImageURL is set for a {"type":"image_url"} part.
// CacheControl marks a prompt-cache breakpoint (Anthropic/compatible gateways);
// it is nil on parts that carry no breakpoint.
type contentPart struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	ImageURL     *imageURL     `json:"image_url,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// cacheControl is the provider prompt-cache directive attached to a content
// part. Only "ephemeral" is emitted; providers that do not support caching
// ignore the field.
type cacheControl struct {
	Type string `json:"type"`
}

// openAIChatUserContent builds the content field for the user message. With no
// images it returns the prompt as a plain string, preserving the exact
// text-only request body. With images it returns a content array: one text part
// followed by an image_url part per image. Each image must be a data URI
// (prefixed "data:"); a malformed image fails loudly rather than shipping bad
// data to the model.
func openAIChatUserContent(prompt string, images []string, stablePrefixLen int) (any, error) {
	if len(images) == 0 && stablePrefixLen <= 0 {
		return prompt, nil
	}
	parts := make([]contentPart, 0, len(images)+2)
	if stablePrefixLen > 0 {
		runes := []rune(prompt)
		if stablePrefixLen > len(runes) {
			stablePrefixLen = len(runes)
		}
		parts = append(parts, contentPart{
			Type:         "text",
			Text:         string(runes[:stablePrefixLen]),
			CacheControl: &cacheControl{Type: "ephemeral"},
		})
		if tail := string(runes[stablePrefixLen:]); tail != "" {
			parts = append(parts, contentPart{Type: "text", Text: tail})
		}
	} else {
		parts = append(parts, contentPart{Type: "text", Text: prompt})
	}
	for i, image := range images {
		if !strings.HasPrefix(image, "data:") {
			return nil, fmt.Errorf("image %d is not a data URI (must start with \"data:\")", i)
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: image}})
	}
	return parts, nil
}

type openAIChatMessage struct {
	Role             string               `json:"role"`
	Content          string               `json:"content"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ReasoningSummary string               `json:"reasoning_summary,omitempty"`
	Reasoning        string               `json:"reasoning,omitempty"`
	ToolCalls        []openAIChatToolCall `json:"tool_calls,omitempty"`
}

type openAIChatCompletionResponse struct {
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIChatUsage    `json:"usage"`
}

type openAIChatUsage struct {
	PromptTokens        int                        `json:"prompt_tokens"`
	CompletionTokens    int                        `json:"completion_tokens"`
	TotalTokens         int                        `json:"total_tokens"`
	PromptTokensDetails *openAIChatPromptTokenInfo `json:"prompt_tokens_details,omitempty"`
	// PromptCacheHitTokens is the flat-field cache-hit convention used by some
	// OpenAI-compatible providers (e.g. DeepSeek) that report cache hits outside
	// prompt_tokens_details. Contract-optional: absent on providers that do not
	// use it.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
}

// openAIChatPromptTokenInfo carries the optional cache breakdown of the prompt
// tokens. Providers that do not implement prompt caching omit this object; a nil
// pointer means "no cache detail reported", not a fabricated zero.
type openAIChatPromptTokenInfo struct {
	CachedTokens int `json:"cached_tokens"`
}

// cachedTokens extracts the prompt-cache hit count from an OpenAI-compatible
// usage block in a provider-neutral way. It accepts either the nested
// prompt_tokens_details.cached_tokens convention (OpenAI, Anthropic-compatible)
// or the flat prompt_cache_hit_tokens convention (DeepSeek and similar),
// returning whichever the provider populated. Zero means no cache hit reported,
// not a fabricated default.
func cachedTokens(u openAIChatUsage) int {
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	return u.PromptCacheHitTokens
}

type openAIChatChoice struct {
	Message openAIChatMessage `json:"message"`
}

type openAIChatTool struct {
	Type     string             `json:"type"`
	Function openAIChatFunction `json:"function"`
}

type openAIChatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openAIChatToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIChatCallFunction `json:"function"`
}

type openAIChatCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func openAIChatTools(tools []port.InferenceTool) []openAIChatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAIChatTool, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		out = append(out, openAIChatTool{
			Type: "function",
			Function: openAIChatFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  normalizeFunctionParameters(tool.InputSchema),
			},
		})
	}
	return out
}

func normalizeFunctionParameters(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	normalized := make(map[string]any, len(schema)+2)
	maps.Copy(normalized, schema)
	if normalized["type"] == nil || normalized["type"] == "" {
		normalized["type"] = "object"
	}
	if normalized["properties"] == nil {
		normalized["properties"] = map[string]any{}
	}
	if isNilRequired(normalized["required"]) {
		delete(normalized, "required")
	}
	return normalized
}

func isNilRequired(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func openAIToolCalls(calls []openAIChatToolCall) ([]domain.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]domain.ToolCall, 0, len(calls))
	for _, call := range calls {
		args := map[string]string{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			var raw map[string]any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &raw); err != nil {
				return nil, fmt.Errorf("decode tool call arguments %q: %w", call.Function.Arguments, err)
			}
			for key, value := range raw {
				switch typed := value.(type) {
				case string:
					args[key] = typed
				default:
					args[key] = fmt.Sprint(typed)
				}
			}
		}
		out = append(out, domain.ToolCall{
			ID:        firstNonEmpty(call.ID, call.Function.Name),
			Name:      call.Function.Name,
			Arguments: args,
		})
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
