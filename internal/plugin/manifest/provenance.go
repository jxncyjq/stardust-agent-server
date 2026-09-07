package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// ErrUnsignedNotAccepted marks a refusal whose reason is that a package
// carries no endorsement from any registered publisher — ProvenanceUnsigned
// met by a policy that does not let it through.
//
// It is a sentinel, separate from ErrRevokedPublisher, because the two
// refusals differ in whether anything an operator does can lift them: an
// unsigned package's very same bytes become loadable once someone accepts
// them, while a package signed by a revoked key does not become loadable by
// anyone accepting anything. A refusal that cannot tell those two apart can
// only ever offer one remedy, and for one of the two that remedy is a lie.
var ErrUnsignedNotAccepted = errors.New("plugin package is unsigned and was never accepted")

// ErrRevokedPublisher marks a refusal whose reason is that a package is
// signed by a key the deployment has withdrawn trust from —
// ProvenanceRevoked.
//
// "I do not demand an endorsement" and "this key was trusted and is not any
// more" are different sentences: the first is a requirement not imposed, the
// second is a decision already taken. Keeping this sentinel distinct from
// ErrUnsignedNotAccepted is what lets a policy treat them differently.
var ErrRevokedPublisher = errors.New("plugin package is signed by a revoked key")

// ProvenanceState is what a package's signature says about who stands behind
// its bytes, on this machine, right now.
//
// The name is deliberately not Trust: internal/plugin/trustlist already has a
// Trust (its assembled trust set plus how fresh the fetched list is — see
// trustlist.Trust), and a caller that reads both packages would otherwise
// have two unrelated concepts under one name in a single import block.
type ProvenanceState int

const (
	// ProvenanceUnsigned covers both "there is no plugin.sig" and "there is
	// one, but the key that made it is not in this machine's trust set".
	//
	// They are one state because they mean the same thing to whoever has to
	// decide: no registered publisher stands behind these bytes. Splitting
	// them would add a row to every policy table without adding a decision.
	ProvenanceUnsigned ProvenanceState = iota

	// ProvenanceRegistered means plugin.sig verified against a key that is in
	// this machine's trust set and is not revoked.
	ProvenanceRegistered

	// ProvenanceRevoked means plugin.sig names a key this deployment has
	// revoked. The signature itself is not checked in that case: no outcome of
	// checking it could make a revoked key trusted again, and the revocation
	// record is the more useful thing to report.
	ProvenanceRevoked
)

// String returns the state's lowercase name. A value outside the three
// defined states renders as ProvenanceState(<number>) rather than being
// folded into one of them — a default branch that answered "unsigned" for an
// undefined value would be a wrong verdict wearing a right one's name.
func (s ProvenanceState) String() string {
	switch s {
	case ProvenanceUnsigned:
		return "unsigned"
	case ProvenanceRegistered:
		return "registered"
	case ProvenanceRevoked:
		return "revoked"
	default:
		return fmt.Sprintf("ProvenanceState(%d)", int(s))
	}
}

