// Package testsupport holds helpers shared by tests across packages.
package testsupport

import (
	"strings"

	"github.com/stardust/legion-agent/internal/port"
)

// RequestText renders an inference request as one string for assertions.
//
// The tool loop sends multi-turn Messages rather than a single Prompt, so a
// test that used to read req.Prompt would now read an empty string and pass or
// fail for the wrong reason. This flattens whichever form the request uses:
// every message's content in order, plus each assistant turn's tool call names
// and arguments, so "did the model see X" stays a single substring check.
func RequestText(req port.InferenceRequest) string {
	if len(req.Messages) == 0 {
		return req.Prompt
	}
	// Parts are joined without a trailing separator so a single-message request
	// renders exactly like the prompt it replaced, keeping equality assertions
	// meaningful.
	parts := make([]string, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Content != "" {
			parts = append(parts, msg.Content)
		}
		for _, call := range msg.ToolCalls {
			var b strings.Builder
			b.WriteString(call.Name)
			for key, value := range call.Arguments {
				b.WriteString(" ")
				b.WriteString(key)
				b.WriteString("=")
				b.WriteString(value)
			}
			parts = append(parts, b.String())
		}
	}
	return strings.Join(parts, "\n")
}
