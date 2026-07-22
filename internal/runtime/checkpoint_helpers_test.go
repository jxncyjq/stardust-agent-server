package runtime

import (
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSessionKeyForTaskPrefersSessionID(t *testing.T) {
	if got := sessionKeyForTask(domain.Task{ID: "t1", SessionID: "s1"}); got != "s1" {
		t.Errorf("sessionKeyForTask = %q, want s1", got)
	}
	if got := sessionKeyForTask(domain.Task{ID: "t1"}); got != "t1" {
		t.Errorf("sessionKeyForTask (no session) = %q, want t1", got)
	}
}

func TestToolEntrySnapshotRoundTrip(t *testing.T) {
	entries := []toolEntry{{key: "read|path=a", text: "- a success: hi"}, {key: "list", text: "- list success: x"}}
	restored := restoreToolEntries(snapshotToolEntries(entries))
	if len(restored) != len(entries) {
		t.Fatalf("restored len = %d, want %d", len(restored), len(entries))
	}
	for i := range entries {
		if restored[i].key != entries[i].key || restored[i].text != entries[i].text {
			t.Errorf("restored[%d] = %+v, want %+v", i, restored[i], entries[i])
		}
	}
}

func TestLoadedSnapshotRoundTrip(t *testing.T) {
	entries := []loadedEntry{
		{name: "read_file", detail: `{"name":"read_file"}`},
		{name: "curator", detail: "skill body text"},
	}
	restored := restoreLoaded(snapshotLoaded(entries))
	if len(restored) != len(entries) {
		t.Fatalf("restored len = %d, want %d", len(restored), len(entries))
	}
	for i := range entries {
		if restored[i].name != entries[i].name || restored[i].detail != entries[i].detail {
			t.Errorf("restored[%d] = %+v, want %+v", i, restored[i], entries[i])
		}
	}
}

func TestLoadedSnapshotRoundTripEmptyIsEmpty(t *testing.T) {
	if got := restoreLoaded(snapshotLoaded(nil)); len(got) != 0 {
		t.Fatalf("restoreLoaded(snapshotLoaded(nil)) = %#v, want empty", got)
	}
}
