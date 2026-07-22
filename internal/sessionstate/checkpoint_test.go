package sessionstate

import (
	"fmt"
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
	got, ok, err := store.Load("sess-1", "")
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
	_, ok, err := store.Load("nope", "")
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
	_, ok, err := store.Load("sess-bad", "")
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
	_, _, err := store.Load("sess-v", "")
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
	if err := store.Delete("sess-del", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := store.Load("sess-del", "")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if ok {
		t.Fatal("Load after delete ok = true, want false")
	}
}

func TestStoreDeleteAbsentIsNoError(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Delete("never-existed", ""); err != nil {
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

func TestCheckpointRoundTripPreservesMode(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	cp := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		TaskID:        "t1",
		SessionKey:    "s1",
		Mode:          "manual",
		BasePrompt:    "p",
		Round:         1,
	}
	if err := store.Save(cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load("s1", "")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.Mode != "manual" {
		t.Fatalf("Mode = %q, want manual", got.Mode)
	}
}

func TestCheckpointSaveLoadUnderWorkingDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	workingDir := t.TempDir()
	s := NewStore(workspaceRoot)
	cp := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion, TaskID: "t1", SessionKey: "s1",
		WorkingDir: workingDir, BasePrompt: "p", CreatedAt: time.Unix(1, 0),
	}
	if err := s.Save(cp); err != nil {
		t.Fatalf("Save error = %v, want nil", err)
	}
	// Physically under <workingDir>/.stardust/session/s1, NOT workspaceRoot.
	want := filepath.Join(workingDir, ".stardust", "session", "s1", "task-state.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("checkpoint not at %q: %v", want, err)
	}
	got, ok, err := s.Load("s1", workingDir)
	if err != nil || !ok {
		t.Fatalf("Load = _, %v, %v; want found", ok, err)
	}
	if got.TaskID != "t1" {
		t.Fatalf("Load TaskID = %q, want t1", got.TaskID)
	}
}

func TestCheckpointSaveLoadWorkspaceRootWhenNoWorkingDir(t *testing.T) {
	workspaceRoot := t.TempDir()
	s := NewStore(workspaceRoot)
	cp := Checkpoint{SchemaVersion: CheckpointSchemaVersion, TaskID: "t2", SessionKey: "s2", CreatedAt: time.Unix(1, 0)}
	if err := s.Save(cp); err != nil {
		t.Fatalf("Save error = %v, want nil", err)
	}
	want := filepath.Join(workspaceRoot, "session", "s2", "task-state.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("checkpoint not at workspaceRoot %q: %v", want, err)
	}
	if _, ok, _ := s.Load("s2", ""); !ok {
		t.Fatal("Load(s2, \"\") not found, want found")
	}
}

func TestListSuspendedInScansGivenBase(t *testing.T) {
	workspaceRoot := t.TempDir()
	workingDir := t.TempDir()
	s := NewStore(workspaceRoot)
	_ = s.Save(Checkpoint{SchemaVersion: CheckpointSchemaVersion, TaskID: "t1", SessionKey: "s1", WorkingDir: workingDir, CreatedAt: time.Unix(1, 0)})
	base := SessionBase(workspaceRoot, workingDir)
	got, err := s.ListSuspendedIn(base)
	if err != nil {
		t.Fatalf("ListSuspendedIn error = %v", err)
	}
	if len(got) != 1 || got[0].TaskID != "t1" {
		t.Fatalf("ListSuspendedIn = %#v, want 1 checkpoint t1", got)
	}
}

func TestCheckpointRoundTripPreservesLoaded(t *testing.T) {
	store := NewStore(t.TempDir())
	cp := sampleCheckpoint("sess-loaded")
	cp.Loaded = []LoadedCapability{
		{Name: "read_file", Detail: `{"name":"read_file"}`},
		{Name: "curator", Detail: "skill body text"},
	}
	if err := store.Save(cp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load("sess-loaded", "")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(got.Loaded) != 2 {
		t.Fatalf("Load Loaded = %#v, want 2 entries", got.Loaded)
	}
	if got.Loaded[0].Name != "read_file" || got.Loaded[0].Detail != `{"name":"read_file"}` {
		t.Errorf("Load Loaded[0] = %#v, want the read_file entry preserved verbatim", got.Loaded[0])
	}
	if got.Loaded[1].Name != "curator" || got.Loaded[1].Detail != "skill body text" {
		t.Errorf("Load Loaded[1] = %#v, want the curator entry preserved verbatim", got.Loaded[1])
	}
}

// TestLoadCheckpointWithoutLoadedFieldIsEmptyNotError pins that a checkpoint
// JSON payload with no "loaded" key at all (the shape a v3 checkpoint has
// whenever the run never called load_capabilities) decodes to an empty Loaded
// with no error -- absence here is a legitimate optional state, not a fault.
func TestLoadCheckpointWithoutLoadedFieldIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	sessDir := SessionDir(dir, "sess-noloaded")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"schema_version":%d,"task_id":"t1","session_key":"sess-noloaded"}`, CheckpointSchemaVersion)
	if err := os.WriteFile(filepath.Join(sessDir, checkpointFileName), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := NewStore(dir).Load("sess-noloaded", "")
	if err != nil {
		t.Fatalf("Load error = %v, want nil (missing loaded key is legal)", err)
	}
	if !ok {
		t.Fatal("Load ok = false, want true")
	}
	if len(got.Loaded) != 0 {
		t.Fatalf("Loaded = %#v, want empty", got.Loaded)
	}
}

func TestLoadRejectsV1Checkpoint(t *testing.T) {
	dir := t.TempDir()
	sessDir := SessionDir(dir, "s1")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A v1 checkpoint on disk must fail loud, not half-decode into a modeless task.
	if err := os.WriteFile(filepath.Join(sessDir, "task-state.json"),
		[]byte(`{"schema_version":1,"task_id":"t1","session_key":"s1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(dir).Load("s1", ""); err == nil {
		t.Fatal("Load of v1 checkpoint: want fail-loud error, got nil")
	}
}
