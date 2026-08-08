package tool

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/browser"
	"github.com/stardust/legion-agent/internal/domain"
)

// BrowserEventSink 是 tool 层对“发浏览器会话生命周期事件”的最小依赖（可选）。
// nil 表示不发（不破坏无事件总线的场景/测试）。实现方（cli 层）负责桥到平台 EventBus。
type BrowserEventSink interface {
	SessionOpened(ctx context.Context, sessionID, taskID, url string)
	SessionClosed(ctx context.Context, sessionID string)
}

// BrowserToolOptions 见 spec §2.2。Enabled 为 false 时 RegisterBrowserTools 是 no-op。
type BrowserToolOptions struct {
	Enabled bool
	Runtime browser.RuntimeAPI
	// Events 可选：非 nil 时在 browser_open/close 成功后发会话生命周期事件；nil 表示不发。
	Events BrowserEventSink
}

// RegisterBrowserTools 注册 browser_open/read/click/type/close。照 RegisterWebTools 语义：
// registry 为 nil、opts.Enabled 为 false、或 Runtime 为 nil 时是 no-op。
func RegisterBrowserTools(registry *Registry, opts BrowserToolOptions) {
	if registry == nil || !opts.Enabled || opts.Runtime == nil {
		return
	}
	rt := opts.Runtime

	registry.RegisterDescriptor(browserOpenDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		url := call.Arguments["url"]
		if url == "" {
			return failure(call.ID, "url is required"), nil
		}
		out, err := rt.Open(ctx, browser.OpenReq{URL: url, SessionID: call.Arguments["session_id"]})
		if err != nil {
			return failure(call.ID, err.Error()), nil
		}
		if opts.Events != nil {
			opts.Events.SessionOpened(ctx, out.SessionID, "", url)
		}
		return success(call.ID, fmt.Sprintf("session_id: %s\n%s", out.SessionID, out.Observation.Text)), nil
	}))

	registry.RegisterDescriptor(browserReadDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		obs, err := rt.Read(ctx, browser.ReadReq{SessionID: call.Arguments["session_id"], Mode: call.Arguments["mode"]})
		if err != nil {
			return failure(call.ID, err.Error()), nil
		}
		return success(call.ID, obs.Text), nil
	}))

	registry.RegisterDescriptor(browserClickDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		obs, err := rt.Click(ctx, browser.ClickReq{SessionID: call.Arguments["session_id"], Ref: call.Arguments["ref"]})
		if err != nil {
			return failure(call.ID, err.Error()), nil
		}
		return success(call.ID, obs.Text), nil
	}))

	registry.RegisterDescriptor(browserTypeDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		submit := call.Arguments["submit"] == "true"
		obs, err := rt.Type(ctx, browser.TypeReq{
			SessionID: call.Arguments["session_id"], Ref: call.Arguments["ref"],
			Text: call.Arguments["text"], Submit: submit,
		})
		if err != nil {
			return failure(call.ID, err.Error()), nil
		}
		return success(call.ID, obs.Text), nil
	}))

	registry.RegisterDescriptor(browserCloseDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		if err := rt.Close(ctx, browser.CloseReq{SessionID: call.Arguments["session_id"]}); err != nil {
			return failure(call.ID, err.Error()), nil
		}
		if opts.Events != nil {
			opts.Events.SessionClosed(ctx, call.Arguments["session_id"])
		}
		return success(call.ID, "ok"), nil
	}))
}

func success(id, out string) domain.ToolResult {
	return domain.ToolResult{CallID: id, Success: true, Output: out}
}
func failure(id, msg string) domain.ToolResult {
	return domain.ToolResult{CallID: id, Success: false, Error: msg}
}

func browserOpenDescriptor() Descriptor {
	return Descriptor{
		Name:        "browser_open",
		Description: "Open a URL in the agent's built-in browser. Returns a session_id and an accessibility-tree observation with stable refs.",
		RiskLevel:   "medium",
		Group:       "browser",
		Sensitive:   true, // 导航有副作用 → Manual 模式 gate
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url":        map[string]any{"type": "string", "description": "Absolute http/https URL."},
				"session_id": map[string]any{"type": "string", "description": "Reuse an existing session; omit to create one."},
			},
		},
	}
}

func browserReadDescriptor() Descriptor {
	return Descriptor{
		Name:        "browser_read",
		Description: "Read the current page as an accessibility tree with stable refs. Read-only.",
		RiskLevel:   "low",
		Group:       "browser",
		Sensitive:   false, // 只读 → Manual 放行、Plan 保留
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"session_id"},
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string"},
				"mode":       map[string]any{"type": "string", "description": "Reserved; only a11y supported in phase 1."},
			},
		},
	}
}

func browserClickDescriptor() Descriptor {
	return Descriptor{
		Name:        "browser_click",
		Description: "Click the element identified by a ref from the latest observation.",
		RiskLevel:   "medium",
		Group:       "browser",
		Sensitive:   true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"session_id", "ref"},
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string"},
				"ref":        map[string]any{"type": "string", "description": "Element ref like e12."},
			},
		},
	}
}

func browserTypeDescriptor() Descriptor {
	return Descriptor{
		Name:        "browser_type",
		Description: "Type text into the element identified by ref. Set submit=true to press Enter after.",
		RiskLevel:   "medium",
		Group:       "browser",
		Sensitive:   true,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"session_id", "ref", "text"},
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string"},
				"ref":        map[string]any{"type": "string"},
				"text":       map[string]any{"type": "string"},
				"submit":     map[string]any{"type": "boolean"},
			},
		},
	}
}

func browserCloseDescriptor() Descriptor {
	return Descriptor{
		Name:        "browser_close",
		Description: "Close a browser session and release its context.",
		RiskLevel:   "low",
		Group:       "browser",
		Sensitive:   false,
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"session_id"},
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string"},
			},
		},
	}
}
