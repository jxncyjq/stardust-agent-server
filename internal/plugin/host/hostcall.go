package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/tetratelabs/wazero/api"
)

// Error codes a host function returns to a guest instead of a result body. A
// host function has no way to return a Go error to the host process, so a
// refusal or a failure travels back to the guest as this envelope; returning a
// zero-length "success" body instead would be indistinguishable from a call
// that legitimately produced nothing.
const (
	// CodeDenied means the capability was granted but the specific arguments
	// were not authorized: a host outside Grant.AllowedHosts, a path outside
	// Grant.AllowedPaths or rejected by the workspace guard, a URL scheme the
	// http capability does not cover.
	CodeDenied = "DENIED"

	// CodeInvalidRequest means the request body could not be understood: it was
	// not decodable JSON, or a required field was absent. The host does not
	// guess a default for a field a plugin left out.
	CodeInvalidRequest = "INVALID_REQUEST"

	// CodeHostError means the request was authorized and well-formed, but the
	// host side of the work failed: an unreachable upstream, an unreadable
	// file, a tool that returned an error.
	CodeHostError = "HOST_ERROR"
)

// RuntimeEventCallFailed is the runtime event type published when a plugin call
// fails, matching the design doc's plugin/call_failed. The failure category
// travels in the event message as "category=<c>" (see CategoryDenied),
// following the encoding evolution.NewLearningRuntimeEvent already uses for
// structured RuntimeEvent fields — domain.RuntimeEvent has no category column
// and this task is not the place to add one.
const RuntimeEventCallFailed = "plugin/call_failed"

// CategoryDenied is the plugin/call_failed category for a call a host function
// refused on capability or allowlist grounds (design doc's error taxonomy). It
// is counted separately from plugin faults: a denial means the plugin
// overstepped, not that it is broken.
const CategoryDenied = "denied"

// Log levels the guest passes to the log host function, mapped onto slog.
const (
	guestLevelDebug uint32 = 0
	guestLevelInfo  uint32 = 1
	guestLevelWarn  uint32 = 2
	guestLevelError uint32 = 3
)

// httpResponseByteLimit caps how much of an upstream response body is copied
// back into guest memory. A guest's linear memory is capped too (see
// NewRuntime), so an unbounded copy would trap the plugin on a large download
// rather than telling it what happened.
const httpResponseByteLimit = 1 << 20

// hostCalls is the receiver behind every registered host function: one
// plugin's grant plus the dependencies its capabilities need.
//
// The methods NEVER call one another through wazero. wazero loses memory
// access when a host function calls another host function, so all shared work
// lives in the plain Go helpers at the bottom of this file.
type hostCalls struct {
	grant perm.Grant
	deps  Deps
}

// errorBody is the JSON envelope carrying a code and a message back to a guest.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// log implements "log(level i32, ptr i32, len i32)": it writes the guest's
// message to the host logger at the mapped level. It has no return value, so
// its own failures (an unreadable message region, an unknown level) are
// reported to the host logger at Error rather than silently dropped.
func (h hostCalls) log(_ context.Context, m api.Module, level, ptr, length uint32) {
	logger := h.deps.Logger.With("plugin", h.deps.PluginName)
	message, err := readGuestBytes(m, ptr, length)
	if err != nil {
		logger.Error("plugin log call: message region is unreadable", "error", err)
		return
	}
	switch level {
	case guestLevelDebug:
		logger.Debug(string(message))
	case guestLevelInfo:
		logger.Info(string(message))
	case guestLevelWarn:
		logger.Warn(string(message))
	case guestLevelError:
		logger.Error(string(message))
	default:
		// An unrecognized level is not silently rounded to info: the message is
		// still delivered, but at Error and labelled, so the broken caller is
		// visible instead of hidden behind a plausible default.
		logger.Error("plugin log call: unknown level", "level", level, "message", string(message))
	}
}

// configGet implements "config_get() -> i64": it hands the plugin its
// deployment-side configuration as the JSON it was configured with, verbatim.
func (h hostCalls) configGet(ctx context.Context, m api.Module) uint64 {
	return writeGuestBody(ctx, m, h.deps.Config)
}

// kvGet implements "kv_get(kp i32, kl i32) -> i64". The key is the raw bytes at
// kp..kp+kl; the host qualifies it with the plugin's namespace before touching
// the store, so a plugin cannot read another plugin's keys by spelling their
// key. The response is {"found":bool,"value":string}.
func (h hostCalls) kvGet(ctx context.Context, m api.Module, kp, kl uint32) uint64 {
	key, err := readGuestBytes(m, kp, kl)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("kv_get: %v", err))
	}
	if len(key) == 0 {
		return h.writeError(ctx, m, CodeInvalidRequest, "kv_get: key must not be empty")
	}
	value, found, err := h.deps.KV.Get(ctx, h.namespacedKey(string(key)))
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("kv_get %q: %v", key, err))
	}
	return h.writeJSON(ctx, m, struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}{Found: found, Value: value})
}

