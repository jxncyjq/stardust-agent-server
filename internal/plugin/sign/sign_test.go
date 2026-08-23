package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// requireErrorContains asserts err is non-nil and its message names want,
// failing loudly (with the actual error, or "nil", in the failure message)
// when it does not. Every error-path test in this file goes through this
// helper so a happy-path implementation that returns nil cannot pass as "the
// error names the problem".
func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// mustGenerateKey generates a fresh ed25519 key pair via GenerateKey,
// failing the test loudly if it errors.
func mustGenerateKey(t *testing.T) (pub []byte, priv []byte) {
	t.Helper()
	p, s, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: unexpected error: %v", err)
	}
	return p, s
}

// keyringJSON marshals a keyring document from a list of raw key entries,
// letting each test control exactly which fields are present (including
// omitting or blanking one) without depending on this package's internal
// struct shapes.
func keyringJSON(t *testing.T, entries ...map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatalf("marshal keyring fixture: %v", err)
	}
	return data
}

// sigJSON marshals a plugin.sig document from raw fields, for the same
// reason keyringJSON does.
func sigJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal signature fixture: %v", err)
	}
	return data
}

func validKeyEntry(id string, pub []byte) map[string]any {
	return map[string]any{"id": id, "algorithm": "ed25519", "public_key": b64(pub)}
}

func validSigFields(keyID string, value []byte) map[string]any {
	return map[string]any{"key_id": keyID, "algorithm": "ed25519", "signature": b64(value)}
}

// --- ParseKeyring ----------------------------------------------------------

func TestParseKeyring_RejectsUnknownField(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	data, err := json.Marshal(map[string]any{
		"keys":  []map[string]any{validKeyEntry("ops-2026", pub)},
		"extra": "surprise",
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	_, gotErr := ParseKeyring(data)
	requireErrorContains(t, gotErr, "parse keyring")
}

func TestParseKeyring_RejectsMalformedJSON(t *testing.T) {
	_, err := ParseKeyring([]byte("{not json"))
	requireErrorContains(t, err, "parse keyring")
}

// TestParseKeyring_RejectsUnknownFieldNested pins the doc's "at any nesting
// level" claim (sign.go:97): DisallowUnknownFields must reject a bogus
// field inside a keys[] entry, not only at the document's top level.
func TestParseKeyring_RejectsUnknownFieldNested(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	entry := validKeyEntry("ops-2026", pub)
	entry["bogus"] = "surprise"
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "parse keyring")
}

// TestParseKeyring_RejectsTrailingContent pins the requirement that a
// keyring document is exactly one JSON value: bytes after the closing brace
// must be refused, not silently discarded (json.Decoder.Decode alone stops
// after the first value and reports no error for what follows).
func TestParseKeyring_RejectsTrailingContent(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	valid := keyringJSON(t, validKeyEntry("ops-2026", pub))
	data := append(append([]byte{}, valid...), []byte(` {"x":1}`)...)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "parse keyring")
}

func TestParseKeyring_RejectsNonEd25519Algorithm(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	entry := validKeyEntry("ops-2026", pub)
	entry["algorithm"] = "rsa"
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "rsa")
}

func TestParseKeyring_RejectsEmptyAlgorithm(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	entry := validKeyEntry("ops-2026", pub)
	entry["algorithm"] = ""
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "algorithm")
}

func TestParseKeyring_RejectsInvalidBase64PublicKey(t *testing.T) {
	entry := map[string]any{"id": "ops-2026", "algorithm": "ed25519", "public_key": "not-base64!!"}
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "public_key")
}

func TestParseKeyring_RejectsWrongPublicKeyLength(t *testing.T) {
	entry := map[string]any{"id": "ops-2026", "algorithm": "ed25519", "public_key": b64([]byte("too-short"))}
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "32")
}

