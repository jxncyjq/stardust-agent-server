package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/tool"
)

// guestToolDecisionRequest is the JSON the host hands a guest for
// abi.OpDecideToolCall: the call it is about to dispatch.
//
// It is deliberately the same shape as guestToolCall — a plugin author reads
// one document whether the host is asking "run this" or "may I run this".
// There is no result to send: the whole point of this seam is that it is
// consulted BEFORE anything ran.
type guestToolDecisionRequest struct {
	CallID    string            `json:"call_id,omitempty"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// guestToolDecision is the guest's answer.
//
// Decision carries the same vocabulary the host's own policy uses ("allow" /
// "deny" / "ask"), so the two compose without a translation table that could
// drift. "ask" means a human must approve the call before it runs; the
// suspend that makes that possible happens a layer above, at the round
// boundary (see internal/manualgate).
// Reason reaches the model and the operator in the refusal, which is why a
// deny with no reason is completed with a placeholder rather than passed on
// as an empty string.
type guestToolDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// pluginDecider asks one plugin whether a tool call may run.
//
// EVERY FAILURE IS A REFUSAL. A timeout, a trap, an answer nobody can decode,
// a decision word this host does not know — all of them deny the call and
// count a fault against the plugin's health.
//
// This is the fail-closed choice from the G4 spec, and the argument for it is
// short: fail-open would make the control something an attacker switches off
// by making the plugin hang or crash, which is not a control. The cost — a
// broken plugin refusing calls — is BOUNDED by G1: consecutive faults reach
// the threshold and the plugin is unloaded automatically, after which nothing
// refuses anything. That boundedness is what makes fail-closed affordable,
// and it is why the fault must be reported here rather than swallowed.
type pluginDecider struct {
	deps  Deps
	guest guestCaller
}

// newPluginDecider builds the decider registered on the tool registry when a
// deployment grants the decide extension.
func newPluginDecider(deps Deps, guest guestCaller) tool.Decider {
	return &pluginDecider{deps: deps, guest: guest}
}

// Decide implements tool.Decider.
//
// It does NOT set its own deadline: the registry already bounds each
// consultation (tool.deciderMaxTimeout, and a quarter of the tool's own
// timeout when that is smaller), and adding a second bound here would either
// duplicate that number in two places or quietly override the tighter one.
func (d *pluginDecider) Decide(ctx context.Context, call domain.ToolCall) tool.Verdict {
	request, err := json.Marshal(guestToolDecisionRequest{
		CallID:    call.ID,
		Tool:      call.Name,
		Arguments: call.Arguments,
	})
	if err != nil {
		// Unreachable for these field types, and still a refusal: a question
		// the host could not even ask has not been answered.
		return d.refuse(ctx, call.Name, fmt.Errorf("encode decision request for plugin %q tool %q: %w",
			d.deps.PluginName, call.Name, err))
	}

	body, err := d.guest.call(ctx, abi.OpDecideToolCall, request)
	if err != nil {
		return d.refuse(ctx, call.Name, fmt.Errorf("ask plugin %q about tool %q: %w",
			d.deps.PluginName, call.Name, err))
	}
	decision, err := decodeToolDecision(body)
	if err != nil {
		return d.refuse(ctx, call.Name, fmt.Errorf("ask plugin %q about tool %q: %w: %w",
			d.deps.PluginName, call.Name, err, ErrGuestABI))
	}

	// The plugin answered, so its consecutive-fault count starts over. A DENY
	// counts as answering: refusing is the plugin working, exactly as a failed
	// tool result is.
	if d.deps.OnSuccess != nil {
		d.deps.OnSuccess(ctx, call.Name)
	}
	return decision
}

// refuse turns a failed consultation into a denial, reporting it to the
// plugin's health counter and the event stream on the way — the same two
// consumers a failed tool call has, and for the same reason: one feeds a
// decision (unload it), the other feeds the record (what happened).
//
// A caller's own cancellation is NOT a fault (ClassifyCallFault excludes it)
// but is still a refusal: there is no call left to allow.
func (d *pluginDecider) refuse(ctx context.Context, toolName string, err error) tool.Verdict {
	category, isFault := ClassifyCallFault(ctx, err)
	if isFault {
		if d.deps.OnFault != nil {
			d.deps.OnFault(ctx, category, toolName, err.Error())
		}
		publishCallFailed(ctx, d.deps.PluginName, d.deps, category, "decide:"+toolName, err.Error())
	}
	return tool.Verdict{Decision: tool.DecisionDeny, Reason: err.Error()}
}

// decodeToolDecision reads a guest's answer to abi.OpDecideToolCall.
//
// Decoding is strict, and every failure is an error rather than a permissive
// default: an answer nobody could read must not become an allow. The three
// refusals mirror decodeToolResult's, for the same reasons —
//
//   - an empty body is not an answer (Instance.Invoke returns (nil, nil) when
//     the guest's packed length is 0, so this is reachable in production);
//   - unknown fields are refused, because a body of the wrong shape decodes
//     into a zero value without complaint, and a zero Decision would have to
//     be interpreted as something;
//   - trailing data is refused, because taking the first of two documents is a
//     choice made on the guest's behalf.
//
// — plus one this op has of its own: a decision word outside the vocabulary is
// refused rather than treated as deny. Both readings are safe, but only the
// error says WHICH word the guest sent, and that is what an author needs to
// fix a plugin that thinks "ask" already works (it will, in G4c).
func decodeToolDecision(body []byte) (tool.Verdict, error) {
	if len(body) == 0 {
		return tool.Verdict{}, errors.New("guest returned no body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var answer guestToolDecision
	if err := decoder.Decode(&answer); err != nil {
		return tool.Verdict{}, fmt.Errorf("decode decision %s: %w", quoteForError(body), err)
	}
	if decoder.More() {
		return tool.Verdict{}, fmt.Errorf("decode decision %s: trailing data after the JSON document",
			quoteForError(body))
	}

	switch decision := tool.Decision(answer.Decision); decision {
	case tool.DecisionAllow:
		return tool.Verdict{Decision: tool.DecisionAllow, Reason: answer.Reason}, nil
	case tool.DecisionDeny, tool.DecisionAsk:
		// A reason is what the operator reads — in the refusal for a deny, and
		// in the approval request for an ask. "It said no" and "somebody
		// should look at this", with no further word, are the two answers
		// nobody downstream can act on, so an empty one is completed rather
		// than passed on.
		reason := answer.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return tool.Verdict{Decision: decision, Reason: reason}, nil
	default:
		return tool.Verdict{}, fmt.Errorf("decode decision %s: unknown decision %q; this ABI understands %q, %q and %q",
			quoteForError(body), answer.Decision, tool.DecisionAllow, tool.DecisionDeny, tool.DecisionAsk)
	}
}