// Provenance is LoadPackage's verdict about one package's origin. It reports;
// it does not decide. Whether an unsigned package may load is the caller's
// policy — the same boundary LoadPackage has always drawn around its trust
// set, only now expressed as a verdict the caller receives instead of an
// error it cannot qualify.
type Provenance struct {
	State ProvenanceState

	// KeyID names the key that signed the package, and is set only when this
	// machine recognises that key — that is, for ProvenanceRegistered and
	// ProvenanceRevoked. It stays empty for ProvenanceUnsigned even when a
	// plugin.sig was present.
	//
	// An id that came out of a plugin.sig this machine could not place lands
	// in UnrecognizedKeyID instead, never here. The two are separate fields so
	// that the difference is a matter of which field was read rather than of
	// remembering to check State first: a non-empty KeyID is always a key this
	// machine has a record of.
	KeyID sign.KeyID

	// UnrecognizedKeyID names the key a plugin.sig claims to have been made
	// with, when this machine's trust set holds no such key. It is set only
	// for ProvenanceUnsigned, and only when a plugin.sig was present and
	// parsed; a package carrying no signature leaves it empty, and so does a
	// deployment with no trust set to place a signature against.
	//
	// It is NOT an endorsement and NOT a fact this machine can corroborate:
	// whoever wrote plugin.sig chose the string, and nothing here vouches for
	// it. It is carried anyway because a refusal that cannot say which key was
	// offered cannot tell a registration that lapsed, a key id typed wrong,
	// and an attacker apart, and those three ask for three different
	// responses. Rendering it therefore has to say, in the same breath, that
	// this machine does not know the key — the alternative to naming it is not
	// a safer message but a message that fits all three cases equally badly.
	//
	// See KeyID for why this is a field of its own rather than that one.
	UnrecognizedKeyID sign.KeyID

	// Publisher is the display name TrustInput.Publishers carries for KeyID.
	// It is empty when that map has no name for the key, and always empty for
	// ProvenanceUnsigned: that state has no KeyID, and UnrecognizedKeyID is by
	// definition outside the trust set the names go with.
	Publisher string

	// Reason and RevokedAt are what the operator wrote down when the key was
	// revoked, carried so that a refusal can explain itself. Both are optional
	// in a revocation record (see sign.Revocation), so both may be zero even
	// for ProvenanceRevoked.
	Reason    string
	RevokedAt time.Time
}

// TrustInput is everything LoadPackage needs to judge a package's provenance:
// the trust set, and the display names that go with the keys in it.
//
// A zero TrustInput (no keyring) is a deployment with no trust set at all.
// That makes every package ProvenanceUnsigned — NOT unchecked. The difference
// matters: "unchecked" would be a verdict of "fine", and this type has no way
// to say that.
type TrustInput struct {
	Keyring *sign.Keyring

	// Publishers maps a key id to the display name a human reads. It is a
	// plain string map rather than a trustlist type so that this package need
	// not know internal/plugin/trustlist exists; a display name is all it uses.
	//
	// A key with no entry here is not an error: the name is decoration on a
	// verdict, and a missing one costs a reader some context but changes no
	// decision.
	Publishers map[sign.KeyID]string
}

