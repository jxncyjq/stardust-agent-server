package legionplugin

import "encoding/json"

// PromptProvider returns the block of text this plugin contributes to the
// system prompt.
//
// It is called ONCE, at activation, and the answer is used for as long as the
// plugin stays mounted — so it may read the plugin's configuration, but it
// must not try to say something different per task. There is no per-task
// hook, deliberately: the block lives in the prompt's cache-stable prefix,
// and text that changed per task would cost every task a cache miss.
//
// Returning "" is a legitimate answer: a plugin may decide it has nothing to
// say for this deployment's configuration, and nothing is rendered.
type PromptProvider func() string

// Prompt registers this plugin's system-prompt segment. Call it exactly once,
// from init, alongside Serve.
//
//	func init() {
//		legionplugin.Serve("legion-jira", "0.1.0", legionplugin.Tool{ ... })
//		legionplugin.Prompt(func() string {
//			return "When citing a Jira issue, link it as https://jira.example.com/browse/KEY."
//		})
//	}
//
// Two things worth knowing before using it:
//
//   - The text is FENCED in the prompt with markers naming this plugin and
//     saying it is untrusted. That is not decoration: it is what lets the
//     model weigh a plugin's instruction against the host's own.
//   - It is BOUNDED (2048 runes per plugin, 8192 across all of them).
//     Overrunning is truncated visibly rather than refused — but it is paid
//     for on every inference, forever, so shorter is not a style preference.
//
// As with the other extensions, registering is half of it: the deployment must
// also grant it (`agent plugins grant <name> --extensions prompt`). A grant
// without this registration is refused at activation.
//
// It PANICS on a nil provider or a second registration — a plugin has one
// segment, and a second registration would silently discard one of them.
func Prompt(provider PromptProvider) {
	if provider == nil {
		panic("legionplugin: Prompt: provider is nil; a registration that contributes nothing is a wiring mistake")
	}
	if registry.prompt != nil {
		panic("legionplugin: Prompt: a prompt segment is already registered; a plugin has one segment, " +
			"and a second registration would silently discard one of them")
	}
	registry.prompt = provider
}

// promptSegment is op 4's body: answer with the registered text.
//
// A plugin with no provider answers with an empty segment rather than an
// error. It cannot happen through a correctly assembled deployment (the host
// cross-checks the grant against op 0), and an embedder driving the ABI
// directly gets a well-formed answer instead of a trap.
func promptSegment() []byte {
	text := ""
	if registry.prompt != nil {
		text = registry.prompt()
	}
	body, err := json.Marshal(wirePromptSegment{Text: text})
	if err != nil {
		// Unreachable for a string field, and not silently dropped: the host
		// refuses to mount a plugin whose segment it cannot read, which is the
		// honest outcome of an encoder that failed.
		return []byte(`{"text":""}`)
	}
	return body
}

// wirePromptSegment is the document the host strictly decodes.
type wirePromptSegment struct {
	Text string `json:"text"`
}

// extensionPrompt is the wire name of the prompt seam, mirroring
// internal/plugin/perm.ExtensionPrompt.
const extensionPrompt = "prompt"
