package legionplugin

import "encoding/json"

// ToolDecisionRequest is one tool call the host is ASKING ABOUT, before it
// dispatches it. A plugin granted the "decide" extension is consulted for
// every tool the agent runs, not only its own.
//
// There is no result field: nothing has run yet. That is the whole difference
// between this seam and ToolObservation.
type ToolDecisionRequest struct {
	CallID    string            `json:"call_id"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments"`
}

// ToolDecision is a plugin's answer: allow or deny, plus a reason.
//
// Build it with Allow or Deny. The zero value is deliberately NOT a valid
// answer — an author who forgets to return something gets a decision the host
// refuses to decode, which fails closed, rather than an accidental allow.
type ToolDecision struct {
	decision string
	reason   string
}

// Allow answers "I do not object".
//
// It is not an authorization. The host's own permissions and policy already
// ran and allowed this call before any plugin was asked; a plugin can only
// make the outcome stricter, never looser.
func Allow() ToolDecision { return ToolDecision{decision: decisionAllow} }

// Deny refuses the call. reason reaches the model and the operator verbatim,
// so it should say what is wrong in words somebody can act on.
func Deny(reason string) ToolDecision {
	return ToolDecision{decision: decisionDeny, reason: reason}
}

// Decider is consulted before a tool call runs.
//
// It must be FAST — faster than an observer. The host bounds each
// consultation at min(the tool's own timeout / 4, 200ms), and the tool has
// not started yet: every millisecond here is a millisecond added to the
// call. And unlike the observe seam, FAILING IS NOT FREE: a consultation that
// times out, traps, or answers something the host cannot decode DENIES the
// call and counts a fault against this plugin's health.
type Decider func(ToolDecisionRequest) ToolDecision

// Decide registers this plugin's decider. Call it exactly once, from init,
// alongside Serve.
//
//	func init() {
//		legionplugin.Serve("legion-gatekeeper", "0.1.0", legionplugin.Tool{ ... })
//		legionplugin.Decide(func(req legionplugin.ToolDecisionRequest) legionplugin.ToolDecision {
//			if req.Tool == "write_file" && frozen() {
//				return legionplugin.Deny("writes are frozen during the incident")
//			}
//			return legionplugin.Allow()
//		})
//	}
//
// As with Observe, registering is only half of it: the deployment must also
// grant the extension (`agent plugins grant <name> --extensions decide`).
// Without the grant the host registers no decider and this function is never
// called. With the grant but WITHOUT this registration, activation is
// refused — the SDK reports its registered extensions in op 0 and the host
// cross-checks the grant against them.
//
// It PANICS on a nil decider or a second registration: a plugin has one
// decider, and a second registration would silently discard one of them.
func Decide(decider Decider) {
	if decider == nil {
		panic("legionplugin: Decide: decider is nil; a registration that decides nothing is a wiring mistake")
	}
	if registry.decider != nil {
		panic("legionplugin: Decide: a decider is already registered; a plugin has one decider, " +
			"and a second registration would silently discard one of them")
	}
	registry.decider = decider
}

// decide is op 3's body: decode the call, ask the registered decider, answer.
//
// Unlike observe's, this answer MATTERS, so the failure paths differ: there is
// no benign acknowledgement to fall back on. A request this SDK cannot decode
// is answered with an explicit DENY that says so — the host would deny the
// call anyway (it fails closed on an unreadable answer), and denying here
// makes the reason legible instead of leaving the operator with "the plugin
// answered garbage".
//
// A decider registered nowhere cannot happen through a correctly assembled
// deployment (activation cross-checks the grant against op 0), and still gets
// an answer rather than a trap.
func decide(request []byte) []byte {
	if registry.decider == nil {
		return mustEncodeDecision(Deny("legionplugin: this plugin registered no decider"))
	}
	var decisionRequest ToolDecisionRequest
	if err := json.Unmarshal(request, &decisionRequest); err != nil {
		return mustEncodeDecision(Deny("legionplugin: could not decode the decision request: " + err.Error()))
	}
	return mustEncodeDecision(registry.decider(decisionRequest))
}

// wireDecision is the document the host strictly decodes.
type wireDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// mustEncodeDecision renders a decision, falling back to a hand-built DENY if
// encoding fails.
//
// The fallback direction is the whole point: an SDK that could not say
// "allow" must not accidentally say it. Failing to encode ends as a refusal,
// same as every other way of not answering.
func mustEncodeDecision(decision ToolDecision) []byte {
	body, err := json.Marshal(wireDecision{Decision: decision.decision, Reason: decision.reason})
	if err != nil {
		return []byte(`{"decision":"deny","reason":"legionplugin: could not encode the decision"}`)
	}
	return body
}

// The decision vocabulary, mirroring internal/tool.Decision. Spelled here
// rather than imported because a guest is compiled on its own, often outside
// this repository.
const (
	decisionAllow = "allow"
	decisionDeny  = "deny"
)

// extensionDecide is the wire name of the decision seam, mirroring
// internal/plugin/perm.ExtensionDecide.
const extensionDecide = "decide"
