package trustlist

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// keyEntry pairs one entry of a keyring document's "keys" array with the id it
// declares: the bytes so the entry can be carried into the merged document
// exactly as they arrived, the id so entries can be de-duplicated and ordered
// without being re-encoded. Re-encoding an entry would make this file a second
// opinion about what a keyring entry looks like, next to sign's.
type keyEntry struct {
	id  sign.KeyID
	raw json.RawMessage
}

// keyEntriesOf pulls the entries of a keyring document's "keys" array out as
// their original bytes. side names which half of the trust set doc came from,
// so an error says which document to go and look at.
//
// An empty or absent "keys" array is an error rather than "this side
// contributes nothing": sign.ParseKeyring refuses a keyring document with no
// keys, so a document that reached here with none is malformed, and reading it
// as an empty contribution would let a broken file quietly shrink the trust
// set instead of stopping the merge.
func keyEntriesOf(doc json.RawMessage, side string) ([]keyEntry, error) {
	var shape keyringShape
	if err := json.Unmarshal(doc, &shape); err != nil {
		return nil, fmt.Errorf("read the %s keyring document: %w", side, err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(shape.Keys, &raws); err != nil {
		return nil, fmt.Errorf("read the %s keyring's keys: %w", side, err)
	}
	var ids []struct {
		ID sign.KeyID `json:"id"`
	}
	if err := json.Unmarshal(shape.Keys, &ids); err != nil {
		return nil, fmt.Errorf("read the ids of the %s keyring's keys: %w", side, err)
	}
	if len(raws) != len(ids) {
		// Two decodes of the same array disagreeing on its length is not a
		// case to pick a winner for; it means one of them read something this
		// function does not understand.
		return nil, fmt.Errorf("read the %s keyring's keys: %d entries by bytes but %d by id",
			side, len(raws), len(ids))
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("the %s keyring document registers no keys; a keyring with an empty "+
			"keys list is not a trust set this deployment could act on", side)
	}
	entries := make([]keyEntry, 0, len(raws))
	for i, raw := range raws {
		if ids[i].ID == "" {
			return nil, fmt.Errorf("the %s keyring's keys[%d] has no id", side, i)
		}
		entries = append(entries, keyEntry{id: ids[i].ID, raw: raw})
	}
	return entries, nil
}

// sortedIDs returns the keys of m in ascending order.
func sortedIDs[V any](m map[sign.KeyID]V) []sign.KeyID {
	ids := make([]sign.KeyID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Merge combines the deployment's local keyring with the fetched trust list
// into the single trust set a package is judged against, and returns the
// display names that go with it.
//
// Both halves answer the same question — which signing keys does this
// deployment trust — from different places: the local file serves an intranet
// or an offline install, the list serves the public ecosystem. So registrations
// are unioned: both kinds of plugin have to be loadable on one machine. When
// both halves register the same id, the local entry is the one kept; a list
// fetched over the network must not be able to redefine a key the operator
// wrote down on this machine.
//
// Revocations are unioned too, and that is the half that matters: either side
// saying a key is revoked makes it revoked. Letting one side's silence cancel
// the other's revocation would make revocation depend on where it was recorded,
// and revocation is the one thing in this design that must not weaken as more
// sources appear. A revocation is kept even when no surviving entry registers
// that id — sign.ParseKeyring accepts such a record (it checks only that a
// key_id is present, unique and that revoked_at, when given, is RFC 3339), and
// dropping it would make "delete the public key" an antidote to revoking it.
//
// A nil result means this deployment has no trust set at all. That is not
// "signatures are not checked" — it is "no key is recognised", and every
// package judged against it comes out unsigned.
//
// The merged set is assembled by writing a keyring document and handing it to
// sign.ParseKeyring, never by reasoning about trust outside sign.Keyring: one
// rule implemented twice drifts, and the direction it drifts is always some
// path forgetting to consult the revocations. This is the same route
// assembleKeyring takes for the same reason.
//
// # Why the local half arrives as bytes while the list half arrives as a Trust
//
// sign.Keyring's exported surface is IDs, RevokedIDs, Revoked and Verify —
// there is no way to read a public key back out of one. A *sign.Keyring
// therefore cannot contribute its registrations to a new keyring document, so
// the local half is taken as the bytes of the keyring document itself. That is
// also how the rest of this package moves a trust set around: assembleKeyring
// carries the keys segment through as json.RawMessage rather than re-encoding
// it, because the bytes are what a signature covers.
//
// The list half needs both of Trust's keyring fields because each carries a
// half the other does not: t.KeyringRaw is the only place the list's public
// keys survive as bytes, while t.Keyring is the only place this machine's
// accumulated revocation record appears (assembleKeyring unions it into the
// keyring it builds; the raw document holds only what that one list wrote
// down). Reading revocations off the raw document instead would drop every
// revocation this machine has recorded since — exactly the weakening the
// union above exists to prevent. A Trust carrying one field without the other
// is refused rather than half-used, for the same reason.
//
// # localRaw must already have been through sign.ParseKeyring
//
// Wherever Merge is called, localRaw has to be a keyring document that has
// already been parsed by sign.ParseKeyring. That is an obligation on the
// caller; the parameter is a bare json.RawMessage, so nothing about the type
// enforces it. The reason is the de-duplication above: it keeps one entry per
// id and one revocation per key_id, while sign.ParseKeyring hard-refuses a
// document that registers the same id twice ("key id %q appears twice") or
// revokes the same key_id twice ("key id %q is revoked twice"). Hand this
// function an unvetted keyring document and the de-duplication quietly
// dissolves those two rules, because the second copy is dropped here before
// sign.ParseKeyring ever sees the pair. The de-duplication exists only to
// serve "the record already held wins"; it takes on no validation duty.
// assembleKeyring carries the identical obligation for the same reason.
//
// # Every key revoked is an error, not an empty trust set
//
// sign.ParseKeyring refuses a trust set in which every registered key is
// revoked. The union taken here can produce exactly that: the revocations
// reaching this function are cumulative — t.Keyring carries every revocation
// this machine has recorded, not only the ones the current list names — while
// the keys are whatever the two documents register today, so the revoked side
// can come to cover all of them. Merge returns that error rather than
// swallowing it. The error must not be read as the nil result above: nil means
// no trust set at all because neither half was present, while this error means
// a half was present and what the halves add up to is not a trust set.
func Merge(localRaw json.RawMessage, t Trust) (*sign.Keyring, map[sign.KeyID]string, error) {
	switch {
	case t.Keyring == nil && len(t.KeyringRaw) > 0:
		return nil, nil, fmt.Errorf("merge trust sets: the trust list half has a keyring document but no " +
			"parsed keyring; its revocations live only in the parsed one, so merging from the document " +
			"alone would silently drop them")
	case t.Keyring != nil && len(t.KeyringRaw) == 0:
		return nil, nil, fmt.Errorf("merge trust sets: the trust list half has a parsed keyring but no " +
			"keyring document; its public keys live only in the document, so merging from the parsed " +
			"keyring alone would silently drop every key it registers")
	}

	// Local first, list second, first one seen wins — for registrations and
	// for revocations alike. It is the same precedence assembleKeyring and
	// revokedSet.mergeFrom use: the record already held is the one kept, so a
	// later document can add to what is known and never rewrite it.
	keys := map[sign.KeyID]json.RawMessage{}
	revoked := map[sign.KeyID]rawRevocationEntry{}
	addKey := func(e keyEntry) {
		if _, dup := keys[e.id]; !dup {
			keys[e.id] = e.raw
		}
	}
	addRevocation := func(e rawRevocationEntry) {
		if _, dup := revoked[e.KeyID]; !dup {
			revoked[e.KeyID] = e
		}
	}

	if len(localRaw) > 0 {
		entries, err := keyEntriesOf(localRaw, "local")
		if err != nil {
			return nil, nil, fmt.Errorf("merge trust sets: %w", err)
		}
		for _, e := range entries {
			addKey(e)
		}
		var shape keyringShape
		if err := json.Unmarshal(localRaw, &shape); err != nil {
			return nil, nil, fmt.Errorf("merge trust sets: read the local keyring's revocations: %w", err)
		}
		for i, e := range shape.Revoked {
			// The same shape rules parseRevokedSet and mergeFrom apply, applied
			// once more at this entrance. A bad revoked_at that got in here would
			// surface later as a sign.ParseKeyring complaint about the assembled
			// document, pointing an operator at a document nobody wrote.
			if err := e.validate(); err != nil {
				return nil, nil, fmt.Errorf("merge trust sets: the local keyring's revoked[%d] %w", i, err)
			}
			addRevocation(e)
		}
	}

	if t.Keyring != nil {
		entries, err := keyEntriesOf(t.KeyringRaw, "trust list")
		if err != nil {
			return nil, nil, fmt.Errorf("merge trust sets: %w", err)
		}
		for _, e := range entries {
			addKey(e)
		}
		for _, id := range t.Keyring.RevokedIDs() {
			record, ok := t.Keyring.Revoked(id)
			if !ok {
				// RevokedIDs and Revoked read the same map; an id from the first
				// that the second does not know is a broken invariant, not a
				// revocation to skip.
				return nil, nil, fmt.Errorf("merge trust sets: the trust list's keyring lists key %q as "+
					"revoked but has no record for it", id)
			}
			entry := rawRevocationEntry{KeyID: id, Reason: record.Reason}
			if !record.At.IsZero() {
				entry.RevokedAt = record.At.Format(time.RFC3339)
			}
			addRevocation(entry)
		}
	}

	// No keys means neither half was present: a half that IS present either
	// contributes at least one key or stops the merge with an error
	// (keyEntriesOf refuses a document that registers none). So this is the
	// "no trust set at all" case the doc comment describes, not a merge that
	// quietly came out empty.
	if len(keys) == 0 {
		return nil, map[sign.KeyID]string{}, nil
	}

	// Both arrays go out sorted by id so this document is a deterministic
	// function of the two halves. Go randomises map iteration order, so
	// unsorted they would come out in a different byte order on every run over
	// the same inputs — and that order is observable, because sign.ParseKeyring
	// below walks each array in order and refuses at the first entry it cannot
	// accept. Which of several unacceptable entries gets named, and therefore
	// which error this merge fails with, would otherwise be decided afresh on
	// every run.
	keyList := make([]json.RawMessage, 0, len(keys))
	for _, id := range sortedIDs(keys) {
		keyList = append(keyList, keys[id])
	}
	revList := make([]rawRevocationEntry, 0, len(revoked))
	for _, id := range sortedIDs(revoked) {
		revList = append(revList, revoked[id])
	}

	doc := struct {
		Keys    []json.RawMessage    `json:"keys"`
		Revoked []rawRevocationEntry `json:"revoked,omitempty"`
	}{Keys: keyList, Revoked: revList}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("merge trust sets: encode the merged keyring document: %w", err)
	}
	merged, err := sign.ParseKeyring(data)
	if err != nil {
		return nil, nil, fmt.Errorf("merge trust sets: %w", err)
	}

	names := make(map[sign.KeyID]string, len(t.Publishers))
	for id, p := range t.Publishers {
		names[id] = p.DisplayName
	}
	return merged, names, nil
}