// TestParseKeyring_RejectsOverlongPublicKey pins the length check to "!=
// ed25519.PublicKeySize" rather than "< ed25519.PublicKeySize". A 33-byte
// key is the one input that would reach ed25519.Verify unrejected under a
// "<" mutation and panic there instead of being refused here — this test
// must fail loudly (via error, not panic) on the correct implementation.
func TestParseKeyring_RejectsOverlongPublicKey(t *testing.T) {
	entry := map[string]any{"id": "ops-2026", "algorithm": "ed25519", "public_key": b64(make([]byte, 33))}
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "33")
	requireErrorContains(t, err, "32")
}

func TestParseKeyring_RejectsEmptyID(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	entry := validKeyEntry("", pub)
	data := keyringJSON(t, entry)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "id")
}

func TestParseKeyring_RejectsDuplicateID(t *testing.T) {
	pubA, _ := mustGenerateKey(t)
	pubB, _ := mustGenerateKey(t)
	data := keyringJSON(t, validKeyEntry("ops-2026", pubA), validKeyEntry("ops-2026", pubB))

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "ops-2026")
	requireErrorContains(t, err, "twice")
}

func TestParseKeyring_RejectsEmptyKeyList(t *testing.T) {
	data := keyringJSON(t)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "empty")
}

// TestParseKeyring_RejectsEmptyKeyListLiteral exercises the brief's literal
// {"keys":[]} shape directly. keyringJSON(t) with zero entries marshals a
// nil slice to {"keys":null}, not {"keys":[]} — both hit the same
// len(raw.Keys)==0 branch in this implementation, but only this case
// constructs the byte-exact document rule 5 describes.
func TestParseKeyring_RejectsEmptyKeyListLiteral(t *testing.T) {
	data := []byte(`{"keys":[]}`)

	_, err := ParseKeyring(data)
	requireErrorContains(t, err, "empty")
}

func TestParseKeyring_Valid(t *testing.T) {
	pubA, _ := mustGenerateKey(t)
	pubB, _ := mustGenerateKey(t)
	data := keyringJSON(t, validKeyEntry("ops-2026", pubA), validKeyEntry("dev-2025", pubB))

	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}
	ids := kr.IDs()
	if len(ids) != 2 {
		t.Fatalf("IDs() = %v, want 2 entries", ids)
	}
	want := []KeyID{"dev-2025", "ops-2026"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("IDs()[%d] = %q, want %q (want sorted order)", i, ids[i], want[i])
		}
	}
}

// --- ParseSignature ----------------------------------------------------------

func TestParseSignature_RejectsUnknownField(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("payload"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	fields := validSigFields(string(sig.KeyID), sig.Value)
	fields["extra"] = "surprise"
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	_, gotErr := ParseSignature(data)
	requireErrorContains(t, gotErr, "parse signature")
}

func TestParseSignature_RejectsMalformedJSON(t *testing.T) {
	_, err := ParseSignature([]byte("{not json"))
	requireErrorContains(t, err, "parse signature")
}

func TestParseSignature_RejectsNonEd25519Algorithm(t *testing.T) {
	fields := validSigFields("ops-2026", make([]byte, 64))
	fields["algorithm"] = "rsa"
	data := sigJSON(t, fields)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "rsa")
}

func TestParseSignature_RejectsEmptyAlgorithm(t *testing.T) {
	fields := validSigFields("ops-2026", make([]byte, 64))
	fields["algorithm"] = ""
	data := sigJSON(t, fields)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "algorithm")
}

func TestParseSignature_RejectsWrongSignatureLength(t *testing.T) {
	fields := validSigFields("ops-2026", []byte("too-short"))
	data := sigJSON(t, fields)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "64")
}

// TestParseSignature_RejectsOverlongSignatureLength is the signature-side
// counterpart of TestParseKeyring_RejectsOverlongPublicKey: a 65-byte value
// is the one input that would reach ed25519.Verify unrejected under a "<"
// mutation of the length check.
func TestParseSignature_RejectsOverlongSignatureLength(t *testing.T) {
	fields := validSigFields("ops-2026", make([]byte, 65))
	data := sigJSON(t, fields)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "65")
	requireErrorContains(t, err, "64")
}

