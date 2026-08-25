package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// sampleEntry returns a minimal, already-valid local Entry named name, for
// tests that only care about identity and ordering, not content.
//
// GrantStated is deliberately true here (an explicit, if empty, grant
// decision) rather than left at its false zero value: a false GrantStated
// entry's "grant" key is omitted entirely on write (see entryDoc's doc
// comment in edit.go), which means Grant.Capabilities always reads back nil
// regardless of what it held before the write — exactly the shape Part A's
// own dedicated tests in this file cover. A round-trip fidelity check that
// used a false-GrantStated fixture would be asserting a Capabilities value
// MarshalDeployment never even attempts to preserve, not a real regression.
func sampleEntry(name string) Entry {
	return Entry{
		Name:        name,
		Source:      "./plugins/" + name,
		Grant:       GrantDecl{Capabilities: []string{}},
		GrantStated: true,
		Tools:       []ToolAccept{{Name: "do_thing"}},
	}
}

// requireDeploymentsEquivalent asserts got and want hold the same plugins in
// the same order, comparing every Entry field, with one deliberate
// exception: Config is compared after json.Compact rather than by raw
// bytes. json.MarshalIndent reformats an embedded json.RawMessage's
// whitespace to match its surrounding indentation (a compact
// {"project": "LEGION"} fixture value re-encodes with newlines and
// two-space indentation) even though the value it encodes never changes;
// comparing raw bytes here would fail on that harmless reformatting.
// json.Compact removes only insignificant whitespace — it does not touch
// numbers or reorder keys — so this still catches real corruption that a
// decode-into-`any`-then-compare would hide: unmarshaling JSON into `any`
// turns every number into float64, so a 20-digit id losing precision (or a
// key silently dropped) would decode to something reflect.DeepEqual still
// calls equal. Comparing decoded values is what would mask a defect here;
// comparing compacted bytes is what avoids it.
func requireDeploymentsEquivalent(t *testing.T, got, want Deployment) {
	t.Helper()
	if len(got.Plugins) != len(want.Plugins) {
		t.Fatalf("Plugins has %d entries, want %d", len(got.Plugins), len(want.Plugins))
	}
	for i := range want.Plugins {
		g, w := got.Plugins[i], want.Plugins[i]
		gc, wc := g.Config, w.Config
		g.Config, w.Config = nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Errorf("Plugins[%d] = %+v, want %+v", i, g, w)
		}
		if !rawMessageEquivalent(t, gc, wc) {
			t.Errorf("Plugins[%d].Config = %s, want %s (not equivalent JSON)", i, gc, wc)
		}
	}
}

func rawMessageEquivalent(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var ac, bc bytes.Buffer
	if err := json.Compact(&ac, a); err != nil {
		t.Fatalf("compact Config %s: %v", a, err)
	}
	if err := json.Compact(&bc, b); err != nil {
		t.Fatalf("compact Config %s: %v", b, err)
	}
	return bytes.Equal(ac.Bytes(), bc.Bytes())
}

// --- Rule 1: AddEntry on a duplicate name errors, naming the name ---------

func TestAddEntry_DuplicateName_Errors(t *testing.T) {
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira")}}
	dup := sampleEntry("legion-jira")
	dup.Source = "./plugins/legion-jira-v2"

	_, err := AddEntry(dep, dup)
	requireErrorContains(t, err, "legion-jira")
}

// --- Rule 2: UpdateEntry on an unknown name errors, naming the name and
// the existing entries -------------------------------------------------

func TestUpdateEntry_UnknownName_ErrorsNamingRequestedAndExisting(t *testing.T) {
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira"), sampleEntry("legion-notify")}}

	_, err := UpdateEntry(dep, "legion-missing", func(e Entry) (Entry, error) { return e, nil })
	requireErrorContains(t, err, "legion-missing")
	requireErrorContains(t, err, "legion-jira")
	requireErrorContains(t, err, "legion-notify")
}

