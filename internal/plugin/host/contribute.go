package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/tool"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// gateableLabel is the ledger label a tool's gateable-catalog entry is filed
// under. It is spelled in one place so a ledger snapshot — the answer to
// "the plugin loaded, so what did it actually leave behind?" — and this file can
// never disagree.
func gateableLabel(toolName string) string { return "gateable:" + toolName }

// pluginCallOrigin renders the audit call origin of work a plugin drives:
// "plugin:<name>". It is the single spelling of that format, shared with the
// call_tool host function (hostCalls.callTool), because a forensic pass over the
// audit trail selects on it.
func pluginCallOrigin(pluginName string) string { return "plugin:" + pluginName }

// guestCaller runs one operation against a plugin's guest, on an instance the
// call owns exclusively for its whole duration.
//
// It is an interface for two reasons. It names what a contributed tool's handler
// may do with the instance pool — one call, and the acquire/release discipline
// kept for it (see pool.call) — rather than handing every handler the pool's
// panic-happy contract to keep itself. And it is the seam a test substitutes
// (guestCallerFunc, in contribute_test.go): what the handler adds around a guest
// call is a ctx marking and a JSON contract, and driving those through wazero
// would prove less about them, not more. *pool is the only production
// implementation.
type guestCaller interface {
	call(ctx context.Context, op int32, in []byte) ([]byte, error)
}