// TestParseSignature_RejectsTrailingContent is the plugin.sig counterpart
// of TestParseKeyring_RejectsTrailingContent.
func TestParseSignature_RejectsTrailingContent(t *testing.T) {
	valid := sigJSON(t, validSigFields("ops-2026", make([]byte, 64)))
	data := append(append([]byte{}, valid...), []byte(` {"x":1}`)...)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "parse signature")
}

func TestParseSignature_RejectsEmptyKeyID(t *testing.T) {
	fields := validSigFields("", make([]byte, 64))
	data := sigJSON(t, fields)

	_, err := ParseSignature(data)
	requireErrorContains(t, err, "key_id")
}

func TestParseSignature_Valid(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("payload"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	data := sigJSON(t, validSigFields(string(sig.KeyID), sig.Value))

	got, err := ParseSignature(data)
	if err != nil {
		t.Fatalf("ParseSignature: unexpected error: %v", err)
	}
	if got.KeyID != "ops-2026" {
		t.Errorf("KeyID = %q, want %q", got.KeyID, "ops-2026")
	}
	if got.Algorithm != "ed25519" {
		t.Errorf("Algorithm = %q, want %q", got.Algorithm, "ed25519")
	}
	if len(got.Value) != 64 {
		t.Errorf("len(Value) = %d, want 64", len(got.Value))
	}
}

// --- Sign / GenerateKey / Verify --------------------------------------------

func TestGenerateKey_ProducesUsableKeyPair(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	if len(pub) != 32 {
		t.Errorf("len(pub) = %d, want 32", len(pub))
	}
	if len(priv) != 64 {
		t.Errorf("len(priv) = %d, want 64", len(priv))
	}
}

func TestSign_RejectsMalformedPrivateKey(t *testing.T) {
	_, err := Sign([]byte("too-short"), "ops-2026", []byte("payload"))
	requireErrorContains(t, err, "private key")
}

func TestSign_RejectsEmptyKeyID(t *testing.T) {
	_, priv := mustGenerateKey(t)
	_, err := Sign(priv, "", []byte("payload"))
	requireErrorContains(t, err, "key id")
}

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	message := []byte("plugin package bytes")
	sig, err := Sign(priv, "ops-2026", message)
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	data := keyringJSON(t, validKeyEntry("ops-2026", pub))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	if err := kr.Verify(sig, message); err != nil {
		t.Errorf("Verify: unexpected error: %v", err)
	}
}

func TestVerify_RejectsTamperedMessage(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	message := []byte("plugin package bytes")
	sig, err := Sign(priv, "ops-2026", message)
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	data := keyringJSON(t, validKeyEntry("ops-2026", pub))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	tampered := append([]byte(nil), message...)
	tampered[0] ^= 0xFF

	// Assert on "does not verify" — wording that only the ed25519-failure
	// branch produces. Asserting on "ops-2026" alone would not distinguish
	// this from the unknown-key-id branch, whose error also names
	// "ops-2026" as a trusted id; this test must pin rule 7 (the
	// ed25519.Verify outcome), not merely "some error mentioning that id".
	err = kr.Verify(sig, tampered)
	requireErrorContains(t, err, "does not verify")
	requireErrorContains(t, err, "ops-2026")
}

func TestVerify_RejectsWrongKey(t *testing.T) {
	pubA, privA := mustGenerateKey(t)
	pubB, _ := mustGenerateKey(t)
	message := []byte("plugin package bytes")

	// Sign with key A's private key but claim it as key B — the keyring
	// entry for "key-b" holds key B's public key, which never produced
	// this signature.
	sig, err := Sign(privA, "key-b", message)
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	_ = pubA
	data := keyringJSON(t, validKeyEntry("key-a", pubA), validKeyEntry("key-b", pubB))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	err = kr.Verify(sig, message)
	requireErrorContains(t, err, "key-b")
}

func TestVerify_RejectsUnknownKeyID(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	message := []byte("plugin package bytes")
	sig, err := Sign(priv, "unknown-key", message)
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	data := keyringJSON(t, validKeyEntry("ops-2026", pub))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	err = kr.Verify(sig, message)
	requireErrorContains(t, err, "unknown-key")
	requireErrorContains(t, err, "ops-2026")
}