// kvPut implements "kv_put(kp i32, kl i32, vp i32, vl i32) -> i64", storing the
// value under the plugin's namespaced key. The response is {"ok":true}; a
// failure is an error envelope, never a quiet ok=false.
func (h hostCalls) kvPut(ctx context.Context, m api.Module, kp, kl, vp, vl uint32) uint64 {
	key, err := readGuestBytes(m, kp, kl)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("kv_put key: %v", err))
	}
	if len(key) == 0 {
		return h.writeError(ctx, m, CodeInvalidRequest, "kv_put: key must not be empty")
	}
	value, err := readGuestBytes(m, vp, vl)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("kv_put value: %v", err))
	}
	if err := h.deps.KV.Put(ctx, h.namespacedKey(string(key)), string(value)); err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("kv_put %q: %v", key, err))
	}
	return h.writeJSON(ctx, m, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

// httpRequestBody is the JSON request of the http_request host function.
type httpRequestBody struct {
	// Method is required. http.NewRequest would silently substitute GET for an
	// empty method, and a plugin that did not say what it wants to do must not
	// have a verb chosen for it.
	Method string `json:"method"`
	// URL is required and must be http or https.
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// httpResponseBody is the JSON response of the http_request host function.
type httpResponseBody struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body"`
	// Truncated reports that the upstream body was longer than
	// httpResponseByteLimit and Body holds only its first bytes. It is stated
	// rather than hidden, so a plugin never parses a clipped body as complete.
	Truncated bool `json:"truncated,omitempty"`
}

// httpRequest implements "http_request(ptr i32, len i32) -> i64".
//
// The http capability being granted is only the first check: this function
// also refuses any URL whose host is not in Grant.AllowedHosts and any scheme
// other than http/https, returning CodeDenied and publishing a
// plugin/call_failed{category=denied} event.
func (h hostCalls) httpRequest(ctx context.Context, m api.Module, ptr, length uint32) uint64 {
	raw, err := readGuestBytes(m, ptr, length)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("http_request: %v", err))
	}
	var req httpRequestBody
	if err := json.Unmarshal(raw, &req); err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("http_request: decode request: %v", err))
	}
	if req.URL == "" {
		return h.writeError(ctx, m, CodeInvalidRequest, "http_request: url must not be empty")
	}
	if req.Method == "" {
		return h.writeError(ctx, m, CodeInvalidRequest, "http_request: method must not be empty")
	}
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("http_request: parse url %q: %v", req.URL, err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return h.deny(ctx, m, funcHTTPRequest,
			fmt.Sprintf("scheme %q is not reachable through the http capability", parsed.Scheme))
	}
	if !h.grant.HostAllowed(parsed.Hostname()) {
		return h.deny(ctx, m, funcHTTPRequest,
			fmt.Sprintf("host %q is not in this plugin's allowed_hosts", parsed.Hostname()))
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("http_request: build request: %v", err))
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := h.deps.HTTP.Do(httpReq)
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("http_request %s %s: %v", req.Method, req.URL, err))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			h.deps.Logger.Warn("plugin http_request: closing the response body failed",
				"plugin", h.deps.PluginName, "url", req.URL, "error", cerr)
		}
	}()
	// One byte past the limit distinguishes "exactly at the limit" from
	// "clipped", so Truncated states the truth instead of guessing.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, httpResponseByteLimit+1))
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("http_request %s %s: read response body: %v",
			req.Method, req.URL, err))
	}
	truncated := len(payload) > httpResponseByteLimit
	if truncated {
		payload = payload[:httpResponseByteLimit]
	}
	return h.writeJSON(ctx, m, httpResponseBody{
		Status:    resp.StatusCode,
		Headers:   resp.Header,
		Body:      string(payload),
		Truncated: truncated,
	})
}