func TestUpdateEntry_MutatesNamedEntry_LeavesOthersUntouched(t *testing.T) {
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira"), sampleEntry("legion-notify")}}

	got, err := UpdateEntry(dep, "legion-notify", func(e Entry) (Entry, error) {
		e.Enabled = true
		e.Grant.Capabilities = []string{"log"}
		return e, nil
	})
	if err != nil {
		t.Fatalf("UpdateEntry: unexpected error: %v", err)
	}

	if !reflect.DeepEqual(got.Plugins[0], dep.Plugins[0]) {
		t.Errorf("UpdateEntry modified an entry it was not asked to touch")
	}
	if !got.Plugins[1].Enabled {
		t.Errorf("Plugins[1].Enabled = false, want true (mutate's result should have been applied)")
	}
	if want := []string{"log"}; !reflect.DeepEqual(got.Plugins[1].Grant.Capabilities, want) {
		t.Errorf("Plugins[1].Grant.Capabilities = %v, want %v", got.Plugins[1].Grant.Capabilities, want)
	}

	// The caller's Deployment must be left untouched.
	if dep.Plugins[1].Enabled {
		t.Errorf("UpdateEntry mutated the caller's Deployment")
	}
}

// TestUpdateEntry_MutateError_WrapsAndReturnsWithoutModifyingDep covers the
// forward path UpdateEntry's own doc comment promises but which nothing
// previously exercised: "If mutate itself returns an error, UpdateEntry
// wraps and returns it without modifying dep." A caller (Task 4's grant
// authorization) relies on exactly this path to reject an ungranted
// capability from inside mutate, so an UpdateEntry that silently discarded
// mutate's error would let that rejection vanish with every other test
// still green.
func TestUpdateEntry_MutateError_WrapsAndReturnsWithoutModifyingDep(t *testing.T) {
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira"), sampleEntry("legion-notify")}}
	sentinel := errors.New("mutate refused: capability not granted")

	_, err := UpdateEntry(dep, "legion-notify", func(Entry) (Entry, error) {
		return Entry{}, sentinel
	})
	if err == nil {
		t.Fatalf("UpdateEntry: want an error when mutate fails, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("UpdateEntry error = %v, want it to wrap the sentinel error (errors.Is)", err)
	}
	requireErrorContains(t, err, "legion-notify")

	// dep itself, and both of its entries, must be left untouched.
	if !reflect.DeepEqual(dep.Plugins[0], sampleEntry("legion-jira")) ||
		!reflect.DeepEqual(dep.Plugins[1], sampleEntry("legion-notify")) {
		t.Errorf("UpdateEntry modified dep despite mutate returning an error")
	}
}

// --- Rule 3: new entries append at the end; existing order is untouched ---

func TestAddEntry_AppendsAtEnd_ExistingOrderUntouched(t *testing.T) {
	first := sampleEntry("legion-jira")
	second := sampleEntry("legion-notify")
	dep := Deployment{Plugins: []Entry{first, second}}
	third := sampleEntry("legion-slack")

	got, err := AddEntry(dep, third)
	if err != nil {
		t.Fatalf("AddEntry: unexpected error: %v", err)
	}

	wantNames := []string{"legion-jira", "legion-notify", "legion-slack"}
	if len(got.Plugins) != len(wantNames) {
		t.Fatalf("Plugins = %d entries, want %d", len(got.Plugins), len(wantNames))
	}
	for i, want := range wantNames {
		if got.Plugins[i].Name != want {
			t.Errorf("Plugins[%d].Name = %q, want %q (existing order must be untouched, new entry appended last)",
				i, got.Plugins[i].Name, want)
		}
	}
	if !reflect.DeepEqual(got.Plugins[0], first) || !reflect.DeepEqual(got.Plugins[1], second) {
		t.Errorf("AddEntry modified an existing entry's content")
	}

	// The caller's Deployment must be left untouched — AddEntry must not
	// write through dep.Plugins' backing array.
	if len(dep.Plugins) != 2 {
		t.Errorf("AddEntry mutated the caller's Deployment: len(dep.Plugins) = %d, want 2", len(dep.Plugins))
	}
}

// --- Rule 4: MarshalDeployment round-trips through ParseDeployment --------

// TestMarshalDeployment_RoundTripsRealFixture loads the package's own real
// plugins.json fixture (used by ParseDeployment's own tests), marshals what
// it parses to, and asserts every field survives — the scenario the brief
// calls out by name, rather than a Deployment this test built by hand.
func TestMarshalDeployment_RoundTripsRealFixture(t *testing.T) {
	data := mustReadFixture(t, "plugins.json")
	original, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment(fixture): %v", err)
	}

	marshaled, err := MarshalDeployment(original)
	if err != nil {
		t.Fatalf("MarshalDeployment: %v", err)
	}

	roundTripped, err := ParseDeployment(marshaled)
	if err != nil {
		t.Fatalf("ParseDeployment(MarshalDeployment(original)): %v\nmarshaled:\n%s", err, marshaled)
	}

	requireDeploymentsEquivalent(t, roundTripped, original)
}

