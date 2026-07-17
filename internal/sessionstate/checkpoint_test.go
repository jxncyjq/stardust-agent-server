package sessionstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func sampleCheckpoint(key string) Checkpoint {
	return Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "task-1",
		AgentID:       "agent-1",
		SessionKey:    key,
		BasePrompt:    "system + task framing",
		Round:         2,
		ToolEntries:   []ToolEntrySnapshot{{Key: "read|path=a", Text: "- a success: hi"}},
		PendingCalls:  []domain.ToolCall{{ID: "c1", Name: "read", Arguments: map[string]string{"path": "b"}}},
		PromptTokens:  10,
		TotalTokens:   12,
		Images:        []string{"data:image/png;base64,xxx"},
		CreatedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	cp := sampleCheckpoint("sess-1")
	if err := store.Save(cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load("sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("Load ok = false, want true after Save")
	}
	if got.Round != cp.Round || got.BasePrompt != cp.BasePrompt {
		t.Errorf("Load round/base = %d/%q, want %d/%q", got.Round, got.BasePrompt, cp.Round, cp.BasePrompt)
	}
	if len(got.PendingCalls) != 1 || got.PendingCalls[0].Name != "read" {
		t.Errorf("Load PendingCalls = %#v, want one read call", got.PendingCalls)
	}
	if len(got.ToolEntries) != 1 || got.ToolEntries[0].Key != "read|path=a" {
		t.Errorf("Load ToolEntries = %#v, want one entry", got.ToolEntries)
	}
}

func TestStoreLoadAbsentReturnsFalseNoError(t *testing.T) {
	store := NewStore(t.TempDir())
	_, ok, err := store.Load("nope")
	if err != nil {
		t.Fatalf("Load absent error = %v, want nil (absence is legitimate, not a fault)", err)
	}
	if ok {
		t.Fatal("Load absent ok = true, want false")
	}
}

func TestStoreLoadCorruptJSONFailsLoud(t *testing.T) {
	base := t.TempDir()
	store := NewStore(base)
	dir := SessionDir(base, "sess-bad")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ok, err := store.Load("sess-bad")
	if err == nil {
		t.Fatal("Load corrupt error = nil, want fail-loud error")
	}
	if ok {
		t.Fatal("Load corrupt ok = true, want false")
	}
}

func TestStoreLoadVersionMismatchFailsLoud(t *testing.T) {
	base := t.TempDir()
	store := NewStore(base)
	cp := sampleCheckpoint("sess-v")
	cp.SchemaVersion = CheckpointSchemaVersion + 99
	// Save writes whatever version the checkpoint carries; Load must reject it.
	if err := store.Save(cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, _, err := store.Load("sess-v")
	if err == nil {
		t.Fatal("Load version-mismatch error = nil, want fail-loud error")
	}
}

func TestStoreDeleteRemovesCheckpoint(t *testing.T) {
	store := NewStore(t.TempDir())
	cp := sampleCheckpoint("sess-del")
	if err := store.Save(cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete("sess-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := store.Load("sess-del")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if ok {
		t.Fatal("Load after delete ok = true, want false")
	}
}

func TestStoreDeleteAbsentIsNoError(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Delete("never-existed"); err != nil {
		t.Fatalf("Delete absent = %v, want nil (idempotent)", err)
	}
}

func TestStoreListSuspendedReturnsAllCheckpoints(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save(sampleCheckpoint("s1")); err != nil {
		t.Fatalf("Save s1: %v", err)
	}
	if err := store.Save(sampleCheckpoint("s2")); err != nil {
		t.Fatalf("Save s2: %v", err)
	}
	got, err := store.ListSuspended()
	if err != nil {
		t.Fatalf("ListSuspended: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSuspended len = %d, want 2", len(got))
	}
}

func TestStoreListSuspendedEmptyIsEmptyNoError(t *testing.T) {
	store := NewStore(t.TempDir())
	got, err := store.ListSuspended()
	if err != nil {
		t.Fatalf("ListSuspended empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSuspended empty len = %d, want 0", len(got))
	}
}