// readFile implements "read_file(ptr i32, len i32) -> i64" with a JSON request
// {"path":string} and a JSON response {"path":string,"content":string}.
//
// The path goes through two checks, in this order: port.WorkspacePathGuard —
// the repository's single path boundary, which resolves symlinks and rejects
// the Windows device-name and alternate-data-stream spellings — and then
// Grant.AllowedPaths. Either refusal is CodeDenied plus a
// plugin/call_failed{category=denied} event.
func (h hostCalls) readFile(ctx context.Context, m api.Module, ptr, length uint32) uint64 {
	raw, err := readGuestBytes(m, ptr, length)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("read_file: %v", err))
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("read_file: decode request: %v", err))
	}
	if req.Path == "" {
		return h.writeError(ctx, m, CodeInvalidRequest, "read_file: path must not be empty")
	}

	checked, err := h.deps.FS.Check(ctx, req.Path)
	if err != nil {
		return h.deny(ctx, m, funcReadFile,
			fmt.Sprintf("path %q rejected by the workspace guard: %v", req.Path, err))
	}
	if err := checkAllowedPath(ctx, h.grant, checked); err != nil {
		return h.deny(ctx, m, funcReadFile, err.Error())
	}

	content, err := os.ReadFile(checked)
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("read_file %q: %v", req.Path, err))
	}
	return h.writeJSON(ctx, m, struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: req.Path, Content: string(content)})
}