// TestMarshalDeployment_RoundTrip_PreservesExplicitEnabledFalse is the
// mandatory negative-space companion to the fixture round trip above: the
// repo fixture's entries are all enabled (one explicitly, one by omission),
// so a round trip over it alone would stay green even if MarshalDeployment
// tagged Enabled "omitempty" and silently turned every disabled entry back
// into an enabled one. This test's second entry is explicitly disabled and
// must still read back disabled after a full Marshal -> Parse cycle.
func TestMarshalDeployment_RoundTrip_PreservesExplicitEnabledFalse(t *testing.T) {
	data := []byte(`{
		"plugins": [
			{"name": "legion-jira", "source": "./plugins/legion-jira", "enabled": true,
			 "grant": {"capabilities": ["log"]}, "tools": [{"name": "jira_search"}]},
			{"name": "legion-notify", "source": "./plugins/legion-notify", "enabled": false,
			 "grant": {"capabilities": []}, "tools": [{"name": "notify_send"}]}
		]
	}`)
	original, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment(fixture): %v", err)
	}
	if original.Plugins[1].Enabled {
		t.Fatalf("test setup is wrong: legion-notify must parse to Enabled = false")
	}

	marshaled, err := MarshalDeployment(original)
	if err != nil {
		t.Fatalf("MarshalDeployment: %v", err)
	}

	roundTripped, err := ParseDeployment(marshaled)
	if err != nil {
		t.Fatalf("ParseDeployment(MarshalDeployment(original)): %v\nmarshaled:\n%s", err, marshaled)
	}

	requireDeploymentsEquivalent(t, roundTripped, original)

	if roundTripped.Plugins[1].Enabled {
		t.Fatalf("legion-notify Enabled = true after a Marshal/Parse round trip, want false; an explicit "+
			"\"enabled\": false must never be written as an omitted field, since ParseDeployment reads an "+
			"omitted \"enabled\" back as true\nmarshaled:\n%s", marshaled)
	}
}

// TestMarshalDeployment_RoundTrip_PreservesRemoteEntryDigest covers the
// entry shape neither round-trip test above does: a REMOTE entry carrying a
// Digest. The repo fixture and the hand-built Deployment above both use
// only local entries (validateEntrySource forbids a local entry from
// carrying a Digest at all), so a MarshalDeployment change that dropped
// Digest on encode would have gone uncaught — the same digest Task 3's
// install path writes into every remote entry it produces.
func TestMarshalDeployment_RoundTrip_PreservesRemoteEntryDigest(t *testing.T) {
	original := Deployment{
		Plugins: []Entry{
			{
				Name:        "legion-remote",
				Source:      "https://plugins.example.com/legion-remote.tar.gz",
				Digest:      "sha256:" + validSHA256,
				Enabled:     true,
				Grant:       GrantDecl{Capabilities: []string{"http"}},
				GrantStated: true,
				Tools:       []ToolAccept{{Name: "fetch_thing"}},
			},
		},
	}

	marshaled, err := MarshalDeployment(original)
	if err != nil {
		t.Fatalf("MarshalDeployment: %v", err)
	}

	roundTripped, err := ParseDeployment(marshaled)
	if err != nil {
		t.Fatalf("ParseDeployment(MarshalDeployment(original)): %v\nmarshaled:\n%s", err, marshaled)
	}

	requireDeploymentsEquivalent(t, roundTripped, original)

	if got, want := roundTripped.Plugins[0].Digest, original.Plugins[0].Digest; got != want {
		t.Fatalf("Digest = %q after a Marshal/Parse round trip, want %q\nmarshaled:\n%s", got, want, marshaled)
	}
}

// --- Part A: GrantStated governs whether "grant" is written at all --------

