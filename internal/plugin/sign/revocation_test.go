package sign

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Revocation is what makes a leaked private key recoverable: without it, a key
// that has been trusted stays trusted forever, because "delete it from keys"
// only stops NEW packages — an operator has no way to say "this key was
// trusted and no longer is" in a way the error messages can explain.

// revocableKeyringJSON marshals a keyring document with both lists.
func revocableKeyringJSON(t *testing.T, keys []map[string]any, revoked []map[string]any) []byte {
	t.Helper()

	doc := map[string]any{"keys": keys}
	if revoked != nil {
		doc["revoked"] = revoked
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal keyring fixture: %v", err)
	}
	return data
}

func TestParseKeyringAcceptsARevocationList(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	other, _ := mustGenerateKey(t)

	keyring, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub), validKeyEntry("old", other)},
		[]map[string]any{{"key_id": "old", "revoked_at": "2026-08-29T10:00:00Z", "reason": "laptop stolen"}},
	))
	if err != nil {
		t.Fatalf("ParseKeyring with a revocation list: %v, want nil", err)
	}
	if got := keyring.RevokedIDs(); len(got) != 1 || got[0] != KeyID("old") {
		t.Errorf("RevokedIDs() = %v, want [old]", got)
	}
}

// TestVerifyRefusesARevokedKeyThatIsStillListed is the whole point: the key is
// still in "keys" and the signature is mathematically correct, and it must
// still be refused.
//
// Keeping the public key alongside the revocation is deliberate — it is what
// lets the error say "this key was trusted and is not any more" instead of the
// far less useful "unknown key id".
func TestVerifyRefusesARevokedKeyThatIsStillListed(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	keyring, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("leaked", pub)},
		[]map[string]any{{"key_id": "leaked", "revoked_at": "2026-08-29T10:00:00Z", "reason": "laptop stolen"}},
	))
	if err == nil {
		t.Fatalf("ParseKeyring with every key revoked = nil error, want a refusal (empty trust set)")
	}

	// Now a keyring where one key survives, so the document parses and the
	// revoked key can still be exercised.
	live, _ := mustGenerateKey(t)
	keyring, err = ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("leaked", pub), validKeyEntry("live", live)},
		[]map[string]any{{"key_id": "leaked", "revoked_at": "2026-08-29T10:00:00Z", "reason": "laptop stolen"}},
	))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}

	message := []byte("plugin.json bytes")
	sig := Signature{KeyID: "leaked", Algorithm: "ed25519", Value: ed25519.Sign(priv, message)}

	err = keyring.Verify(sig, message)
	if err == nil {
		t.Fatal("Verify with a revoked key = nil error, want a refusal")
	}
	if !errors.Is(err, ErrRevokedKey) {
		t.Errorf("Verify error = %v, want it to wrap ErrRevokedKey", err)
	}
	if !strings.Contains(err.Error(), "2026-08-29") || !strings.Contains(err.Error(), "laptop stolen") {
		t.Errorf("Verify error = %v, want it to carry when and why the key was revoked", err)
	}
}

func TestVerifyStillAcceptsAKeyThatWasNotRevoked(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	old, _ := mustGenerateKey(t)
	keyring, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub), validKeyEntry("old", old)},
		[]map[string]any{{"key_id": "old"}},
	))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}

	message := []byte("plugin.json bytes")
	sig := Signature{KeyID: "live", Algorithm: "ed25519", Value: ed25519.Sign(priv, message)}
	if err := keyring.Verify(sig, message); err != nil {
		t.Errorf("Verify with a live key = %v, want nil", err)
	}
}

