package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionReq is the body for POST /v1/sessions.
type SessionReq struct {
	CompanyID string `json:"company_id"`
	AgentID   string `json:"agent_id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
}

// TaskReq is the body for POST /v1/tasks. ID is caller-minted and required.
type TaskReq struct {
	ID        string   `json:"id"`
	Input     string   `json:"input"`
	CompanyID string   `json:"company_id"`
	AgentID   string   `json:"agent_id"`
	SessionID string   `json:"session_id"`
	Images    []string `json:"images,omitempty"`
}

// CoreClient talks to the Legion core over its HTTP API.
type CoreClient interface {
	EnsureSession(ctx context.Context, req SessionReq) (sessionID string, err error)
	SubmitTask(ctx context.Context, req TaskReq) (taskID string, err error)
	TaskResult(ctx context.Context, taskID string) (text string, done bool, err error)
}

// HTTPCoreClient is the HTTP implementation of CoreClient.
type HTTPCoreClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPCoreClient builds a client for the core at baseURL authenticating with
// the given bearer token.
func NewHTTPCoreClient(baseURL, token string) *HTTPCoreClient {
	return &HTTPCoreClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *HTTPCoreClient) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", path, err)
	}
	defer resp.Body.Close()
	// Read failures must not pass for content: a connection cut mid-body used
	// to yield a truncated document that the caller then reported as malformed
	// JSON, dressing a network failure up as a protocol one.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response body: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// EnsureSession creates a session and returns its id.
func (c *HTTPCoreClient) EnsureSession(ctx context.Context, req SessionReq) (string, error) {
	data, err := c.postJSON(ctx, "/v1/sessions", req)
	if err != nil {
		return "", fmt.Errorf("ensure session: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode session response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("ensure session: response had empty id")
	}
	return out.ID, nil
}

// SubmitTask submits a task and returns its id.
func (c *HTTPCoreClient) SubmitTask(ctx context.Context, req TaskReq) (string, error) {
	data, err := c.postJSON(ctx, "/v1/tasks", req)
	if err != nil {
		return "", fmt.Errorf("submit task: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode task response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("submit task: response had empty id")
	}
	return out.ID, nil
}

// TaskResult fetches a task's current result. done is true once the task reaches
// a terminal status (done/failed/suspended); text is the answer, non-empty only
// on a successful completion. A not-yet-terminal task returns done=false with no
// error so the poller retries.
func (c *HTTPCoreClient) TaskResult(ctx context.Context, taskID string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/tasks/"+taskID+"/result", nil)
	if err != nil {
		return "", false, fmt.Errorf("build task result request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("call task result %q: %w", taskID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false, fmt.Errorf("read task result %q response body: %w", taskID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("task result %q returned %s: %s", taskID, resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", false, fmt.Errorf("decode task result %q: %w", taskID, err)
	}
	switch out.Status {
	case "done", "failed", "suspended":
		return out.Result, true, nil
	case "pending", "running", "assigned":
		// Not terminal yet: no error, poller retries. This is the contract the
		// doc comment describes, and it is now stated as a case of its own rather
		// than being whatever fell through.
		return "", false, nil
	default:
		// A status this client has never heard of is not "not finished yet".
		// Version skew or a renamed field used to land here and be answered like
		// an in-flight task, so PollOnce retried forever: the user waiting in
		// Telegram never got a reply, and nothing was logged, because runner.go
		// only warns when err is non-nil. Returning an error routes it to that
		// existing warning.
		return "", false, fmt.Errorf("task result %q: unknown status %q", taskID, out.Status)
	}
}
