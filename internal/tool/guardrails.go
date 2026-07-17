package tool

import (
	"context"

	"github.com/stardust/legion-agent/internal/domain"
)

type GuardrailsFunc struct {
	BeforeFunc func(context.Context, domain.ToolCall) error
	AfterFunc  func(context.Context, domain.ToolCall, domain.ToolResult) error
}

func (g GuardrailsFunc) Before(ctx context.Context, call domain.ToolCall) error {
	if g.BeforeFunc == nil {
		return ctx.Err()
	}
	return g.BeforeFunc(ctx, call)
}

func (g GuardrailsFunc) After(ctx context.Context, call domain.ToolCall, result domain.ToolResult) error {
	if g.AfterFunc == nil {
		return ctx.Err()
	}
	return g.AfterFunc(ctx, call, result)
}
