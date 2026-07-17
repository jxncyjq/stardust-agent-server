package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

const testPNGDataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// stringContentResponse is a standard OpenAI-style chat response whose message
// content is a plain string, used so the test exercises the unchanged
// string-content response path while asserting on the captured request body.
func stringContentResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"vision pipeline ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}
}

// TestGenerateOpenAIChatTextOnlyContentIsString locks in backward compatibility:
// with no images the user message content must serialize as a plain JSON string,
// byte-for-byte identical to the pre-vision request body.
func TestGenerateOpenAIChatTextOnlyContentIsString(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		stringContentResponse(t)(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		Model:   "example-model",
		Client:  server.Client(),
	})

	resp, err := client.Generate(context.Background(), port.InferenceRequest{
		RequestID: "task-text:run",
		Prompt:    "你是什么模型?",
	})
	if err != nil {
		t.Fatalf("Generate(text-only) error = %v, want nil", err)
	}
	if resp.Text != "vision pipeline ok" {
		t.Fatalf("Generate(text-only).Text = %q, want vision pipeline ok", resp.Text)
	}

	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message", captured["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}
	content, ok := message["content"].(string)
	if !ok {
		t.Fatalf("content type = %T (%#v), want string for text-only request", message["content"], message["content"])
	}
	if content != "你是什么模型?" {
		t.Fatalf("content = %q, want the prompt string", content)
	}
}

// TestGenerateOpenAIChatWithImagesEmitsVisionContent asserts the OpenAI vision
// format: content is an array with one text part plus one image_url part per
// image, each image_url.url carrying the original data URI.
func TestGenerateOpenAIChatWithImagesEmitsVisionContent(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("Decode(request body) error = %v, want nil", err)
		}
		stringContentResponse(t)(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		Model:   "example-model",
		Client:  server.Client(),
	})

	secondImage := "data:image/jpeg;base64,/9j/4AAQSkZJRg=="
	resp, err := client.Generate(context.Background(), port.InferenceRequest{
		RequestID: "task-vision:run",
		Prompt:    "描述这些图片",
		Images:    []string{testPNGDataURI, secondImage},
	})
	if err != nil {
		t.Fatalf("Generate(with images) error = %v, want nil", err)
	}
	if resp.Text != "vision pipeline ok" {
		t.Fatalf("Generate(with images).Text = %q, want vision pipeline ok", resp.Text)
	}

	messages := captured["messages"].([]any)
	message := messages[0].(map[string]any)
	parts, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T (%#v), want array for vision request", message["content"], message["content"])
	}
	if len(parts) != 3 {
		t.Fatalf("content parts = %d, want 3 (1 text + 2 images): %#v", len(parts), parts)
	}

	textPart := parts[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "描述这些图片" {
		t.Fatalf("first part = %#v, want {type:text, text:prompt}", textPart)
	}

	wantURLs := []string{testPNGDataURI, secondImage}
	for i, want := range wantURLs {
		imgPart, ok := parts[i+1].(map[string]any)
		if !ok || imgPart["type"] != "image_url" {
			t.Fatalf("part %d = %#v, want {type:image_url,...}", i+1, parts[i+1])
		}
		imageURLObj, ok := imgPart["image_url"].(map[string]any)
		if !ok {
			t.Fatalf("part %d image_url = %#v, want object", i+1, imgPart["image_url"])
		}
		if imageURLObj["url"] != want {
			t.Fatalf("part %d image_url.url = %q, want %q", i+1, imageURLObj["url"], want)
		}
	}
}

// TestGenerateOpenAIChatRejectsMalformedImageDataURI proves the fail-loud
// behaviour: an image that is not a data URI returns an error instead of
// shipping bad data to the model, and no HTTP request is issued.
func TestGenerateOpenAIChatRejectsMalformedImageDataURI(t *testing.T) {
	t.Parallel()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	client := NewHTTPMaasClient(HTTPMaasConfig{
		BaseURL: server.URL,
		Model:   "example-model",
		Client:  server.Client(),
	})

	_, err := client.Generate(context.Background(), port.InferenceRequest{
		RequestID: "task-bad:run",
		Prompt:    "看图",
		Images:    []string{"https://example.com/not-a-data-uri.png"},
	})
	if err == nil {
		t.Fatalf("Generate(malformed image) error = nil, want non-nil")
	}
	if called {
		t.Fatalf("Generate(malformed image) issued an HTTP request, want it to fail before sending")
	}
}

// TestOpenAIChatUserContentBackwardCompatibility unit-tests the content builder
// directly: no images yields the bare prompt string; the malformed-image case
// returns an error.
func TestOpenAIChatUserContentBackwardCompatibility(t *testing.T) {
	t.Parallel()

	content, err := openAIChatUserContent("hello", nil, 0)
	if err != nil {
		t.Fatalf("openAIChatUserContent(no images) error = %v, want nil", err)
	}
	if got, ok := content.(string); !ok || got != "hello" {
		t.Fatalf("openAIChatUserContent(no images) = %#v, want string \"hello\"", content)
	}

	if _, err := openAIChatUserContent("hello", []string{"not-data"}, 0); err == nil {
		t.Fatalf("openAIChatUserContent(bad image) error = nil, want non-nil")
	}
}
