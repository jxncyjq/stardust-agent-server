package manifest

import (
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
	// ProvenanceRevoked.
	//
	// It stays empty for ProvenanceUnsigned even when a plugin.sig was
	// present, because the id in that file names a key this machine knows
	// nothing about; showing it would invite the reader to treat it as
	// meaningful.
	KeyID sign.KeyID

	// Publisher is the display name TrustInput.Publishers carries for KeyID.
	// It is empty when that map has no name for the key, and always empty for
	// ProvenanceUnsigned, which has no KeyID to look up.
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
		// contents could not change the verdict.
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
		// signature at all, and KeyID stays empty — see Provenance.KeyID.
		return Provenance{State: ProvenanceUnsigned}, nil
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