// TestMarshalDeployment_OmitsGrantBlockWhenNotStated is Part A's core
// contract on the write side: an entry whose GrantStated is false — nobody
// has ever recorded a grant decision for it — must marshal with NO "grant"
// key at all, not an empty "grant": {}. This checks the actual bytes rather
// than a round-tripped struct, since a round trip through ParseDeployment
// cannot tell "grant omitted" apart from "grant": {} in the direction that
// matters here (both come back GrantStated true only for the latter).
func TestMarshalDeployment_OmitsGrantBlockWhenNotStated(t *testing.T) {
	dep := Deployment{Plugins: []Entry{{
		Name:   "legion-jira",
		Source: "./plugins/legion-jira",
		Tools:  []ToolAccept{{Name: "jira_search"}},
		// GrantStated left at its zero value: false.
	}}}

	data, err := MarshalDeployment(dep)
	if err != nil {
		t.Fatalf("MarshalDeployment: %v", err)
	}
	entry := decodeOneEntry(t, data)
	if _, present := entry["grant"]; present {
		t.Errorf("marshaled entry = %s, want no \"grant\" key at all when GrantStated is false", data)
	}
}

// TestMarshalDeployment_KeepsGrantBlockWhenStatedEvenIfEmpty is the other
// side: an entry whose GrantStated is true — an explicit `agent plugins
// deny`, or a hand-authorized entry with deliberately zero capabilities —
// keeps its "grant" key on the wire even though Grant.Capabilities is
// empty, because omitting it would silently turn a recorded decision back
// into "never authorized" on the very next read.
func TestMarshalDeployment_KeepsGrantBlockWhenStatedEvenIfEmpty(t *testing.T) {
	dep := Deployment{Plugins: []Entry{{
		Name:        "legion-jira",
		Source:      "./plugins/legion-jira",
		Grant:       GrantDecl{Capabilities: []string{}},
		GrantStated: true,
		Tools:       []ToolAccept{{Name: "jira_search"}},
	}}}

	data, err := MarshalDeployment(dep)
	if err != nil {
		t.Fatalf("MarshalDeployment: %v", err)
	}
	entry := decodeOneEntry(t, data)
	if _, present := entry["grant"]; !present {
		t.Errorf("marshaled entry = %s, want a \"grant\" key present: GrantStated is true", data)
	}

	roundTripped, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment(marshaled): %v", err)
	}
	if !roundTripped.Plugins[0].GrantStated {
		t.Errorf("GrantStated = false after round trip, want true")
	}
}