// TestVerify_RejectsAlgorithmMismatch constructs a Signature directly
// (bypassing ParseSignature, which is the only place algorithm enforcement
// was previously wired up) to prove Verify itself refuses a signature
// naming an algorithm this package does not implement. ParseSignature
// validates the algorithm on the way in, but Verify is the actual security
// gate; any future caller that builds a Signature some other way — a
// second file format, a test helper, a config-driven constructor — must
// not be able to sneak a non-ed25519 algorithm past Verify just because it
// skipped ParseSignature.
func TestVerify_RejectsAlgorithmMismatch(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	message := []byte("plugin package bytes")
	data := keyringJSON(t, validKeyEntry("ops-2026", pub))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	// The raw ed25519 signature is genuine and would verify — only the
	// claimed Algorithm is wrong. Verify must refuse based on the claim
	// alone.
	value := ed25519.Sign(priv, message)
	sig := Signature{KeyID: "ops-2026", Algorithm: "rsa", Value: value}

	err = kr.Verify(sig, message)
	requireErrorContains(t, err, "rsa")
}

// TestVerify_RejectsEmptyAlgorithm is TestVerify_RejectsAlgorithmMismatch's
// empty-string case: an omitted Algorithm on a directly-constructed
// Signature must not be treated as "the default algorithm".
func TestVerify_RejectsEmptyAlgorithm(t *testing.T) {
	pub, priv := mustGenerateKey(t)
	message := []byte("plugin package bytes")
	data := keyringJSON(t, validKeyEntry("ops-2026", pub))
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: unexpected error: %v", err)
	}

	value := ed25519.Sign(priv, message)
	sig := Signature{KeyID: "ops-2026", Algorithm: "", Value: value}

	err = kr.Verify(sig, message)
	requireErrorContains(t, err, "algorithm")
}

// TestVerify_NilKeyringPanics pins M-5: a nil *Keyring must crash loudly
// and self-explanatorily, not read as "no verification required" (that
// policy decision belongs to a later task and must be made by not calling
// Verify at all, never by Verify silently admitting on a nil receiver).
func TestVerify_NilKeyringPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic calling Verify on a nil *Keyring, got none")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "nil") {
			t.Fatalf("panic value %v does not explain the nil keyring", r)
		}
	}()
	var kr *Keyring
	_ = kr.Verify(Signature{KeyID: "ops-2026", Algorithm: "ed25519", Value: make([]byte, 64)}, []byte("m"))
}

// TestIDs_NilKeyringPanics is IDs' counterpart to
// TestVerify_NilKeyringPanics.
func TestIDs_NilKeyringPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic calling IDs on a nil *Keyring, got none")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "nil") {
			t.Fatalf("panic value %v does not explain the nil keyring", r)
		}
	}()
	var kr *Keyring
	_ = kr.IDs()
}

// --- marshalling -----------------------------------------------------------

// privJSON marshals a private key document from raw fields, for the same
// reason keyringJSON and sigJSON do: a test that wants exactly one field
// wrong must be able to say so without depending on this package's internal
// struct shapes.
func privJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal private key fixture: %v", err)
	}
	return data
}

func validPrivFields(keyID string, priv []byte) map[string]any {
	return map[string]any{"key_id": keyID, "algorithm": "ed25519", "private_key": b64(priv)}
}

func TestMarshalSignature_RoundTripsThroughParseSignature(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}

	data, err := MarshalSignature(sig)
	if err != nil {
		t.Fatalf("MarshalSignature: unexpected error: %v", err)
	}
	got, err := ParseSignature(data)
	if err != nil {
		t.Fatalf("ParseSignature of MarshalSignature output: unexpected error: %v", err)
	}
	if got.KeyID != sig.KeyID {
		t.Errorf("KeyID = %q, want %q", got.KeyID, sig.KeyID)
	}
	if got.Algorithm != sig.Algorithm {
		t.Errorf("Algorithm = %q, want %q", got.Algorithm, sig.Algorithm)
	}
	if b64(got.Value) != b64(sig.Value) {
		t.Errorf("Value = %q, want %q", b64(got.Value), b64(sig.Value))
	}
}