// TestParseKeyringAllowsRevokingAKeyThatIsNoLongerListed: an operator who
// already deleted the public key can still record the revocation, and that
// record is what makes a stale package's failure explainable.
func TestParseKeyringAllowsRevokingAKeyThatIsNoLongerListed(t *testing.T) {
	pub, _ := mustGenerateKey(t)

	keyring, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub)},
		[]map[string]any{{"key_id": "deleted-long-ago"}},
	))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	if got := keyring.RevokedIDs(); len(got) != 1 || got[0] != KeyID("deleted-long-ago") {
		t.Errorf("RevokedIDs() = %v, want [deleted-long-ago]", got)
	}
}

func TestParseKeyringRefusesADuplicateRevocation(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	other, _ := mustGenerateKey(t)

	// Two records for one id: which one is in force? There is no answer, so
	// the document is refused rather than one of them silently winning.
	_, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub), validKeyEntry("old", other)},
		[]map[string]any{{"key_id": "old", "reason": "first"}, {"key_id": "old", "reason": "second"}},
	))
	requireErrorContains(t, err, "old")
}

func TestParseKeyringRefusesAnEmptyRevokedKeyID(t *testing.T) {
	pub, _ := mustGenerateKey(t)

	_, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub)},
		[]map[string]any{{"key_id": ""}},
	))
	requireErrorContains(t, err, "key_id")
}

// TestParseKeyringRefusesWhenEveryKeyIsRevoked: that is an empty trust set
// spelled differently, and ParseKeyring already refuses an empty "keys" list
// for the same reason — with mandatory signing it would refuse every plugin,
// and saying so once at parse time beats saying it at every mount.
func TestParseKeyringRefusesWhenEveryKeyIsRevoked(t *testing.T) {
	a, _ := mustGenerateKey(t)
	b, _ := mustGenerateKey(t)

	_, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("a", a), validKeyEntry("b", b)},
		[]map[string]any{{"key_id": "a"}, {"key_id": "b"}},
	))
	if err == nil {
		t.Fatal("ParseKeyring with every key revoked = nil error, want a refusal")
	}
	requireErrorContains(t, err, "revoked")
}

// TestParseKeyringRefusesAMalformedRevokedAt: an unparseable timestamp would
// travel verbatim into the refusal message an operator reads while deciding
// whether a package is safe.
func TestParseKeyringRefusesAMalformedRevokedAt(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	other, _ := mustGenerateKey(t)

	_, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub), validKeyEntry("old", other)},
		[]map[string]any{{"key_id": "old", "revoked_at": "last tuesday"}},
	))
	requireErrorContains(t, err, "revoked_at")
}

// TestParseKeyringWithoutARevokedListIsUnchanged pins backward compatibility:
// every keyring written before this field existed has no "revoked" key at all.
func TestParseKeyringWithoutARevokedListIsUnchanged(t *testing.T) {
	pub, priv := mustGenerateKey(t)

	keyring, err := ParseKeyring(keyringJSON(t, validKeyEntry("live", pub)))
	if err != nil {
		t.Fatalf("ParseKeyring without a revoked list: %v, want nil", err)
	}
	if got := keyring.RevokedIDs(); len(got) != 0 {
		t.Errorf("RevokedIDs() = %v, want empty", got)
	}
	message := []byte("plugin.json bytes")
	sig := Signature{KeyID: "live", Algorithm: "ed25519", Value: ed25519.Sign(priv, message)}
	if err := keyring.Verify(sig, message); err != nil {
		t.Errorf("Verify = %v, want nil", err)
	}
}

func TestRevokedIDsAreSorted(t *testing.T) {
	pub, _ := mustGenerateKey(t)

	keyring, err := ParseKeyring(revocableKeyringJSON(t,
		[]map[string]any{validKeyEntry("live", pub)},
		[]map[string]any{{"key_id": "zeta"}, {"key_id": "alpha"}},
	))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	got := keyring.RevokedIDs()
	if len(got) != 2 || got[0] != KeyID("alpha") || got[1] != KeyID("zeta") {
		t.Errorf("RevokedIDs() = %v, want [alpha zeta]: the order feeds a policy comparison", got)
	}
}
