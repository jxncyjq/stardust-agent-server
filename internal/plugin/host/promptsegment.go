package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stardust/legion-agent/internal/lifecycle"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/prompt"
)

// contributeAll files everything a mounted plugin leaves behind, under the ONE
// contribution owner: its tools (and their gateable-catalog entries and any
// granted seams), then — if the deployment granted it — its prompt segment.
//
// It exists as one function so that "what does activation contribute?" has a
// single answer that a test can drive without a compiled module. The two
// halves fail differently and that is deliberate: contributeTools PANICS on a
// name another contributor already owns (there is no sensible half-mounted
// state), while the prompt segment returns an error, because asking a guest a
// question that goes wrong is not a programming mistake.
//
// The order matters on the way out rather than on the way in: the segment is
// filed under the same owner as the tools, so a suspend or an unload takes the
// plugin's text out of the prompt together with its tools.
func contributeAll(
	ctx context.Context,
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	spec Spec,
	guest guestCaller,
	keep func(revoke func() error),
) error {
	contributeTools(ledger, owner, spec, guest, keep)
	if !spec.Extensions.Prompt {
		return nil
	}
	return contributePromptSegment(ctx, ledger, owner, spec, guest, keep)
}

// guestPromptSegment is the guest's answer to abi.OpPromptSegment: the block
// of text it wants in the system prompt.
type guestPromptSegment struct {
	Text string `json:"text"`
}

// contributePromptSegment asks a guest for its prompt segment ONCE, at
// activation, and files it under the contribution owner.
//
// Once, because the alternative — asking while a prompt is being built — costs
// a wasm call on every task's critical path AND lets the answer change per
// task, which would destroy the cache-stable prefix this block was put in for
// (see the G4 spec's decision C). A plugin's segment is a deployment-level
// fact: it changes when the plugin is remounted, and not otherwise.
//
// Every failure REFUSES THE ACTIVATION rather than mounting the plugin without
// its text. A plugin granted the prompt extension that contributes no prompt
// is a deployment that believes it installed something it did not: the tools
// would be there, the instructions telling the model how to use them would
// silently not be.
//
// A nil segment store while the extension is granted is a WIRING bug in the
// embedder, and it fails the same way for the same reason.
func contributePromptSegment(
	ctx context.Context,
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	spec Spec,
	guest guestCaller,
	keep func(revoke func() error),
) error {
	if spec.Deps.PromptSegments == nil {
		return fmt.Errorf("plugin %q was granted the %q extension, but this host has no prompt segment store "+
			"wired: the plugin's text could never reach the model", spec.Name, "prompt")
	}
	body, err := guest.call(ctx, abi.OpPromptSegment, nil)
	if err != nil {
		return fmt.Errorf("ask plugin %q for its prompt segment: %w", spec.Name, err)
	}
	text, err := decodePromptSegment(body)
	if err != nil {
		return fmt.Errorf("ask plugin %q for its prompt segment: %w: %w", spec.Name, err, ErrGuestABI)
	}
	keep(prompt.ContributeOwned(ledger, owner, spec.Deps.PromptSegments, spec.Name, text))
	return nil
}

// decodePromptSegment reads a guest's answer to abi.OpPromptSegment.
//
// Strict, for the same reasons decodeToolResult is: an empty body is not an
// answer, unknown fields mean the guest is answering a different question than
// the one that was asked, and trailing data is two documents where the
// contract names one.
//
// An empty "text" IS a valid answer and yields an empty segment (which
// prompt.Segments declines to register): a plugin may legitimately decide it
// has nothing to say for this deployment's configuration. What it may not do
// is fail to answer.
func decodePromptSegment(body []byte) (string, error) {
	if len(body) == 0 {
		return "", errors.New("guest returned no body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var answer guestPromptSegment
	if err := decoder.Decode(&answer); err != nil {
		return "", fmt.Errorf("decode prompt segment %s: %w", quoteForError(body), err)
	}
	if decoder.More() {
		return "", fmt.Errorf("decode prompt segment %s: trailing data after the JSON document",
			quoteForError(body))
	}
	return answer.Text, nil
}