// TestMarshalDeployment_DraftedEntryRoundTripsWithGrantStatedFalse is the
// brief's own end-to-end demand for rule 6's first half: DraftEntry (install's
// entry constructor, draft.go) leaves GrantStated false by zero value, so an
// entry install writes must come back out of a full
// AddEntry/WriteDeployment/(disk)/ParseDeployment cycle still reporting
// GrantStated false — "never authorized" — rather than this package assuming
// DraftEntry's zero value survives MarshalDeployment/ParseDeployment
// untested. Rule 6's second half — after `agent plugins grant`, the same
// entry parses back with GrantStated true — is covered at the CLI layer by
// internal/cli's TestPluginsGrantAuthorizesAnEntryWithItsDeclaredCapabilities,
// which reads the real file `agent plugins grant` wrote back through this
// same ParseDeployment.
func TestMarshalDeployment_DraftedEntryRoundTripsWithGrantStatedFalse(t *testing.T) {
	pm := draftManifest()
	drafted, err := DraftEntry(pm, "https://example.com/plugin.wasm", draftDigest)
	if err != nil {
		t.Fatalf("DraftEntry: %v", err)
	}
	if drafted.GrantStated {
		t.Fatalf("DraftEntry's own GrantStated = true, want false (zero value); this test's premise is wrong")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	dep, err := AddEntry(Deployment{}, drafted)
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if err := WriteDeployment(path, dep); err != nil {
		t.Fatalf("WriteDeployment: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	entry := decodeOneEntry(t, data)
	if _, present := entry["grant"]; present {
		t.Errorf("written plugins.json = %s, want no \"grant\" key: DraftEntry leaves GrantStated false", data)
	}

	roundTripped, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment(%s): %v", path, err)
	}
	if roundTripped.Plugins[0].GrantStated {
		t.Errorf("GrantStated = true after Write/Parse, want false: an install-drafted entry must read back as " +
			"never authorized")
	}
}

// decodeOneEntry decodes data (a marshaled deployment with exactly one
// plugin) into a generic map, for tests that need to assert on JSON KEY
// PRESENCE rather than a decoded Go value — the distinction Part A's
// GrantStated exists to make, which a round trip through ParseDeployment
// cannot itself observe (see TestMarshalDeployment_OmitsGrantBlockWhenNotStated).
func decodeOneEntry(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var decoded struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if len(decoded.Plugins) != 1 {
		t.Fatalf("decoded %d plugins, want exactly 1: %s", len(decoded.Plugins), data)
	}
	return decoded.Plugins[0]
}

// --- BLOCKING-1 fix: MarshalDeployment refuses a Grant it would silently
// discard ---------------------------------------------------------------

// TestMarshalDeployment_RefusesUnstatedGrant is BLOCKING-1's core regression
// test: an Entry that carries a populated Grant while GrantStated is false
// must never marshal successfully, because the "grant" key would be omitted
// entirely (see entryDoc's doc comment) and the write would silently
// discard data the caller handed in while still reporting success. Each
// subtest covers exactly one GrantDecl field, independently — a guard that
// only checked Capabilities would let an AllowedHosts-only or
// AllowedPaths-only entry slip through and lose data just the same.
func TestMarshalDeployment_RefusesUnstatedGrant(t *testing.T) {
	cases := []struct {
		name  string
		grant GrantDecl
	}{
		{"capabilities", GrantDecl{Capabilities: []string{"log"}}},
		{"allowed_hosts", GrantDecl{AllowedHosts: []string{"api.example.com"}}},
		{"allowed_paths", GrantDecl{AllowedPaths: []string{"/tmp/legion"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := Deployment{Plugins: []Entry{{
				Name:   "legion-jira",
				Source: "./plugins/legion-jira",
				Grant:  tc.grant,
				// GrantStated left at its zero value: false.
				Tools: []ToolAccept{{Name: "jira_search"}},
			}}}

			_, err := MarshalDeployment(dep)
			if err == nil {
				t.Fatalf("MarshalDeployment: want an error for a populated %s grant with GrantStated "+
					"false, got nil", tc.name)
			}
			requireErrorContains(t, err, "legion-jira")
			requireErrorContains(t, err, "GrantStated")
		})
	}
}

// TestWriteDeployment_RefusesUnstatedGrant_LeavesPathUnchanged is
// BLOCKING-1's WriteDeployment-level regression test: the refusal must
// surface through WriteDeployment too, and must leave an existing
// plugins.json byte-for-byte unchanged rather than truncating it — the same
// "no partial write" guarantee rule 5's other refusal tests pin.
func TestWriteDeployment_RefusesUnstatedGrant_LeavesPathUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	original := Deployment{Plugins: []Entry{sampleEntry("legion-jira")}}
	if err := WriteDeployment(path, original); err != nil {
		t.Fatalf("seed WriteDeployment: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded %s: %v", path, err)
	}

	bad := Deployment{Plugins: []Entry{{
		Name:   "legion-notify",
		Source: "./plugins/legion-notify",
		Grant:  GrantDecl{Capabilities: []string{"log"}},
		// GrantStated left at its zero value: false.
		Tools: []ToolAccept{{Name: "notify_send"}},
	}}}

	if err := WriteDeployment(path, bad); err == nil {
		t.Fatalf("WriteDeployment: want an error for a populated grant with GrantStated false, got nil")
	} else {
		requireErrorContains(t, err, "legion-notify")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s after the refused write: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("WriteDeployment changed %s despite refusing to write it:\nbefore:\n%s\nafter:\n%s",
			path, before, after)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "plugins.json" {
		t.Fatalf("directory after the refused write = %v, want exactly [plugins.json] (no leftover temp file)",
			entries)
	}
}

// --- Rule 5: WriteDeployment writes atomically -----------------------------

// TestWriteDeployment_WriteThenReadBack_RoundTrips is the happy-path
// baseline: a write must produce a file ParseDeployment reads back with the
// same fields, and must leave no leftover temp artifact behind.
func TestWriteDeployment_WriteThenReadBack_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira"), sampleEntry("legion-notify")}}

	if err := WriteDeployment(path, dep); err != nil {
		t.Fatalf("WriteDeployment: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got, err := ParseDeployment(data)
	if err != nil {
		t.Fatalf("ParseDeployment(written file): %v", err)
	}
	requireDeploymentsEquivalent(t, got, dep)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "plugins.json" {
		t.Fatalf("directory after WriteDeployment = %v, want exactly [plugins.json]", entries)
	}
}

// TestWriteDeployment_FailedWriteLeavesPathUntouched proves
// WriteDeployment's atomicity without needing to catch a real crash
// mid-write: it forces the final rename to fail deterministically, then
// checks that path was not touched in the process.
//
// Holding path open (via a plain os.Open, which on Windows NTFS does not
// request FILE_SHARE_DELETE) blocks a rename onto it — verified empirically
// against this package's own WriteDeployment, which fails with "Access is
// denied" under exactly this setup. That forced failure happens at the very
// last step of an atomic write, after every byte of the new document has
// already been written to a temp file — so if WriteDeployment is truly
// atomic, path must come out of this holding exactly what it held going in.
// A direct write straight into path (open path itself with O_TRUNC, then
// write) is not blocked by the same held-open handle — FILE_SHARE_WRITE,
// unlike the delete-disposition a rename needs, is granted by a plain read
// — so it would succeed and silently replace path's content out from under
// the very reader holding it open: precisely the failure mode a temp file
// plus rename exists to rule out.
//
// This mechanism is Windows NTFS specific: POSIX rename(2) does not care
// whether another process holds the destination open, so held would not
// block the rename on Linux or macOS and the assertion below would never
// fire there — this test is skipped on those platforms in favor of
// TestWriteDeployment_CannotCreateTempFile_LeavesPathUntouched, which
// guards the same rule with a mechanism that actually fails on them.
func TestWriteDeployment_FailedWriteLeavesPathUntouched(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("forced-rename-failure mechanism is Windows NTFS specific")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	original := Deployment{Plugins: []Entry{sampleEntry("legion-jira")}}
	if err := WriteDeployment(path, original); err != nil {
		t.Fatalf("seed WriteDeployment: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded %s: %v", path, err)
	}

	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s to hold it: %v", path, err)
	}
	defer held.Close()

	replacement := Deployment{Plugins: []Entry{sampleEntry("legion-notify")}}
	if err := WriteDeployment(path, replacement); err == nil {
		t.Fatalf("WriteDeployment succeeded while %s was held open; want an error from the blocked rename", path)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s after the failed write: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("WriteDeployment changed %s despite failing to write it:\nbefore:\n%s\nafter:\n%s",
			path, before, after)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "plugins.json" {
		t.Fatalf("directory after the failed write = %v, want exactly [plugins.json] (no leftover temp file)",
			entries)
	}
}

// TestWriteDeployment_CannotCreateTempFile_LeavesPathUntouched is rule 5's
// platform-independent companion to
// TestWriteDeployment_FailedWriteLeavesPathUntouched above: it forces the
// write to fail by a mechanism that works the same way on every POSIX
// platform this package's tests run on, so rule 5 has a working assertion
// on Linux — the only platform CI actually runs this package's tests on —
// where the held-open-handle mechanism above does not apply.
//
// Removing dir's write bit means os.CreateTemp can no longer create a new
// file there (adding a directory entry needs write permission on the
// directory itself, not on any file inside it), so a correctly atomic
// WriteDeployment must fail before it ever touches path, leaving path
// holding whatever it held before. A mutated WriteDeployment that instead
// wrote directly into path (os.WriteFile(path, data, ...), or
// os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, ...) then Write) would not need
// to create anything in dir at all: truncating and rewriting an EXISTING
// file only needs write permission on the file itself, which this test
// deliberately leaves untouched — so that mutation succeeds here and
// silently replaces path's content, exactly the defect rule 5 exists to
// rule out. This is what makes the test genuinely redden under the
// direct-write mutation on Linux, rather than merely failing early for an
// unrelated reason the way making path itself a directory would (a direct
// write into a directory fails too, so that shape cannot tell the two
// implementations apart).
//
// Skipped on Windows: os.Chmod does not restrict directory writes there, so
// this mechanism has no effect on that platform — Windows already has its
// own dedicated test above.
func TestWriteDeployment_CannotCreateTempFile_LeavesPathUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Chmod does not restrict directory writes on Windows; see TestWriteDeployment_FailedWriteLeavesPathUntouched")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	original := Deployment{Plugins: []Entry{sampleEntry("legion-jira")}}
	if err := WriteDeployment(path, original); err != nil {
		t.Fatalf("seed WriteDeployment: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded %s: %v", path, err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod %s to read-only: %v", dir, err)
	}
	defer func() {
		// Restore before t.TempDir()'s own cleanup tries to remove dir,
		// which needs write permission on it to unlink plugins.json.
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("restore chmod on %s: %v", dir, err)
		}
	}()

	replacement := Deployment{Plugins: []Entry{sampleEntry("legion-notify")}}
	if err := WriteDeployment(path, replacement); err == nil {
		t.Fatalf("WriteDeployment succeeded with %s not writable; want an error from the blocked temp-file create",
			dir)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s after the failed write: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("WriteDeployment changed %s despite failing to write it:\nbefore:\n%s\nafter:\n%s",
			path, before, after)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != "plugins.json" {
		t.Fatalf("directory after the failed write = %v, want exactly [plugins.json] (no leftover temp file)",
			entries)
	}
}

// TestWriteDeployment_PreservesExistingFilePermissions is SHOULD-FIX-4's
// regression test: os.CreateTemp always creates its temp file mode 0600,
// and without an explicit chmod before the rename, path's mode would
// silently narrow to 0600 on every write regardless of what it was before.
// plugins.json is operator-managed config, often git-controlled and
// sometimes read by a different service identity than the one running
// "agent plugins install" — a silent permission narrowing produces no error
// at write time and only surfaces later as an access failure somewhere else
// entirely.
//
// Gated to non-Windows: Windows does not expose the POSIX-style permission
// bits this test asserts on (chmod there mostly toggles the read-only
// attribute), so there is nothing meaningful to assert there.
func TestWriteDeployment_PreservesExistingFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce the POSIX permission bits this test asserts on")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	dep := Deployment{Plugins: []Entry{sampleEntry("legion-jira")}}
	if err := WriteDeployment(path, dep); err != nil {
		t.Fatalf("seed WriteDeployment: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}

	replacement := Deployment{Plugins: []Entry{sampleEntry("legion-notify")}}
	if err := WriteDeployment(path, replacement); err != nil {
		t.Fatalf("WriteDeployment: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got, want := info.Mode().Perm(), fs.FileMode(0o640); got != want {
		t.Fatalf("mode of %s after WriteDeployment = %v, want %v (os.CreateTemp's 0600 must not silently "+
			"replace the target's existing permission bits)", path, got, want)
	}
}

// --- Rule 6: WriteDeployment re-parses its own output before committing ---

// TestWriteDeployment_RefusesToWriteWhatItCannotReadBack constructs a
// Deployment directly (bypassing AddEntry, which would refuse this) with
// two entries sharing a Name. Deployment.Plugins carries no invariant of
// its own, and MarshalDeployment does not validate — it only serializes —
// so this is exactly the kind of document WriteDeployment's own
// ParseDeployment self-check exists to catch: ParseDeployment refuses a
// duplicate name, and WriteDeployment must refuse to write the file rather
// than leave the deployment holding a document it cannot read back.
func TestWriteDeployment_RefusesToWriteWhatItCannotReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")

	dep := Deployment{Plugins: []Entry{
		{Name: "dup", Source: "./plugins/a", Grant: GrantDecl{Capabilities: []string{}}, Tools: []ToolAccept{{Name: "t"}}},
		{Name: "dup", Source: "./plugins/b", Grant: GrantDecl{Capabilities: []string{}}, Tools: []ToolAccept{{Name: "t"}}},
	}}

	err := WriteDeployment(path, dep)
	requireErrorContains(t, err, "dup")

	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("WriteDeployment left a file at %s despite refusing to write it", path)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat %s: %v", path, statErr)
	}
}
