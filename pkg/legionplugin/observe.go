package legionplugin

import "encoding/json"

// ToolObservation is one completed tool call, as the host reports it to a
// plugin that was granted the "observe" extension.
//
// It carries the CALL and its RESULT together, so an observer never has to
// correlate two notifications itself. The fields mirror ToolCall and
// ToolResult exactly — one vocabulary, whichever direction a call travels.
//
// Tool is any tool the agent ran, not only this plugin's own: that is the
// point of the seam.
type ToolObservation struct {
	CallID    string            `json:"call_id"`
	Tool      string            `json:"tool"`
	Arguments map[string]string `json:"arguments"`
	Success   bool              `json:"success"`
	Output    string            `json:"output"`
	Error     string            `json:"error"`
}

// Observer is notified after a tool call has answered. It RETURNS NOTHING,
// and that is the contract rather than an omission: the caller already holds
// its result, the host discards whatever a guest replies here, and an
// observer cannot allow, deny, delay or rewrite anything.
//
// It must be FAST. It runs inside the host's tool call, on the caller's
// goroutine, and the host bounds each notification at 200ms — a notification
// that overruns is counted as a fault against this plugin's health, and
// enough consecutive faults unload it. Do the cheap part here; if there is
// expensive work, record what is needed and do it on a later call of your
// own.
type Observer func(ToolObservation)

// Observe registers this plugin's observer. Call it exactly once, from init,
// alongside Serve.
//
//	func init() {
//		legionplugin.Serve("legion-audit", "0.1.0", legionplugin.Tool{ ... })
//		legionplugin.Observe(func(o legionplugin.ToolObservation) {
//			legionplugin.LogInfo("saw " + o.Tool)
//		})
//	}
//
// Registering it is only half of what makes it run: the deployment must also
// GRANT the extension (`agent plugins grant <name> --extensions observe`, or
// the consent dialog). Without the grant the host registers no observer at
// all and this function is never called — an ungranted extension is an absent
// registration, not a runtime check.
//
// The reverse pairing — a grant with no registration here — is refused at
// activation: the SDK reports the registered extensions in its op 0
// self-description, and the host cross-checks the grant against it, so a
// deployment cannot silently authorize a seam this binary does not implement.
//
// It PANICS on a nil observer or a second registration, both of which are
// wiring mistakes with no sensible reading: a plugin has ONE observer, and a
// second Observe call would silently discard whichever one lost.
func Observe(observer Observer) {
	if observer == nil {
		panic("legionplugin: Observe: observer is nil; a registration that observes nothing is a wiring mistake")
	}
	if registry.observer != nil {
		panic("legionplugin: Observe: an observer is already registered; a plugin has one observer, " +
			"and a second registration would silently discard one of them")
	}
	registry.observer = observer
}

// observe is op 2's body: decode the observation, hand it to the registered
// observer, answer.
//
// Every failure is an ANSWER rather than a panic, for the same reason
// dispatch's are: a panic traps the module and costs every call in flight on
// this instance. The answer's content is irrelevant — the host reads and
// discards it — so this returns the smallest well-formed document there is.
// What matters is not trapping and not blocking.
//
// An observation that arrives with no observer registered is answered the
// same way. It cannot happen through a correctly assembled deployment (the
// host cross-checks the grant against this SDK's self-description at
// activation), and an embedder driving the ABI directly still gets an answer
// rather than a trap.
func observe(request []byte) []byte {
	if registry.observer == nil {
		return observeAck
	}
	var observation ToolObservation
	if err := json.Unmarshal(request, &observation); err != nil {
		// Nothing to report this to: the seam is one-way by construction, and
		// LogInfo may not be imported by this build. The host will see a
		// well-formed answer; what it will NOT see is a trap, which is the
		// only thing that would hurt the caller.
		return observeAck
	}
	registry.observer(observation)
	return observeAck
}

// observeAck is op 2's answer: well-formed, minimal, and discarded by the
// host. It is a package-level value rather than a fresh allocation per
// notification because this runs on every tool call the agent makes.
var observeAck = []byte(`{}`)

// registeredExtensions renders the extension names this plugin implements,
// for op 0's self-description. It is derived from what Observe registered —
// never from a literal an author has to remember to keep in sync — so the
// guest cannot claim a seam it did not wire up.
func registeredExtensions() []string {
	if registry.observer == nil {
		return nil
	}
	return []string{extensionObserve}
}

// extensionObserve is the wire name of the observation seam, mirroring
// internal/plugin/perm.ExtensionObserve. It is spelled here rather than
// imported because a guest is compiled on its own, often outside this
// repository.
const extensionObserve = "observe"
