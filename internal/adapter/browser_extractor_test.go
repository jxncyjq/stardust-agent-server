package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

type stubMaas struct {
	gotPrompt string
	resp      port.InferenceResponse
	err       error
}

func (s *stubMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	s.gotPrompt = req.Prompt
	return s.resp, s.err
}

func TestMaasSnapshotExtractor_BuildsTaskPromptAndTrims(t *testing.T) {
	m := &stubMaas{resp: port.InferenceResponse{Text: "  [e1] <button> Buy  "}}
	e := NewMaasSnapshotExtractor(m)
	out, err := e.Extract(context.Background(), "buy milk", "[e1] <button> Buy\n[e2] <link> Ads")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != "[e1] <button> Buy" {
		t.Fatalf("out = %q, want trimmed", out)
	}
	if !strings.Contains(m.gotPrompt, "buy milk") || !strings.Contains(m.gotPrompt, "[e2] <link> Ads") {
		t.Fatalf("prompt missing task or snapshot: %q", m.gotPrompt)
	}
	if !strings.Contains(m.gotPrompt, "[eN]") {
		t.Fatalf("prompt missing ref-preservation instruction")
	}
}

func TestMaasSnapshotExtractor_PropagatesError(t *testing.T) {
	m := &stubMaas{err: errors.New("upstream down")}
	e := NewMaasSnapshotExtractor(m)
	_, err := e.Extract(context.Background(), "t", "s")
	if err == nil {
		t.Fatalf("err = nil, want propagated")
	}
}
