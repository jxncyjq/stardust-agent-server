package port_test

import (
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// Prompt and Messages both set would leave it to each adapter to decide which
// one the model actually sees; a caller that filled the wrong one would get a
// plausible answer to a different question instead of an error.
func TestInferenceRequestValidateRejectsBothPromptAndMessages(t *testing.T) {
	req := port.InferenceRequest{
		RequestID: "r1",
		Prompt:    "hi",
		Messages:  []port.InferenceMessage{{Role: port.RoleUser, Content: "hi"}},
	}

	err := req.Validate()

	if err == nil {
		t.Fatal("expected an error when both Prompt and Messages are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error must explain the conflict, got %v", err)
	}
}

func TestInferenceRequestValidateRejectsUnknownRole(t *testing.T) {
	req := port.InferenceRequest{Messages: []port.InferenceMessage{{Role: "system", Content: "x"}}}

	if err := req.Validate(); err == nil {
		t.Fatal("expected an error for an unknown role")
	}
}

// An OpenAI-compatible provider rejects a tool message it cannot pair with a
// preceding tool call, so the pairing is checked here rather than discovered as
// an HTTP 400 mid-run.
func TestInferenceRequestValidateRequiresToolCallIDOnToolMessage(t *testing.T) {
	req := port.InferenceRequest{Messages: []port.InferenceMessage{{Role: port.RoleTool, Content: "out"}}}

	if err := req.Validate(); err == nil {
		t.Fatal("expected an error for a tool message without ToolCallID")
	}
}

func TestInferenceRequestValidateAcceptsMultiTurnExchange(t *testing.T) {
	req := port.InferenceRequest{Messages: []port.InferenceMessage{
		{Role: port.RoleUser, Content: "read hello.txt"},
		{Role: port.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: port.RoleTool, ToolCallID: "c1", Content: "hello"},
	}}

	if err := req.Validate(); err != nil {
		t.Fatalf("a well-formed multi-turn request must validate: %v", err)
	}
}

// The single-turn contract predates Messages and must keep working untouched.
func TestInferenceRequestValidateAcceptsPromptOnly(t *testing.T) {
	if err := (port.InferenceRequest{Prompt: "hi"}).Validate(); err != nil {
		t.Fatalf("prompt-only request must stay valid: %v", err)
	}
}