func TestMarshalSignature_RejectsEmptyKeyID(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	sig.KeyID = ""
	_, err = MarshalSignature(sig)
	requireErrorContains(t, err, "key_id is empty")
}

func TestMarshalSignature_RejectsNonEd25519Algorithm(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	sig.Algorithm = "rsa"
	_, err = MarshalSignature(sig)
	requireErrorContains(t, err, "rsa")
}

func TestMarshalSignature_RejectsWrongValueLength(t *testing.T) {
	_, priv := mustGenerateKey(t)
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	sig.Value = sig.Value[:10]
	_, err = MarshalSignature(sig)
	requireErrorContains(t, err, "want 64")
}

func TestMarshalKeyEntry_ParsesInsideAKeyring(t *testing.T) {
	pub, priv := mustGenerateKey(t)

	entry, err := MarshalKeyEntry("ops-2026", pub)
	if err != nil {
		t.Fatalf("MarshalKeyEntry: unexpected error: %v", err)
	}
	kr, err := ParseKeyring([]byte(`{"keys":[` + string(entry) + `]}`))
	if err != nil {
		t.Fatalf("ParseKeyring of a document built from MarshalKeyEntry: unexpected error: %v", err)
	}
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	if err := kr.Verify(sig, []byte("message")); err != nil {
		t.Fatalf("Verify against the marshalled entry: unexpected error: %v", err)
	}
}

func TestMarshalKeyEntry_RejectsEmptyID(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	_, err := MarshalKeyEntry("", pub)
	requireErrorContains(t, err, "key id is empty")
}

func TestMarshalKeyEntry_RejectsWrongPublicKeyLength(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	_, err := MarshalKeyEntry("ops-2026", pub[:10])
	requireErrorContains(t, err, "want 32")
}

func TestMarshalKeyring_RoundTripsThroughParseKeyring(t *testing.T) {
	pub, priv := mustGenerateKey(t)

	data, err := MarshalKeyring("ops-2026", pub)
	if err != nil {
		t.Fatalf("MarshalKeyring: unexpected error: %v", err)
	}
	kr, err := ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring of MarshalKeyring output: unexpected error: %v", err)
	}
	ids := kr.IDs()
	if len(ids) != 1 || ids[0] != "ops-2026" {
		t.Fatalf("IDs() = %v, want exactly [ops-2026]", ids)
	}
	sig, err := Sign(priv, "ops-2026", []byte("message"))
	if err != nil {
		t.Fatalf("Sign: unexpected error: %v", err)
	}
	if err := kr.Verify(sig, []byte("message")); err != nil {
		t.Fatalf("Verify against the marshalled keyring: unexpected error: %v", err)
	}
}

func TestMarshalKeyring_RejectsWrongPublicKeyLength(t *testing.T) {
	pub, _ := mustGenerateKey(t)
	_, err := MarshalKeyring("ops-2026", pub[:31])
	requireErrorContains(t, err, "want 32")
}

func TestMarshalPrivateKey_RoundTripsThroughParsePrivateKey(t *testing.T) {
	pub, priv := mustGenerateKey(t)

	data, err := MarshalPrivateKey("ops-2026", priv)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: unexpected error: %v", err)
	}
	id, got, err := ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("ParsePrivateKey of MarshalPrivateKey output: unexpected error: %v", err)
	}
	if id != "ops-2026" {
		t.Errorf("key id = %q, want %q", id, "ops-2026")
	}
	if b64(got) != b64(priv) {
		t.Error("round-tripped private key differs from the one marshalled")
	}
	sig, err := Sign(got, id, []byte("message"))
	if err != nil {
		t.Fatalf("Sign with the round-tripped key: unexpected error: %v", err)
	}
	if !ed25519.Verify(pub, []byte("message"), sig.Value) {
		t.Error("a signature made with the round-tripped private key does not verify against the original public key")
	}
}

func TestMarshalPrivateKey_RejectsEmptyID(t *testing.T) {
	_, priv := mustGenerateKey(t)
	_, err := MarshalPrivateKey("", priv)
	requireErrorContains(t, err, "key id is empty")
}

