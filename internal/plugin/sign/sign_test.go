package sign

import (
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

	err = kr.Verify(sig, tampered)
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
