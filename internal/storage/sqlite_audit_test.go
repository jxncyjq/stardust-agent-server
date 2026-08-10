package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestAuditEventTokenRoundTrip(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	in := domain.AuditEvent{
		ID: "t1:model-audit-1", RequestID: "t1:run", SubjectType: "model", SubjectID: "t1",
		Action: "model_inference_completed", Hash: "memory",
		PromptTokens: 1200, CompletionTokens: 340, CachedTokens: 800, TotalTokens: 1540,
		CreatedAt: time.Now(),
	}
	if err := r.AppendAuditEvent(ctx, in); err != nil {
		t.Fatal(err)
	}
	events, err := r.ListAuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *domain.AuditEvent
	for i := range events {
		if events[i].ID == in.ID {
			got = &events[i]
		}
	}
	if got == nil {
		t.Fatal("event not found")
	}
	if got.PromptTokens != 1200 || got.CompletionTokens != 340 || got.CachedTokens != 800 || got.TotalTokens != 1540 {
		t.Fatalf("token mismatch: %+v", got)
	}
}

func TestAuditEventWithoutTokensReadsZero(t *testing.T) {
	r := openTestRepo(t)
	ctx := context.Background()
	if err := r.AppendAuditEvent(ctx, domain.AuditEvent{ID: "t1:audit-1", RequestID: "t1:run", SubjectType: "task", SubjectID: "t1", Action: "task_completed", Hash: "memory", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, _ := r.ListAuditEvents(ctx)
	for _, e := range events {
		if e.ID == "t1:audit-1" && e.TotalTokens != 0 {
			t.Fatalf("want 0 tokens, got %d", e.TotalTokens)
		}
	}
}