func TestMarshalPrivateKey_RejectsWrongLength(t *testing.T) {
	_, priv := mustGenerateKey(t)
	_, err := MarshalPrivateKey("ops-2026", priv[:32])
	requireErrorContains(t, err, "want 64")
}

// TestMarshalPrivateKey_ErrorsNeverEchoTheKey pins the one property this
// document's existence risks: private key material reaching an error message.
// A key of the wrong LENGTH is exactly the case an implementation is most
// tempted to report by dumping what it was handed.
func TestMarshalPrivateKey_ErrorsNeverEchoTheKey(t *testing.T) {
	_, priv := mustGenerateKey(t)
	short := priv[:32]
	_, err := MarshalPrivateKey("ops-2026", short)
	if err == nil {
		t.Fatal("MarshalPrivateKey: want an error for a 32-byte private key, got nil")
	}
	if strings.Contains(err.Error(), b64(short)) || strings.Contains(err.Error(), string(short)) {
		t.Errorf("MarshalPrivateKey error %q echoes the private key material", err)
	}
}

func TestParsePrivateKey_RejectsUnknownField(t *testing.T) {
	_, priv := mustGenerateKey(t)
	fields := validPrivFields("ops-2026", priv)
	fields["comment"] = "generated on the ops laptop"
	_, _, err := ParsePrivateKey(privJSON(t, fields))
	requireErrorContains(t, err, "comment")
}

func TestParsePrivateKey_RejectsMalformedJSON(t *testing.T) {
	_, _, err := ParsePrivateKey([]byte(`{"key_id":`))
	requireErrorContains(t, err, "parse private key")
}

func TestParsePrivateKey_RejectsTrailingContent(t *testing.T) {
	_, priv := mustGenerateKey(t)
	data := append(privJSON(t, validPrivFields("ops-2026", priv)), []byte(`{"key_id":"other"}`)...)
	_, _, err := ParsePrivateKey(data)
	requireErrorContains(t, err, "unexpected content")
}

func TestParsePrivateKey_RejectsEmptyKeyID(t *testing.T) {
	_, priv := mustGenerateKey(t)
	_, _, err := ParsePrivateKey(privJSON(t, validPrivFields("", priv)))
	requireErrorContains(t, err, "key_id is empty")
}

func TestParsePrivateKey_RejectsNonEd25519Algorithm(t *testing.T) {
	_, priv := mustGenerateKey(t)
	fields := validPrivFields("ops-2026", priv)
	fields["algorithm"] = "rsa"
	_, _, err := ParsePrivateKey(privJSON(t, fields))
	requireErrorContains(t, err, "rsa")
}

func TestParsePrivateKey_RejectsInvalidBase64(t *testing.T) {
	fields := map[string]any{"key_id": "ops-2026", "algorithm": "ed25519", "private_key": "not base64!!"}
	_, _, err := ParsePrivateKey(privJSON(t, fields))
	requireErrorContains(t, err, "base64")
}

func TestParsePrivateKey_RejectsWrongLength(t *testing.T) {
	_, priv := mustGenerateKey(t)
	_, _, err := ParsePrivateKey(privJSON(t, validPrivFields("ops-2026", priv[:32])))
	requireErrorContains(t, err, "want 64")
}

// TestParsePrivateKey_ErrorsNeverEchoTheDocument is the reading half of
// TestMarshalPrivateKey_ErrorsNeverEchoTheKey: a private_key that is valid
// base64 but the wrong length must be reported by LENGTH, never by content.
func TestParsePrivateKey_ErrorsNeverEchoTheDocument(t *testing.T) {
	_, priv := mustGenerateKey(t)
	short := priv[:48]
	_, _, err := ParsePrivateKey(privJSON(t, validPrivFields("ops-2026", short)))
	if err == nil {
		t.Fatal("ParsePrivateKey: want an error for a 48-byte private key, got nil")
	}
	if strings.Contains(err.Error(), b64(short)) || strings.Contains(err.Error(), string(short)) {
		t.Errorf("ParsePrivateKey error %q echoes the private key material", err)
	}
}
