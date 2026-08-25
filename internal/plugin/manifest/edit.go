package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AddEntry returns a copy of dep with entry appended after its existing
// Plugins, leaving their content and order untouched (new entries always go
// at the end — order is how an operator navigates plugins.json by hand).
//
// AddEntry refuses to add an entry whose Name already appears in dep,
// naming the name: silently overwriting an existing entry would erase
// whatever authorization decision (Entry.Enabled, Entry.Grant) an operator
// already recorded for it, with nothing in the diff to show a decision was
// ever made. A caller that means to change an existing entry must use
// UpdateEntry instead.
func AddEntry(dep Deployment, entry Entry) (Deployment, error) {
	for _, existing := range dep.Plugins {
		if existing.Name == entry.Name {
			return Deployment{}, fmt.Errorf("add plugin %q: an entry with this name already exists in the "+
				"deployment manifest; overwriting it would silently erase whatever authorization decision was "+
				"already recorded for it (use UpdateEntry to change an existing entry instead)", entry.Name)
		}
	}

	plugins := make([]Entry, len(dep.Plugins), len(dep.Plugins)+1)
	copy(plugins, dep.Plugins)
	plugins = append(plugins, entry)
	return Deployment{Plugins: plugins}, nil
}

// UpdateEntry returns a copy of dep with the entry named name replaced by
// the result of calling mutate on its current value. Every other entry, and
// the overall order of dep.Plugins, is left untouched.
//
// UpdateEntry refuses an unknown name, naming both the requested name and
// every name that does exist in dep, so a caller can see immediately what
// it should have typed instead of re-reading plugins.json to find out. If
// mutate itself returns an error, UpdateEntry wraps and returns it without
// modifying dep.
func UpdateEntry(dep Deployment, name string, mutate func(Entry) (Entry, error)) (Deployment, error) {
	index := -1
	names := make([]string, len(dep.Plugins))
	for i, existing := range dep.Plugins {
		names[i] = existing.Name
		if existing.Name == name {
			index = i
		}
	}
	if index == -1 {
		existing := "(none)"
		if len(names) > 0 {
			existing = strings.Join(names, ", ")
		}
		return Deployment{}, fmt.Errorf("update plugin %q: no entry with this name exists in the deployment "+
			"manifest; existing entries are: %s", name, existing)
	}

	updated, err := mutate(dep.Plugins[index])
	if err != nil {
		return Deployment{}, fmt.Errorf("update plugin %q: %w", name, err)
	}

	plugins := make([]Entry, len(dep.Plugins))
	copy(plugins, dep.Plugins)
	plugins[index] = updated
	return Deployment{Plugins: plugins}, nil
}

// entryDoc is the encode-side counterpart of rawEntry (manifest.go): the
// same JSON shape, built for marshaling rather than decoding. It exists
// because Entry itself carries no JSON struct tags — Entry is not meant to
// be (de)serialized directly, only rawEntry (decode) and entryDoc (encode)
// are — and because getting a few of its tags wrong would reopen exactly
// the hole rawEntry.Enabled exists to close:
//
//   - Enabled is a plain bool, not a pointer: by the time this package's
//     caller holds an Entry, ParseDeployment has already collapsed
//     "enabled omitted" into Enabled == true (see Entry.Enabled). Enabled
//     is deliberately NOT tagged omitempty here — encoding/json's omitempty
//     elides a bool field when it is false, and eliding Enabled: false would
//     make the field absent, which ParseDeployment reads back as enabled
//     (true) rather than disabled. An omitempty tag on this one field would
//     silently promote every disabled or not-yet-authorized plugin to
//     enabled on its very next save/load round trip.
//   - Digest and Config are tagged omitempty because ParseDeployment
//     tolerates their absence (a local entry must carry no Digest at all;
//     Config is optional pass-through), and omitting a zero value here
//     matches this repo's existing plugins.json style (see
//     testdata/plugins.json, where entries with no config simply have no
//     "config" key) without changing what a round trip decodes back to:
//     an omitted "digest"/"config" and an empty-string/nil one decode to
//     the same Go zero value either way.
//   - Grant and Tools carry no omitempty: GrantDecl is a struct, for which
//     omitempty is a no-op in encoding/json, and Tools' nil/non-nil
//     distinction survives a plain (non-omitempty) round trip on its own
//     (encoding a nil slice produces JSON null, which decodes back to nil;
//     an empty slice produces "[]", which decodes back to a non-nil empty
//     slice) — adding omitempty here would only change output shape, not
//     correctness, so it is left off to keep the tag list aligned with
//     rawEntry's.
type entryDoc struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Digest  string          `json:"digest,omitempty"`
	Enabled bool            `json:"enabled"`
	Grant   GrantDecl       `json:"grant"`
	Tools   []ToolAccept    `json:"tools"`
	Config  json.RawMessage `json:"config,omitempty"`
}