// guestToolCall is the JSON request the host hands a guest for abi.OpCallTool.
//
// It is deliberately the mirror image of callToolRequest, the guest→host
// direction of the same idea (see hostcall.go): the same field names, so a
// plugin author reads one shape whichever way a tool call travels. CallID is
// sent so a guest can correlate its own logs with the host's; it is not what the
// host reads back — see pluginToolHandler.
type guestToolCall struct {
	CallID    string            `json:"call_id,omitempty"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// contributeTools registers every tool spec.Tools claims, filing everything it
// files under the ONE owner it is handed — which in an activation is the
// contribution-side owner (ToolsOwner), never the instance owner: that is what
// makes withdrawing a plugin's contributions a single DisposeOwner that cannot
// reach its runtime or its pool (see Plugin.Suspend). Each tool is three
// things:
//
//  1. the tool enters spec.Registry, so the model can call it
//     (tool.RegisterOwned);
//  2. its name enters the gateable catalog (toolauth.Contribute), which is what
//     a per-agent disabled_tools list resolves against. Without this step the
//     tool is callable but no agent config can disable it — an authorization
//     bypass, not a missing row in the config UI;
//  3. the handler marks its context with the plugin's call origin, so everything
//     the guest drives from inside the call — a tool call back through call_tool
//     — is attributed to the plugin in the audit trail rather than to the agent.
//
// keep receives the one-shot revoke handle of every ledger entry filed here, in
// filing order, so the caller can roll them back. It is called as each entry is
// filed rather than once at the end, because this function's failure mode is a
// PANIC: a tool name already taken is fail-loud in the registry and in the
// gateable catalog alike (neither can be shared by two contributors), so a
// contribution can die half-way through. What was already filed must still be
// revocable then — Activate's rollback runs on the way out of the panic and
// revokes exactly what keep collected.
//
// keep must be non-nil, and that is checked before anything is contributed: a
// nil keep means every handle this function produces is dropped, so a panic
// part-way through the list would leak exactly what keep exists to make
// revocable. It is a programming error with no sensible default, so it panics
// here — the stance newPool takes on a nil factory and lifecycle.Ledger.Add on a
// nil dispose — rather than nil-dereferencing at the first tool with a message
// that names none of this.
func contributeTools(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	spec Spec,
	guest guestCaller,
	keep func(revoke func() error),
) {
	if keep == nil {
		panic(fmt.Sprintf("host: contributeTools: keep is nil for plugin %q; the revoke handles of every "+
			"contributed tool would be dropped, so a panicking contribution could not be rolled back", spec.Name))
	}

	// The observe extension, when the deployment granted it: one registration
	// per plugin, filed under the SAME contribution owner the tools are, so a
	// suspend or an unload takes the plugin off this seam together with its
	// tools. A plugin whose tools were withdrawn but which kept watching every
	// call would be a plugin the deployment believes it has disabled.
	//
	// Not granted means not registered — there is nothing in the host to call,
	// which is the same shape as an ungranted capability being absent from the
	// host module rather than refusing at call time.
	if spec.Extensions.Observe {
		keep(tool.ObserveOwned(ledger, owner, spec.Registry, "plugin:"+spec.Name,
			newPluginObserver(spec.Deps, guest)))
	}

	// The decide extension, on the same terms — and it is the seam where "not
	// granted means not registered" stops being a nicety: a registered decider
	// can REFUSE the agent's tool calls, so a plugin that was never granted it
	// must not be reachable from the dispatch path at all.
	if spec.Extensions.Decide {
		keep(tool.DecideOwned(ledger, owner, spec.Registry, "plugin:"+spec.Name,
			newPluginDecider(spec.Deps, guest)))
	}

	for _, descriptor := range spec.Tools {
		handler := pluginToolHandler(spec.Name, spec.Deps, descriptor.Name, guest, spec.Deps.OnFault)
		keep(tool.RegisterOwned(ledger, owner, spec.Registry, descriptor, handler))

		// Step 2, and it is not optional: without it the tool is callable and
		// ungateable, so a per-agent disabled_tools list naming it is rejected as
		// an unknown tool and the tool stays reachable for every agent.
		undo := toolauth.Contribute(toolauth.GateableTool{
			Name:        descriptor.Name,
			Description: descriptor.Description,
		})
		keep(ledger.Add(owner, gateableLabel(descriptor.Name), func() error {
			undo()
			return nil
		}))
	}
}

// faultReporter is how a tool handler tells its owner that a call failed in a
// way that counts toward the plugin's health: category is one of
// CategoryTimeout / CategoryTrap / CategoryABI (never CategoryDenied — that
// one travels through the host-function path and means the plugin overstepped
// rather than broke), and reason is the error text.
//
// It is a plain function rather than an interface because there is exactly one
// production implementation (the Loader's counter) and it is CONTRACT-DECLARED
// OPTIONAL: a nil reporter means nobody is counting, which is what an embedder
// mounting a plugin outside the Loader gets.
type faultReporter func(ctx context.Context, category, toolName, reason string)

// pluginToolHandler builds the handler behind one contributed tool: it hands the
// call to the plugin's guest and reads the guest's answer back as a
// domain.ToolResult — and, when any of that fails in a way that counts against
// the plugin, reports the fault twice over.
//
// toolName is the descriptor's name, not call.Name: the registry dispatches by
// name, so the two are equal, and taking the authoritative one means the guest
// is always asked for the tool this handler was registered as.
//
// Twice, because the two consumers need different things and neither can be
// derived from the other: report feeds the Loader's consecutive-fault counter
// (a decision), while the plugin/call_failed event feeds the audit trail (a
// record). A failure that only counted would be invisible to whoever is
// reading events; one that was only published would never unload anything.
//
// A FAILED TOOL RESULT is not a fault. A guest answering {"success":false} did
// its job and said no — counting that would unload a plugin for behaving
// exactly as designed. Only failures ClassifyCallFault recognises count, which
// deliberately excludes a caller's own cancellation.
func pluginToolHandler(pluginName string, deps Deps, toolName string, guest guestCaller, report faultReporter) tool.Handler {
	return tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		// fail is the single exit for every failure of this call, so no path
		// can forget to account for one.
		fail := func(err error) (domain.ToolResult, error) {
			category, isFault := ClassifyCallFault(ctx, err)
			if !isFault {
				return domain.ToolResult{}, err
			}
			if report != nil {
				report(ctx, category, toolName, err.Error())
			}
			publishCallFailed(ctx, pluginName, deps, category, "tool:"+toolName, err.Error())
			return domain.ToolResult{}, err
		}

		// Step 3: mark the ctx BEFORE the guest is entered. Everything the guest
		// reaches from inside this call — a tool call back through call_tool —
		// runs under it, and that marking is what keeps the plugin's own calls
		// distinguishable from the agent's in the audit trail.
		ctx = tool.WithCallOrigin(ctx, pluginCallOrigin(pluginName))

		request, err := json.Marshal(guestToolCall{
			CallID:    call.ID,
			Tool:      toolName,
			Arguments: call.Arguments,
		})
		if err != nil {
			// Unreachable for these field types (strings and a map of strings),
			// and still not swallowed: a request the host cannot encode is not a
			// call that was made, and answering with an empty result would report
			// it as a tool that ran and produced nothing.
			return fail(fmt.Errorf("encode call of plugin %q tool %q: %w",
				pluginName, toolName, err))
		}
		body, err := guest.call(ctx, abi.OpCallTool, request)
		if err != nil {
			return fail(fmt.Errorf("call plugin %q tool %q: %w", pluginName, toolName, err))
		}
		result, err := decodeToolResult(body)
		if err != nil {
			// A body nobody can decode is an ABI violation: the guest answered
			// something that is not the document this op's contract names.
			return fail(fmt.Errorf("call plugin %q tool %q: %w: %w",
				pluginName, toolName, err, ErrGuestABI))
		}
		// The correlation id belongs to the host. The guest is told which call it
		// is answering (see guestToolCall) but never gets to choose it, so a
		// plugin cannot attach its answer to somebody else's call.
		result.CallID = call.ID
		// The plugin answered, so its consecutive-fault count starts over. A
		// failed RESULT counts as answering: the tool said no, which is the
		// plugin working.
		if deps.OnSuccess != nil {
			deps.OnSuccess(ctx, toolName)
		}
		return result, nil
	})
}

// decodeToolResult reads a guest's answer to abi.OpCallTool.
//
// Decoding is strict, and every failure is an error rather than an empty
// ToolResult: an answer nobody could read, handed on as a zero value, would
// reach the model as a tool that succeeded and produced nothing.
//
//   - An empty body is not an answer: abi's contract for this op is a JSON
//     document, and Instance.Invoke returns (nil, nil) whenever the guest's
//     packed result length is 0, so this is reachable in production rather than
//     theoretical.
//   - Unknown fields are refused, because a body of the wrong shape decodes into
//     a zero ToolResult without complaint — a guest answering {"result":…} would
//     otherwise look like a successless success.
//   - Trailing data is refused for the same reason: silently taking the first of
//     two documents is a choice made on the guest's behalf.
//   - A result marked unsuccessful with no Error is refused: "it failed", with no
//     reason, is the one answer nothing downstream can act on.
func decodeToolResult(body []byte) (domain.ToolResult, error) {
	if len(body) == 0 {
		return domain.ToolResult{}, errors.New("guest returned no body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var result domain.ToolResult
	if err := decoder.Decode(&result); err != nil {
		return domain.ToolResult{}, fmt.Errorf("decode tool result %s: %w", quoteForError(body), err)
	}
	if decoder.More() {
		return domain.ToolResult{}, fmt.Errorf("decode tool result %s: trailing data after the JSON document",
			quoteForError(body))
	}
	if !result.Success && result.Error == "" {
		return domain.ToolResult{}, fmt.Errorf("guest reported a failed call with no reason: %s", quoteForError(body))
	}
	return result, nil
}
