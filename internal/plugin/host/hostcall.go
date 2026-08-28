package host

import (
	"context"
	"encoding/json"
	"errors"
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
// travels inside the event message; formatCallFailedMessage and
// EventHasCategory are the only two places that know how.
const RuntimeEventCallFailed = "plugin/call_failed"

// CategoryDenied is the plugin/call_failed category for a call a host function
// refused on capability or allowlist grounds (design doc's error taxonomy). It
// is counted separately from plugin faults: a denial means the plugin
// overstepped, not that it is broken.
const CategoryDenied = "denied"

// CategoryTimeout, CategoryTrap and CategoryABI are the remaining
// plugin/call_failed categories from the design doc's error taxonomy
// (legion-plugin-system.md §6.9).
//
// They differ from CategoryDenied in kind, not degree: a denial means the
// plugin asked for something it was not authorized to have — it is working
// exactly as written, just beyond its grant — while all three of these mean
// the plugin failed to answer at all. That is why these three count toward a
// plugin's health and CategoryDenied does not.
const (
	CategoryTimeout = "timeout"
	CategoryTrap    = "trap"
	CategoryABI     = "abi"
)

// eventCategoryToken renders a failure category as the token it travels as
// inside a plugin/call_failed message. It is the ONE place the encoding is
// spelled.
//
// The category is not a field of its own because domain.RuntimeEvent has none:
// internal/domain/types.go gives it Type/TaskID/Message/token counters/ElapsedMs
// and no metadata map, and adding one would change the persisted runtime_events
// schema. Until that changes, every emitter goes through
// formatCallFailedMessage and every consumer (a denial counter, a test) through
// EventHasCategory, so the format has a single definition to change.
func eventCategoryToken(category string) string {
	return "category=" + category
}

// formatCallFailedMessage renders the message of a plugin/call_failed event,
// with the failure category leading it (see eventCategoryToken).
func formatCallFailedMessage(category, plugin, hostFunc, reason string) string {
	return fmt.Sprintf("%s plugin=%s host_function=%s reason=%s",
		eventCategoryToken(category), plugin, hostFunc, reason)
}

// EventHasCategory reports whether event is a plugin/call_failed event of the
// given failure category (CategoryDenied and the like).
//
// It is the supported way to read a category back: see formatCallFailedMessage
// for why the category lives in the message text rather than in a field.
func EventHasCategory(event domain.RuntimeEvent, category string) bool {
	if event.Type != RuntimeEventCallFailed {
		return false
	}
	return strings.HasPrefix(event.Message, eventCategoryToken(category)+" ")
}

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

// readFileByteLimit caps how much of a file read_file copies into guest memory.
// It exists for the same reason httpResponseByteLimit does, and answers the same
// way: the response states that it was truncated instead of leaving the guest
// with a clipped body it would parse as complete.
const readFileByteLimit = 1 << 20

// httpMaxRedirects caps the redirect chain http_request will follow, matching
// the cap internal/tool/web.go applies to the agent's own web tool. A chain
// this long is a misbehaving upstream, and each extra hop is another host to
// re-authorize.
const httpMaxRedirects = 10

// callToolDepthCap bounds how deep ONE chain of plugin-initiated tool calls may
// go: a plugin calls call_tool, the tool it reaches is another plugin's tool, that
// guest calls call_tool again. The depth accumulates across the chain (it travels
// on the context, see withCallToolDepth), so a chain cannot reset it by taking one
// more hop.
//
// It is a hard ceiling with no config toggle, for the same reason the agent's own
// tool loop cap is one: a recursion a plugin drives has no round budget of its own
// to run out of.
const callToolDepthCap = 3

// callToolDepthKey is the context key carrying how many call_tool frames are
// already on the stack beneath the current one.
//
// It is unexported and travels on the context rather than in Deps because it is
// per-CALL, not per-plugin: one host module serves every call the plugin makes,
// and a chain that hops through another plugin's tool has to keep counting across
// the hop. A field on hostCalls would be shared by unrelated concurrent calls.
type callToolDepthKey struct{}

// withCallToolDepth marks ctx as being depth levels deep in a call_tool chain.
func withCallToolDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, callToolDepthKey{}, depth)
}