// deploymentDoc is the encode-side counterpart of rawDeployment
// (manifest.go); see entryDoc.
type deploymentDoc struct {
	Plugins []entryDoc `json:"plugins"`
}

// MarshalDeployment renders dep as plugins.json bytes, indented two spaces
// to match this repo's other JSON config writers (e.g.
// internal/plugin/sign.MarshalKeyring, internal/sessionstate/checkpoint.go
// both use json.MarshalIndent(v, "", "  ")).
//
// The result is defined to round-trip through ParseDeployment back to a
// Deployment holding the same fields dep started with: MarshalDeployment
// writes every field ParseDeployment reads, through entryDoc, entryDoc's
// mirrored encode-side counterpart of the rawEntry type ParseDeployment
// decodes through (see entryDoc's doc comment for the traps that mirroring
// has to avoid, chief among them Entry.Enabled's absent-vs-false
// distinction).
func MarshalDeployment(dep Deployment) ([]byte, error) {
	doc := deploymentDoc{Plugins: make([]entryDoc, len(dep.Plugins))}
	for i, entry := range dep.Plugins {
		doc.Plugins[i] = entryDoc{
			Name:    entry.Name,
			Source:  entry.Source,
			Digest:  entry.Digest,
			Enabled: entry.Enabled,
			Grant:   entry.Grant,
			Tools:   entry.Tools,
			Config:  entry.Config,
		}
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal deployment manifest: %w", err)
	}
	return data, nil
}

// WriteDeployment marshals dep (via MarshalDeployment) and writes it to
// path.
//
// Before writing anything, WriteDeployment re-parses its own marshaled
// bytes with ParseDeployment and refuses to proceed if that fails: writing
// out a plugins.json this package cannot read back would leave the
// deployment's own tooling unable to load its target state on the very next
// start. This check runs before any filesystem write, so a dep that fails
// it (for example, one a caller assembled by hand with two entries sharing
// a Name, bypassing AddEntry) leaves path completely untouched.
//
// The write itself is atomic: a temp file is created beside path, in the
// same directory (so the final rename is a same-filesystem move, which is
// atomic with respect to a concurrent reader on every platform this package
// runs on), written and synced, and then renamed over path. A process
// killed at any point before the rename leaves path exactly as it was
// before the call; a process killed during or after the rename leaves path
// holding either the old document or the complete new one, never a
// half-written mixture that the next startup would fail to parse.
func WriteDeployment(path string, dep Deployment) (err error) {
	data, merr := MarshalDeployment(dep)
	if merr != nil {
		return fmt.Errorf("write deployment manifest %q: %w", path, merr)
	}
	if _, perr := ParseDeployment(data); perr != nil {
		return fmt.Errorf("write deployment manifest %q: refusing to write a document this package cannot "+
			"read back: %w", path, perr)
	}

	dir := filepath.Dir(path)
	tmp, cerr := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if cerr != nil {
		return fmt.Errorf("write deployment manifest %q: create temp file beside it: %w", path, cerr)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err == nil {
			return
		}
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temp file %q: %w", tmpPath, rmErr))
		}
	}()

	if _, werr := tmp.Write(data); werr != nil {
		failure := fmt.Errorf("write deployment manifest %q: write temp file %q: %w", path, tmpPath, werr)
		if cerr := tmp.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close temp file %q: %w", tmpPath, cerr))
		}
		return failure
	}
	if serr := tmp.Sync(); serr != nil {
		failure := fmt.Errorf("write deployment manifest %q: sync temp file %q: %w", path, tmpPath, serr)
		if cerr := tmp.Close(); cerr != nil {
			failure = errors.Join(failure, fmt.Errorf("close temp file %q: %w", tmpPath, cerr))
		}
		return failure
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("write deployment manifest %q: close temp file %q: %w", path, tmpPath, cerr)
	}
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		return fmt.Errorf("write deployment manifest %q: rename temp file into place: %w", path, rerr)
	}
	return nil
}
