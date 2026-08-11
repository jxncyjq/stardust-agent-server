package tool

import (
	"context"
	"testing"
)

func TestUserTaskContextRoundTrip(t *testing.T) {
	ctx := WithUserTask(context.Background(), "buy milk")
	if got := UserTaskFromContext(ctx); got != "buy milk" {
		t.Fatalf("got %q, want buy milk", got)
	}
}

func TestUserTaskFromContext_Absent(t *testing.T) {
	if got := UserTaskFromContext(context.Background()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