// callToolDepthFrom reports how many call_tool frames the current call is nested
// under.
//
// An unmarked context is depth 0, and that is a contract-legal absence rather
// than a fallback: nothing has entered call_tool yet, so there is no frame to
// count. Every nested call is reached through withCallToolDepth, so a chain can
// only be undercounted if a hop drops the context entirely — which would also
// drop the shared budget and the call origin, and callTool refuses a call whose
// budget is missing.
func callToolDepthFrom(ctx context.Context) int {
	depth, ok := ctx.Value(callToolDepthKey{}).(int)
	if !ok {
		return 0
	}
	return depth
}

// errRedirectDenied is the sentinel a grant-aware CheckRedirect returns when a
// redirect would leave Grant.AllowedHosts. net/http wraps it in a *url.Error,
// whose Unwrap makes it reachable with errors.Is, which is how httpRequest tells
// a refused hop apart from a transport failure and reports it as a denial rather
// than as a host error.
var errRedirectDenied = errors.New("redirect target is not authorized by this plugin's grant")

// redirectGuardedClient returns a copy of client whose redirect policy
// re-validates every hop against g.
//
// The allowlist check on the request URL is not enough on its own: Go's client
// follows redirects to ARBITRARY hosts, so an allowed host answering
// "302 Location: http://169.254.169.254/…" would otherwise fetch an internal
// address and hand its body to the guest, with no denial and no event. The
// allowlist is this module's gate, so the module installs the check rather than
// trusting the injected client to carry it (internal/tool/web.go:280 does the
// same per-hop revalidation for the agent's web tool).
//
// The copy is shallow and deliberate: the caller's client keeps its own policy
// (a deployment's SSRF-guarded transport and timeout are copied along, and its
// own CheckRedirect is called after this one rather than replaced, so nothing a
// deployment configured is silently dropped).
func redirectGuardedClient(g perm.Grant, client *http.Client) *http.Client {
	guarded := *client
	inherited := client.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= httpMaxRedirects {
			// Wrapped in errRedirectDenied like the two refusals below it: stopping
			// at the cap is this host's REFUSAL of the plugin's request, and an
			// unwrapped error would reach the guest as HOST_ERROR with no denial
			// event, indistinguishable from an unreachable upstream.
			return fmt.Errorf("%w: stopped after %d redirects", errRedirectDenied, httpMaxRedirects)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("%w: scheme %q is not reachable through the http capability",
				errRedirectDenied, req.URL.Scheme)
		}
		if !g.HostAllowed(req.URL.Hostname()) {
			return fmt.Errorf("%w: host %q is not in this plugin's allowed_hosts",
				errRedirectDenied, req.URL.Hostname())
		}
		if inherited != nil {
			return inherited(req, via)
		}
		return nil
	}
	return &guarded
}

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
// its own failures (an unreadable message region, an empty message, an unknown
// level) are reported to the host logger at Error rather than silently dropped.
func (h hostCalls) log(_ context.Context, m api.Module, level, ptr, length uint32) {
	logger := h.deps.Logger.With("plugin", h.deps.PluginName)
	message, err := readGuestBytes(m, ptr, length)
	if err != nil {
		logger.Error("plugin log call: message region is unreadable", "error", err)
		return
	}
	if len(message) == 0 {
		// readGuestBytes maps an empty region to no bytes, which is a legal thing
		// for a guest to pass but not a thing this function can do anything with:
		// emitting a blank line at the requested level would hide the broken
		// caller behind what looks like a log entry.
		logger.Error("plugin log call: the guest passed no message", "level", level)
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
// plugin/call_failed{category=denied} event. A redirect leaving the allowlist is
// refused the same way, because the client BuildHostModule hands this function
// re-validates every hop (see redirectGuardedClient).
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
		// A refused redirect is a denial, not an upstream fault: the plugin asked
		// for something it is not authorized to have — a host outside its
		// allowed_hosts, a non-http scheme, or more hops than the cap allows — it
		// just asked via a Location header. The wrapped error says which of the
		// three it was. net/http returns the 3xx response alongside the error and
		// has already closed its body, so there is nothing to close here.
		if errors.Is(err, errRedirectDenied) {
			return h.deny(ctx, m, funcHTTPRequest,
				fmt.Sprintf("%s %s was refused while following redirects: %v",
					req.Method, req.URL, err))
		}
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
// {"path":string} and a JSON response
// {"path":string,"content":string,"truncated":bool}.
//
// The path goes through two checks, in this order: port.WorkspacePathGuard —
// the repository's single path boundary, which resolves symlinks and rejects
// the Windows device-name and alternate-data-stream spellings — and then
// Grant.AllowedPaths. Either refusal is CodeDenied plus a
// plugin/call_failed{category=denied} event. A call cancelled while those checks
// run is NOT a denial: it is reported as CodeHostError with no denial event, so a
// shutdown is never counted against the plugin.
//
// The content is capped at readFileByteLimit and truncation is stated, exactly
// as httpRequest states it: the guest chooses the path, so an unbounded
// os.ReadFile would let it allocate an arbitrary file in the host and then die
// on the guest-side allocator instead of being told what happened.
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
		if callWasCancelled(ctx, err) {
			return h.writeError(ctx, m, CodeHostError,
				fmt.Sprintf("read_file %q: the call was cancelled: %v", req.Path, err))
		}
		return h.deny(ctx, m, funcReadFile,
			fmt.Sprintf("path %q rejected by the workspace guard: %v", req.Path, err))
	}
	if err := checkAllowedPath(ctx, h.grant, checked); err != nil {
		if callWasCancelled(ctx, err) {
			return h.writeError(ctx, m, CodeHostError,
				fmt.Sprintf("read_file %q: the call was cancelled: %v", req.Path, err))
		}
		return h.deny(ctx, m, funcReadFile, err.Error())
	}

	file, err := os.Open(checked)
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("read_file %q: %v", req.Path, err))
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			h.deps.Logger.Warn("plugin read_file: closing the file failed",
				"plugin", h.deps.PluginName, "path", req.Path, "error", cerr)
		}
	}()
	// One byte past the limit distinguishes "exactly at the limit" from
	// "clipped", so Truncated states the truth instead of guessing.
	content, err := io.ReadAll(io.LimitReader(file, readFileByteLimit+1))
	if err != nil {
		return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("read_file %q: %v", req.Path, err))
	}
	truncated := len(content) > readFileByteLimit
	if truncated {
		content = content[:readFileByteLimit]
	}
	return h.writeJSON(ctx, m, struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		// Truncated reports that the file was longer than readFileByteLimit and
		// Content holds only its first bytes, so a plugin never reads a clipped
		// file as a whole one.
		Truncated bool `json:"truncated,omitempty"`
	}{Path: req.Path, Content: string(content), Truncated: truncated})
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
// The call must clear BOTH counters a plugin-initiated call is subject to, in
// this order:
//
//   - the per-chain recursion depth (callToolDepthCap), which travels on the
//     context and therefore accumulates across hops;
//   - the per-task tool budget SHARED with the model (tool.LoopBudget), keyed by
//     domain.GuardedToolName so both writers count the same string for the same
//     tool. A counter of the plugin's own would be a channel around the task's
//     total allowance.
//
// Depth is checked first so a chain past the cap is refused without spending
// allowance on a call that will never run. Either refusal — and an absent budget,
// which is broken wiring rather than "unlimited" — is a denial: CodeDenied plus
// the plugin/call_failed{category=denied} event.
//
// The budget only exists while a tool call is being dispatched (installed by
// internal/runtime's dispatchToolCall), so call_tool is task-time only: a guest
// that calls it from anywhere else — for example while answering abi.OpManifest
// during activation — will always be denied for lack of a budget.
//
// Past the counters the call goes through tool.Registry.Execute like any other, so
// permissions, policy, guardrails, timeouts, sanitizing and audit all stay on the
// one path; the only thing added is the "plugin:<name>" call origin, which is what
// makes a plugin's calls distinguishable in the audit trail. Any authorization-class
// refusal comes back as a denial too — the plugin overstepped — whether it is the
// tool policy (tool.ErrPermissionDenied) or a path guardrail rejecting an argument
// outside the workspace (port.ErrPathOutsideWorkspace); an input that fails the
// tool's own schema (tool.ErrInvalidInput) is CodeInvalidRequest, matching this
// function's own decode/field checks; everything else (an unresolvable tool name, a
// failing handler) is a host error. The successful response is a JSON
// domain.ToolResult, including a result the tool itself marked as failed — that is
// the tool's answer, not a host failure.
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
	call := domain.ToolCall{
		ID:        req.CallID,
		Name:      req.Tool,
		Arguments: req.Arguments,
	}
	// The name the counters use is the tool actually reached, not the wrapper it
	// may have been reached through: domain.GuardedToolName is the one function
	// both writers of the shared budget call, which is what makes it shared.
	guarded := domain.GuardedToolName(call)

	depth := callToolDepthFrom(ctx)
	if depth >= callToolDepthCap {
		return h.deny(ctx, m, funcCallTool, fmt.Sprintf(
			"tool %q would be level %d of one call chain, past the depth cap of %d",
			guarded, depth+1, callToolDepthCap))
	}
	budget, found := tool.LoopBudgetFrom(ctx)
	if !found {
		// Not "this task has no limit": nothing installed the task's shared budget
		// on the way here, and running the call uncounted is precisely the bypass
		// the shared counter exists to prevent. The controller's mandated response
		// to this state is still DENIED plus the denial event (kept below, same as
		// every other refusal in this function) — but unlike those, this one is not
		// the plugin overstepping: it is a dispatch path that starts tool calls
		// without installing tool.WithLoopBudget. Logged at Error, separately from
		// deny()'s own Warn, so it stays diagnosable as broken host wiring rather
		// than reading like ordinary plugin misbehaviour.
		h.deps.Logger.Error("plugin call_tool: no shared per-task tool budget on the context; "+
			"this is broken host wiring, not the plugin misbehaving",
			"plugin", h.deps.PluginName, "tool", guarded)
		return h.deny(ctx, m, funcCallTool, fmt.Sprintf(
			"tool %q cannot be called: this call carries no shared per-task tool budget, "+
				"so it could not be counted against the task's allowance", guarded))
	}
	if count, limit := budget.Record(guarded); count > limit {
		return h.deny(ctx, m, funcCallTool, fmt.Sprintf(
			"tool %q has been called %d times in this task, past the shared per-task cap of %d",
			guarded, count, limit))
	}

	callCtx := tool.WithCallOrigin(ctx, pluginCallOrigin(h.deps.PluginName))
	// The chain the tool reached from here belongs to: a tool that enters another
	// guest which calls call_tool again continues this count instead of restarting.
	callCtx = withCallToolDepth(callCtx, depth+1)
	result, err := h.deps.Tools.Execute(callCtx, h.deps.Agent, call)
	if err != nil {
		switch {
		case errors.Is(err, tool.ErrInvalidInput):
			// Authorized but malformed: the same class call_tool's own decode/field
			// checks above already report as CodeInvalidRequest, so a schema failure
			// inside Registry.Execute must land the same way — not as a host fault,
			// and not as a denial (nothing was refused on authorization grounds).
			return h.writeError(ctx, m, CodeInvalidRequest, fmt.Sprintf("call_tool %q: %v", guarded, err))
		case errors.Is(err, tool.ErrPermissionDenied), errors.Is(err, port.ErrPathOutsideWorkspace):
			// Both are the plugin overstepping, and neither must read like an
			// upstream fault: a policy refusal (tool.ErrPermissionDenied) and a
			// PathGuardrails refusal (port.ErrPathOutsideWorkspace, wrapped by
			// PathGuardrails.Before) are the same authorization-class refusal
			// read_file's own workspace-guard check already treats as a denial
			// (see readFile above) — without this an operator counting
			// plugin/call_failed{denied} would miss every plugin a path guardrail
			// turned away. Everything else — an unknown tool name, a broken
			// handler, a cancelled call — stays a host error, so the denial count
			// is not inflated by faults either.
			return h.deny(ctx, m, funcCallTool,
				fmt.Sprintf("tool %q was refused: %v", guarded, err))
		default:
			return h.writeError(ctx, m, CodeHostError, fmt.Sprintf("call_tool %q: %v", req.Tool, err))
		}
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
// to the process working directory. BuildHostModule rejects an empty entry
// outright, so a module built through it can never reach that skip; it stays here
// because this function must be fail-closed on its own terms.
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

// callWasCancelled reports whether a failure from the path checks is the
// ambient cancellation of the call rather than a refusal of the path.
//
// port.WorkspacePathGuard.Check returns ctx.Err() before it looks at the path at
// all, so without this distinction a shutdown mid-call would reach the guest as
// DENIED and be published as plugin/call_failed{category=denied} — a security
// refusal attributed to a plugin that did nothing wrong, and one more denial for
// whoever counts them. Everything that is NOT cancellation stays a denial,
// including a guard failure this host cannot classify: "cannot prove the path is
// allowed" is a refusal.
func callWasCancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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
// message (see formatCallFailedMessage) because domain.RuntimeEvent has no
// category field; TaskID is left empty on purpose — a host module belongs to a plugin for its whole lifetime,
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
		Type:      RuntimeEventCallFailed,
		Message:   formatCallFailedMessage(CategoryDenied, h.deps.PluginName, hostFunc, reason),
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
