package host

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/tool"
)

// observeTimeout bounds ONE notification to ONE plugin.
//
// It is short because it is spent by the tool call that is already finished:
// the caller is holding its result, waiting for observers to be told about it.
// A plugin that wants to do something slow with an observation must do it
// after answering, on its own time — this seam gives it a fifth of a second to
// take the notification, not to act on it.
//
// It is a constant rather than a setting because there is no deployment for
// which a longer bound is the right answer: an observer is a listener, and a
// listener that needs seconds is a design mistake this value makes visible
// instead of hiding.
const observeTimeout = 200 * time.Millisecond

// guestToolObservation is the JSON the host hands a guest for
// abi.OpObserveToolResult.
//
// It carries the call AND its result together, because an observer that only
// saw results would have to correlate them itself against a call it was never
// told about. Field names mirror guestToolCall and domain.ToolResult so a
// plugin author reads one vocabulary across all three directions.
type guestToolObservation struct {
	CallID    string            `json:"call_id,omitempty"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments,omitempty"`
	Success   bool              `json:"success"`
	Output    string            `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
}

// pluginObserver notifies one plugin, read-only, after a tool call answered.
//
// Everything about it is one-way. The guest's answer is READ AND DISCARDED —
// not because reading it is expensive, but because a returned value invites
// exactly one question ("could a plugin change the result?") whose answer must
// stay "no". A failure is reported to the plugin's own health (OnFault, see
// G1) and nowhere else: the caller already has its result, and a broken
// observer must not be able to affect it.
type pluginObserver struct {
	deps  Deps
	guest guestCaller
}

// newPluginObserver builds the observer registered on the tool registry when a
// deployment grants the observe extension.
func newPluginObserver(deps Deps, guest guestCaller) tool.Observer {
	return &pluginObserver{deps: deps, guest: guest}
}

// Observe implements tool.Observer.
func (o *pluginObserver) Observe(ctx context.Context, call domain.ToolCall, result domain.ToolResult) {
	request, err := json.Marshal(guestToolObservation{
		CallID:    call.ID,
		Tool:      call.Name,
		Arguments: call.Arguments,
		Success:   result.Success,
		Output:    result.Output,
		Error:     result.Error,
	})
	if err != nil {
		// Unreachable for these field types, and still not dropped: an
		// observation nobody could encode is a fault of this package, and
		// silence would make it invisible.
		o.report(ctx, fmt.Errorf("encode observation of tool %q for plugin %q: %w",
			call.Name, o.deps.PluginName, err), call.Name)
		return
	}

	// Its own deadline, derived from a context with the caller's cancellation
	// still in force: a caller who walked away should not be waited on, but a
	// caller who is merely slow must not lend this notification more time than
	// observeTimeout.
	notifyCtx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	if _, err := o.guest.call(notifyCtx, abi.OpObserveToolResult, request); err != nil {
		o.report(ctx, fmt.Errorf("notify plugin %q of tool %q: %w", o.deps.PluginName, call.Name, err), call.Name)
	}
	// Whatever the guest answered is discarded here, deliberately. See the
	// type's doc comment.
}

// report routes a failed notification to the plugin's health counter and the
// event stream, exactly as a failed tool call does — an observer that keeps
// timing out is a plugin that keeps failing, and G1's consecutive-fault
// counter is what eventually unloads it.
//
// It never returns anything: there is no caller to tell. That is the whole
// point of this seam.
func (o *pluginObserver) report(ctx context.Context, err error, toolName string) {
	category, isFault := ClassifyCallFault(ctx, err)
	if !isFault {
		return
	}
	if o.deps.OnFault != nil {
		o.deps.OnFault(ctx, category, toolName, err.Error())
	}
	publishCallFailed(ctx, o.deps.PluginName, o.deps, category, "observe:"+toolName, err.Error())
}