// callToolRequest is the JSON request of the call_tool host function.
//
// It deliberately has no risk-level field. Registry.Execute fills a call's risk
// level from the tool's own descriptor only when the call carries none, so a
// guest able to declare its own risk level could talk its way past the policy
// and approval gates that read it.
type callToolRequest struct {
	CallID    string            `json:"call_id,omitempty"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// callTool implements "call_tool(ptr i32, len i32) -> i64": the plugin asks the
// host to run one of the host's own registered tools.
//
// The call goes through tool.Registry.Execute like any other, so permissions,
// policy, guardrails, timeouts, sanitizing and audit all stay on the one path;
// the only thing added is the "plugin:<name>" call origin, which is what makes
// a plugin's calls distinguishable in the audit trail. The successful response
// is a JSON domain.ToolResult, including a result the tool itself marked as
// failed — that is the tool's answer, not a host failure.
func (h hostCalls) callTool(ctx context.Context, m api.Module, ptr, length uint32) uint64 {
	raw, err := readGuestBytes(m, ptr, length)
	if err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("call_tool: %v", err))
	}
	var req callToolRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("call_tool: decode request: %v", err))
	}
	if req.Tool == "" {
		return h.writeError(ctx, m, CodeInvalidRequest, "call_tool: tool must not be empty")
	}

	callCtx := tool.WithCallOrigin(ctx, "plugin:"+h.deps.PluginName)
	result, err := h.deps.Tools.Execute(callCtx, h.deps.Agent, domain.ToolCall{
		ID:        req.CallID,
		Name:      req.Tool,
		Arguments: req.Arguments,
	})
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("call_tool %q: %v", req.Tool, err))
	}
	return h.writeJSON(ctx, m, result)
}

// checkAllowedPath reports whether path is inside one of the grant's
// allowed_paths, returning an error describing the refusal when it is not.
//
// Containment is decided by port.WorkspacePathGuard — the same check that
// guards the workspace boundary, rooted at each allowed path in turn — rather
// than by a lexical prefix test. That is what makes allowed_paths hold under
// symlinks: a link inside an allowed directory pointing at a file elsewhere in
// the workspace is spelled entirely inside the allowlist, so a lexical test
// would let the plugin read a file the allowlist excludes. Rooting the guard at
// the allowed path resolves both sides and refuses it.
//
// An empty allowlist denies everything (an fs grant with no allowed_paths is a
// plugin that may call read_file and reach nothing), and so does a malformed
// empty entry — filepath.Clean("") is ".", which would silently widen the grant
// to the process working directory.
func checkAllowedPath(ctx context.Context, g perm.Grant, path string) error {
	if len(g.AllowedPaths) == 0 {
		return fmt.Errorf("path %q is refused: this plugin has no allowed_paths", path)
	}
	for _, allowed := range g.AllowedPaths {
		if allowed == "" {
			continue
		}
		if _, err := port.NewWorkspacePathGuard(allowed).Check(ctx, path); err == nil {
			return nil
		}
	}
	return fmt.Errorf("path %q is not inside any of this plugin's allowed_paths %v", path, g.AllowedPaths)
}

// namespacedKey qualifies a guest-supplied kv key with the plugin's own
// namespace. The guest never sees or supplies the prefix, so it cannot escape
// its namespace by spelling one.
func (h hostCalls) namespacedKey(key string) string {
	return "plugin/" + h.deps.PluginName + "/" + key
}

// deny refuses a call on capability-argument grounds: it publishes the
// plugin/call_failed{category=denied} event, records the refusal in the host
// log, and returns the CodeDenied envelope to the guest.
func (h hostCalls) deny(ctx context.Context, m api.Module, hostFunc, reason string) uint64 {
	h.publishDenial(ctx, hostFunc, reason)
	return h.writeError(ctx, m, CodeDenied, fmt.Sprintf("%s: %s", hostFunc, reason))
}

// publishDenial emits the denial telemetry. The category travels inside the
// message because domain.RuntimeEvent has no category field; TaskID is left
// empty on purpose — a host module belongs to a plugin for its whole lifetime,
// so there is no one task to attribute its denials to, and the plugin name is
// the subject that matters.
//
// A telemetry failure cannot be returned anywhere (a host function's caller is
// the guest, and the guest is being refused either way), so it is logged at
// Error rather than dropped: a denial that never reached the event stream must
// still be findable.
func (h hostCalls) publishDenial(ctx context.Context, hostFunc, reason string) {
	h.deps.Logger.Warn("plugin host call denied",
		"plugin", h.deps.PluginName, "host_function", hostFunc, "reason", reason)
	event := domain.RuntimeEvent{
		Type: RuntimeEventCallFailed,
		Message: fmt.Sprintf("category=%s plugin=%s host_function=%s reason=%s",
			CategoryDenied, h.deps.PluginName, hostFunc, reason),
		CreatedAt: time.Now(),
	}
	if err := h.deps.Events.Publish(ctx, event); err != nil {
		h.deps.Logger.Error("plugin host call denial event was not published",
			"plugin", h.deps.PluginName, "host_function", hostFunc, "error", err)
	}
}

// writeError encodes an error envelope and hands it to the guest.
func (h hostCalls) writeError(ctx context.Context, m api.Module, code, message string) uint64 {
	return h.writeJSON(ctx, m, errorBody{Code: code, Message: message})
}

// writeJSON encodes value and hands it to the guest.
//
// An encoding failure is a bug in this package's own response types, not a
// runtime condition: there is no honest value to return in its place (an empty
// body means "no result" on the wire), so it panics, which wazero surfaces as
// a failed guest call rather than as a crashed host.
func (h hostCalls) writeJSON(ctx context.Context, m api.Module, value any) uint64 {
	body, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("host: encode %T response for plugin %q: %v", value, h.deps.PluginName, err))
	}
	return writeGuestBody(ctx, m, body)
}

// readGuestBytes copies length bytes at ptr out of the calling guest's linear
// memory.
//
// The copy is not optional: the returned slice would otherwise alias guest
// memory, which the guest can grow (and therefore move) or reuse during the
// host work that follows.
func readGuestBytes(m api.Module, ptr, length uint32) ([]byte, error) {
	if length == 0 {
		// An empty body is a legal thing for a guest to pass; whether it is
		// acceptable for a given host function is that function's business.
		return nil, nil
	}
	mem := m.Memory()
	if mem == nil {
		return nil, fmt.Errorf("read %d bytes at %d: calling guest has no linear memory", length, ptr)
	}
	view, ok := mem.Read(ptr, length)
	if !ok {
		return nil, fmt.Errorf("read %d bytes at %d: out of range", length, ptr)
	}
	out := make([]byte, len(view))
	copy(out, view)
	return out, nil
}

// writeGuestBody hands body back to the calling guest: it allocates through the
// guest's own plugin_alloc export (the only allocator that pins memory the
// guest will later free), writes the bytes there, and returns the packed
// (ptr, len) result.
//
// It is a plain function, not a host function, because a host function must
// never call another host function — wazero loses memory access when it does.
//
// Every failure here panics. A host function that cannot deliver its result has
// no way to say so: returning PackResult(0, 0) would tell the guest "the call
// succeeded and produced nothing", which for a function that must return a body
// is a lie the guest would act on. wazero converts the panic into a failed
// guest call, so the plugin's caller sees a trapped invocation and the Instance
// is marked dead.
func writeGuestBody(ctx context.Context, m api.Module, body []byte) uint64 {
	if len(body) == 0 {
		// The ABI's contract-legal "no return body" (abi.PackResult docs).
		return abi.PackResult(0, 0)
	}
	alloc := m.ExportedFunction(abi.ExportAlloc)
	if alloc == nil {
		panic(fmt.Sprintf("host: calling guest exports no %s; cannot return a %d-byte result",
			abi.ExportAlloc, len(body)))
	}
	res, err := alloc.Call(ctx, uint64(len(body)))
	if err != nil {
		panic(fmt.Sprintf("host: guest %s(%d) failed: %v", abi.ExportAlloc, len(body), err))
	}
	if len(res) == 0 || res[0] == 0 {
		panic(fmt.Sprintf("host: guest %s(%d) returned a null pointer", abi.ExportAlloc, len(body)))
	}
	ptr := uint32(res[0])
	mem := m.Memory()
	if mem == nil {
		panic("host: calling guest has no linear memory; cannot return a result")
	}
	if !mem.Write(ptr, body) {
		panic(fmt.Sprintf("host: writing a %d-byte result at %d is out of range", len(body), ptr))
	}
	return abi.PackResult(ptr, uint32(len(body)))
}