// ManifestDigest returns the digest of dir/plugin.json's raw bytes, in the
// exact format Entry.AcceptedUnsigned is held to: the literal "sha256:"
// followed by 64 lowercase hex digits (digestPattern).
//
// It hashes the bytes as they are on disk rather than a re-encoded manifest,
// for the same reason assessProvenance verifies a signature over the raw bytes:
// a decode-then-re-encode round trip is only stable if both sides agree on a
// byte-identical JSON encoding, and an acceptance that moved when a field was
// reordered would alarm about a package nobody changed.
//
// Hashing plugin.json is enough to pin the code that will run, because
// plugin.json carries plugin.wasm's sha256 and LoadPackage compares the wasm
// bytes against that declared digest on every load.
func ManifestDigest(dir string) (string, error) {
	path := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("digest plugin manifest %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DescribeRevocation renders what an operator wrote down about a revocation, as
// a phrase meant to be appended to a sentence that has already said the key was
// revoked — " at 2026-08-29T10:00:00Z (laptop stolen)", or the empty string
// when the record carries neither.
//
// The empty string is not a fallback: Provenance.Reason and Provenance.RevokedAt
// are both optional in a revocation record (see sign.Revocation), so a record
// with neither is a legitimate one, and the sentence it is appended to still
// says everything a refusal must say.
//
// It is a function here rather than a rendering each refusal writes for itself
// so that every refusal about a revoked key reads the same, whether it comes
// from a convergence or from an install.
func DescribeRevocation(prov Provenance) string {
	switch {
	case !prov.RevokedAt.IsZero() && prov.Reason != "":
		return fmt.Sprintf(" at %s (%s)", prov.RevokedAt.Format(time.RFC3339), prov.Reason)
	case !prov.RevokedAt.IsZero():
		return fmt.Sprintf(" at %s", prov.RevokedAt.Format(time.RFC3339))
	case prov.Reason != "":
		return fmt.Sprintf(" (%s)", prov.Reason)
	default:
		return ""
	}
}

// assessProvenance reads dir/plugin.sig, if any, and judges what it says about
// manifestData's origin.
//
// It returns an error only for failures that are NOT verdicts:
//
//   - plugin.sig exists but cannot be read (a permission fault, or it is not a
//     regular file) — an environment problem, not a statement about trust, so
//     it is not wrapped in ErrUntrustedPackage;
//   - plugin.sig is malformed, or its signature does not verify against a key
//     this machine trusts — the bytes and the signature disagree, which is a
//     tampering report rather than an absence of endorsement. Both wrap
//     ErrUntrustedPackage.
//
// Everything else is a Provenance. In particular a MISSING plugin.sig is
// ProvenanceUnsigned rather than an error, which is the change this whole type
// exists for: whether that is allowed is the caller's policy.
func assessProvenance(dir string, manifestData []byte, trust TrustInput) (Provenance, error) {
	if trust.Keyring == nil {
		// No trust set: nothing can be verified against anything, so no key
		// can be recognised. plugin.sig is not even read — with no keys, its
		// contents could not change the verdict — which is also why
		// UnrecognizedKeyID stays empty here: naming a key that was never
		// looked at would report a comparison that never happened.
		return Provenance{State: ProvenanceUnsigned}, nil
	}

	sigPath := filepath.Join(dir, "plugin.sig")
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Provenance{State: ProvenanceUnsigned}, nil
		}
		// NOT ErrUntrustedPackage: see that sentinel's doc comment.
		return Provenance{}, fmt.Errorf("read plugin.sig: %w", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		return Provenance{}, fmt.Errorf("parse plugin.sig: %w: %w", ErrUntrustedPackage, err)
	}

	// Revocation is checked BEFORE both of the checks below it, and each of
	// those two orderings buys something different.
	//
	// Before the membership check, because a revocation record does not
	// require the key to still be listed among the keys (sign.ParseKeyring
	// accepts one for an id "keys" no longer carries). Asking "is it known?"
	// first would answer no for such a key and hand it the ProvenanceUnsigned
	// verdict — the one verdict an operator is offered a way to accept. A
	// revocation would then be undone by removing a public key.
	//
	// Before sign.Keyring.Verify, because Verify does its own revocation check
	// first and reports one as an error. That error is the right answer for a
	// caller that only wants pass/fail; here it would destroy the verdict — a
	// revocation would arrive as an untrusted-package error carrying no state,
	// and the time and the reason would be readable only by scraping a message.
	if revocation, gone := trust.Keyring.Revoked(sig.KeyID); gone {
		return Provenance{
			State:     ProvenanceRevoked,
			KeyID:     sig.KeyID,
			Publisher: trust.Publishers[sig.KeyID],
			Reason:    revocation.Reason,
			RevokedAt: revocation.At,
		}, nil
	}
	if !slices.Contains(trust.Keyring.IDs(), sig.KeyID) {
		// Signed by a key this machine does not know. Same verdict as no
		// signature at all — no registered publisher stands behind these bytes
		// either way — so KeyID stays empty and the offered id is reported as
		// UnrecognizedKeyID. That field is what keeps the two ways of reaching
		// this verdict distinguishable to whoever has to act on it: see
		// Provenance.UnrecognizedKeyID.
		return Provenance{State: ProvenanceUnsigned, UnrecognizedKeyID: sig.KeyID}, nil
	}
	if err := trust.Keyring.Verify(sig, manifestData); err != nil {
		return Provenance{}, fmt.Errorf("verify plugin.json signature: %w: %w", ErrUntrustedPackage, err)
	}
	return Provenance{
		State:     ProvenanceRegistered,
		KeyID:     sig.KeyID,
		Publisher: trust.Publishers[sig.KeyID],
	}, nil
}
