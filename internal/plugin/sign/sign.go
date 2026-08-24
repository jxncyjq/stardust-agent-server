// Package sign implements the Ed25519 signature primitives and the trust
// keyring a deployment uses to decide which plugin packages it will load.
//
// A plugin package today is trusted on the strength of a sha256 digest that
// travels in the very manifest being verified (see internal/plugin/manifest)
// — anyone who can edit plugin.json can also edit its own checksum. This
// package adds the missing layer: a detached signature over the package,
// checked against a keyring of public keys the deployment has decided to
// trust, so that "the bytes match the manifest" and "the bytes came from
// someone we trust" become two separate, both-required facts.
//
// This package does pure computation only. ParseKeyring and ParseSignature
// take bytes already read from disk; Verify takes a message already read
// into memory. Locating plugin.sig next to a package, reading it, and
// deciding when signature checking is mandatory are later tasks' jobs —
// mixing file I/O into this package would make it impossible to exhaustively
// test the validation rules below, for the same reason
// internal/plugin/manifest keeps parsing separate from assembly.
//
// A Signature names the key it was made with (KeyID), and Verify refuses an
// unknown key id rather than trying every public key in the Keyring: trying
// every key would make "signed with a key we don't trust" and "signed with
// the right key but tampered with afterward" indistinguishable failures.
// Keeping them distinguishable is also why a Keyring holds public keys only
// — nothing in this package ever needs a private key except to produce a
// signature (Sign) or to mint a fresh key pair (GenerateKey), and neither of
// those places lets a private key leak into a log or an error message.
package sign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
)

// signatureAlgorithm is the only algorithm name ParseKeyring and
// ParseSignature accept. There is exactly one signing scheme this package
// implements; a keyring entry or signature naming anything else — including
// an empty string, which is not treated as a default — names a scheme this
// package cannot check.
const signatureAlgorithm = "ed25519"

// KeyID names one public key within a Keyring, and — carried on a
// Signature — the key a signature was produced with. Verify uses a
// Signature's KeyID to select the single public key to check it against; it
// never iterates the keyring trying every key (see the package doc for why).
type KeyID string

// Signature is a detached Ed25519 signature over a plugin package's bytes,
// as read from (via ParseSignature) or written to a plugin.sig file. KeyID
// names which Keyring entry produced it; Algorithm is carried alongside the
// raw Value so that a signature naming an algorithm this package does not
// implement fails loudly instead of being checked against the wrong scheme.
type Signature struct {
	KeyID     KeyID
	Algorithm string
	Value     []byte
}

// Keyring is a validated, immutable set of trusted Ed25519 public keys,
// looked up by KeyID. It holds public keys only: nothing in this package
// ever needs a Keyring to hold a private key, and giving it one would risk
// that key reaching a log or an error message through this type. The only
// way to obtain a Keyring is ParseKeyring, which guarantees every entry has
// passed its shape checks and that no two entries share an id.
type Keyring struct {
	keys map[KeyID]ed25519.PublicKey
}

// rawKeyEntry mirrors one entry of a keyring document's "keys" array for
// decoding, before ParseKeyring has validated and base64-decoded it.
type rawKeyEntry struct {
	ID        KeyID  `json:"id"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key"`
}

// rawKeyring mirrors a keyring document's JSON shape for decoding:
//
//	{
//	  "keys": [
//	    { "id": "ops-2026", "algorithm": "ed25519", "public_key": "<base64 32 字节>" }
//	  ]
//	}
type rawKeyring struct {
	Keys []rawKeyEntry `json:"keys"`
}

// ParseKeyring decodes and validates a keyring document (the deployment's
// trust set of public keys) from bytes already read from disk. It refuses:
//
//   - a document with any field name it does not recognize, at any nesting
//     level (json.Decoder.DisallowUnknownFields) — a mistyped key name must
//     fail parsing, not silently do nothing;
//   - an empty "keys" list — an empty trust set combined with mandatory
//     signing would refuse every plugin, and saying so here, at parse time,
//     is more actionable than discovering it one plugin at a time at mount
//     time;
//   - an entry with an empty id;
//   - an entry whose algorithm is not exactly "ed25519" (an empty string is
//     not treated as a default: an entry that omitted its algorithm is a
//     malformed entry, not one requesting the default scheme);
//   - an entry whose public_key does not decode as base64, or does not
//     decode to exactly ed25519.PublicKeySize (32) bytes;
//   - two entries sharing the same id — a trust set must name each key
//     unambiguously, or "which key signed this" stops being a well-defined
//     question;
//   - any non-whitespace content in data after the single JSON document —
//     a second, ignored document is the same kind of mistake
//     DisallowUnknownFields exists to catch: something written wrong being
//     silently dropped instead of refused.
//
// Every error names the offending key's id (or, when the id itself is the
// problem, the entry's index) rather than only reporting that parsing
// failed.
func ParseKeyring(data []byte) (*Keyring, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawKeyring
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse keyring: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse keyring: unexpected content after the JSON document")
	}
	if len(raw.Keys) == 0 {
		return nil, fmt.Errorf("parse keyring: keys is empty; an empty trust set combined with mandatory " +
			"signing would refuse every plugin")
	}

	keys := make(map[KeyID]ed25519.PublicKey, len(raw.Keys))
	for i, entry := range raw.Keys {
		if entry.ID == "" {
			return nil, fmt.Errorf("parse keyring: keys[%d] has no id", i)
		}
		if entry.Algorithm != signatureAlgorithm {
			return nil, fmt.Errorf("parse keyring: key %q algorithm is %q, want %q",
				entry.ID, entry.Algorithm, signatureAlgorithm)
		}
		pub, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("parse keyring: key %q public_key is not valid base64: %w", entry.ID, err)
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("parse keyring: key %q public_key is %d bytes, want %d",
				entry.ID, len(pub), ed25519.PublicKeySize)
		}
		if _, dup := keys[entry.ID]; dup {
			return nil, fmt.Errorf("parse keyring: key id %q appears twice; a trust set must name each key "+
				"unambiguously", entry.ID)
		}
		keys[entry.ID] = ed25519.PublicKey(pub)
	}
	return &Keyring{keys: keys}, nil
}

// IDs returns the ids of every key in the keyring, sorted for a
// deterministic, reproducible order — callers that render this list (Verify
// does, in its unknown-key-id error) get the same text on every run.
//
// IDs panics if k is nil: a nil *Keyring is a programming error, not "no
// keys" — a nil-safe zero value here would let callers read "no keyring
// configured" as "trust set of size zero" instead of the fail-loud crash
// that omission deserves.
func (k *Keyring) IDs() []KeyID {
	if k == nil {
		panic("sign: IDs called on a nil *Keyring")
	}
	ids := make([]KeyID, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Verify checks that sig is a valid Ed25519 signature over message, made by
// the private key matching the Keyring entry sig.KeyID names.
//
// Verify looks up sig.KeyID directly rather than trying every key in the
// keyring: a signature declares which key it was made with, and honoring
// that declaration is what keeps "signed with a key that is not trusted"
// and "signed with a trusted key but the message was tampered with
// afterward" distinguishable failures. Trying every key would collapse
// them, because a message tampered with after being signed by an untrusted
// key could happen to also fail against a trusted key for an unrelated
// reason, masking which check actually failed.
//
// Verify also checks sig.Algorithm before touching the keyring: a Signature
// naming any algorithm other than the one this package implements is
// refused by name, exactly as ParseSignature would refuse it — a Signature
// built directly (bypassing ParseSignature) must not be able to carry a
// claimed algorithm of "rsa" or "" into an ed25519 check and have it
// silently ignored.
//
// An unknown sig.KeyID is refused, naming both the unknown id and the ids
// Verify does trust (via IDs), so the caller can tell "wrong key" from
// "right key, bad signature" without guessing. A signature that fails
// ed25519.Verify against the resolved public key is refused the same way,
// naming the key it was checked against.
//
// Verify panics if k is nil, for the same reason IDs does: a nil *Keyring
// must never read as "no verification required". Callers that want signing
// to be optional must decide that before calling Verify, not by passing it
// a nil receiver.
func (k *Keyring) Verify(sig Signature, message []byte) error {
	if k == nil {
		panic("sign: Verify called on a nil *Keyring")
	}
	if sig.Algorithm != signatureAlgorithm {
		return fmt.Errorf("verify signature: algorithm is %q, want %q", sig.Algorithm, signatureAlgorithm)
	}
	pub, ok := k.keys[sig.KeyID]
	if !ok {
		return fmt.Errorf("verify signature: key id %q is not in the keyring (trusted ids: %v)",
			sig.KeyID, k.IDs())
	}
	if !ed25519.Verify(pub, message, sig.Value) {
		return fmt.Errorf("verify signature: signature does not verify against key %q", sig.KeyID)
	}
	return nil
}

// rawSignature mirrors a plugin.sig document's JSON shape for decoding:
//
//	{ "key_id": "ops-2026", "algorithm": "ed25519", "signature": "<base64 64 字节>" }
type rawSignature struct {
	KeyID     KeyID  `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

// ParseSignature decodes and validates a plugin.sig document from bytes
// already read from disk. It refuses:
//
//   - a document with any field name it does not recognize, at any nesting
//     level (json.Decoder.DisallowUnknownFields);
//   - an empty key_id — an unnamed signature cannot be resolved to a
//     Keyring entry by Verify;
//   - an algorithm that is not exactly "ed25519" (an empty string is not
//     treated as a default, for the same reason ParseKeyring rejects one);
//   - a signature that does not decode as base64, or does not decode to
//     exactly ed25519.SignatureSize (64) bytes;
//   - any non-whitespace content in data after the single JSON document, for
//     the same reason ParseKeyring refuses it.
//
// ParseSignature does not check the signature against any key — it only
// validates the document's own shape. Checking it is Keyring.Verify's job.
func ParseSignature(data []byte) (Signature, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawSignature
	if err := dec.Decode(&raw); err != nil {
		return Signature{}, fmt.Errorf("parse signature: %w", err)
	}
	if dec.More() {
		return Signature{}, fmt.Errorf("parse signature: unexpected content after the JSON document")
	}
	if raw.KeyID == "" {
		return Signature{}, fmt.Errorf("parse signature: key_id is empty")
	}
	if raw.Algorithm != signatureAlgorithm {
		return Signature{}, fmt.Errorf("parse signature: algorithm is %q, want %q",
			raw.Algorithm, signatureAlgorithm)
	}
	value, err := base64.StdEncoding.DecodeString(raw.Signature)
	if err != nil {
		return Signature{}, fmt.Errorf("parse signature: signature is not valid base64: %w", err)
	}
	if len(value) != ed25519.SignatureSize {
		return Signature{}, fmt.Errorf("parse signature: signature is %d bytes, want %d",
			len(value), ed25519.SignatureSize)
	}
	return Signature{KeyID: raw.KeyID, Algorithm: raw.Algorithm, Value: value}, nil
}

// Sign produces a Signature over message using priv, naming it as made by
// id. It is the only place besides GenerateKey where a private key appears
// in this package's API — neither function's result type, nor
// ed25519.PrivateKey itself, has a String() method, so a private key cannot
// reach a log or an error message by being formatted through this package.
func Sign(priv ed25519.PrivateKey, id KeyID, message []byte) (Signature, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Signature{}, fmt.Errorf("sign message: private key is %d bytes, want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	if id == "" {
		return Signature{}, fmt.Errorf("sign message: key id is empty")
	}
	value := ed25519.Sign(priv, message)
	return Signature{KeyID: id, Algorithm: signatureAlgorithm, Value: value}, nil
}

// GenerateKey mints a fresh Ed25519 key pair using crypto/rand as its
// entropy source. The returned private key must never be logged or placed
// in an error message; it exists only to be passed to Sign or stored by the
// caller under the caller's own protection — this package keeps no copy of
// it.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	return pub, priv, nil
}

// --- encoding --------------------------------------------------------------
//
// Every file format this package can read, it also writes here, and nowhere
// else. Before these functions existed the plugin.sig encoding lived only in
// a test helper, which meant one decoder (ParseSignature) faced two
// independent encoders that had to be kept in agreement by hand — the day the
// document gains a field, only one of them learns about it. Each Marshal*
// below validates what it is handed before encoding it, so none of them can
// produce a document its matching Parse* would refuse.

// marshalDocument encodes one of this package's raw document structs as
// indented JSON with a trailing newline. Indented because every one of these
// documents is a file an operator reads, diffs and pastes; the trailing
// newline for the same reason. Both are safe for the readers: the Parse*
// functions accept any amount of whitespace around the single document they
// decode.
func marshalDocument(doc any) ([]byte, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode document: %w", err)
	}
	return append(data, '\n'), nil
}

// MarshalSignature encodes sig as the bytes of a plugin.sig document, the
// exact shape ParseSignature reads back:
//
//	{"key_id":"ops-2026","algorithm":"ed25519","signature":"<base64 64 字节>"}
//
// It validates before it encodes, so it cannot write a document its own
// reader would refuse: an empty key id, an algorithm other than "ed25519"
// (a Signature assembled literally, bypassing Sign, can carry one) and a
// value that is not ed25519.SignatureSize bytes are errors rather than bytes
// on disk. A signing tool that emitted signatures the verifier rejects would
// teach operators that verification is broken, which is worse than not
// signing at all.
func MarshalSignature(sig Signature) ([]byte, error) {
	if sig.KeyID == "" {
		return nil, fmt.Errorf("marshal signature: key_id is empty")
	}
	if sig.Algorithm != signatureAlgorithm {
		return nil, fmt.Errorf("marshal signature: algorithm is %q, want %q",
			sig.Algorithm, signatureAlgorithm)
	}
	if len(sig.Value) != ed25519.SignatureSize {
		return nil, fmt.Errorf("marshal signature: signature is %d bytes, want %d",
			len(sig.Value), ed25519.SignatureSize)
	}
	data, err := marshalDocument(rawSignature{
		KeyID:     sig.KeyID,
		Algorithm: sig.Algorithm,
		Signature: base64.StdEncoding.EncodeToString(sig.Value),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal signature %q: %w", sig.KeyID, err)
	}
	return data, nil
}

// newKeyEntry validates id and pub and returns the raw entry both
// MarshalKeyEntry and MarshalKeyring encode, so the two can never disagree
// about what a keyring entry looks like or about which entries are legal.
func newKeyEntry(id KeyID, pub ed25519.PublicKey) (rawKeyEntry, error) {
	if id == "" {
		return rawKeyEntry{}, fmt.Errorf("key id is empty")
	}
	if len(pub) != ed25519.PublicKeySize {
		return rawKeyEntry{}, fmt.Errorf("public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return rawKeyEntry{
		ID:        id,
		Algorithm: signatureAlgorithm,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// MarshalKeyEntry encodes one entry of a keyring document — the shape an
// operator pastes into the "keys" array of a trust set they already have:
//
//	{"id":"ops-2026","algorithm":"ed25519","public_key":"<base64 32 字节>"}
//
// The result is not a keyring document on its own and ParseKeyring will not
// accept it; MarshalKeyring produces the whole document for a trust set whose
// only key is this one.
func MarshalKeyEntry(id KeyID, pub ed25519.PublicKey) ([]byte, error) {
	entry, err := newKeyEntry(id, pub)
	if err != nil {
		return nil, fmt.Errorf("marshal key entry: %w", err)
	}
	data, err := marshalDocument(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal key entry %q: %w", id, err)
	}
	return data, nil
}

// MarshalKeyring encodes a complete keyring document whose trust set is the
// single key id/pub, the shape ParseKeyring reads back:
//
//	{"keys":[{"id":"ops-2026","algorithm":"ed25519","public_key":"<base64 32 字节>"}]}
//
// Two callers want exactly this: a deployment that trusts one signing key,
// and any tool that needs to check its own work against a key pair it holds
// — marshalling and re-parsing costs nothing and routes the check through
// the production reader (ParseKeyring, then Keyring.Verify) instead of a
// second, privately assembled trust set that could drift from it.
func MarshalKeyring(id KeyID, pub ed25519.PublicKey) ([]byte, error) {
	entry, err := newKeyEntry(id, pub)
	if err != nil {
		return nil, fmt.Errorf("marshal keyring: %w", err)
	}
	data, err := marshalDocument(rawKeyring{Keys: []rawKeyEntry{entry}})
	if err != nil {
		return nil, fmt.Errorf("marshal keyring %q: %w", id, err)
	}
	return data, nil
}

// rawPrivateKey mirrors a private key document's JSON shape for encoding and
// decoding:
//
//	{ "key_id": "ops-2026", "algorithm": "ed25519", "private_key": "<base64 64 字节>" }
//
// The key id travels WITH the key on purpose. A signature names the keyring
// entry it was made with, and if that name lived only in a command-line flag
// then a typo would produce a perfectly valid signature that no deployment
// can resolve — a failure that surfaces only at mount time, on the far side
// of a handoff. Binding the name to the key material once, when the pair is
// minted, removes that class of mistake entirely.
type rawPrivateKey struct {
	KeyID      KeyID  `json:"key_id"`
	Algorithm  string `json:"algorithm"`
	PrivateKey string `json:"private_key"`
}

// MarshalPrivateKey encodes priv, named as key id, as the bytes of a private
// key document (see rawPrivateKey for the shape). It is the counterpart of
// ParsePrivateKey and, like the other Marshal* functions here, validates
// first: an empty id or a key that is not ed25519.PrivateKeySize bytes is an
// error rather than a file.
//
// THE RETURNED BYTES ARE SECRET. They are the whole private key. A caller
// must write them where only it can read them (mode 0600) and must never
// print them, log them, or place them in an error message. No error this
// package returns ever contains key material — a wrong length is reported as
// a length — and callers must hold the same line.
func MarshalPrivateKey(id KeyID, priv ed25519.PrivateKey) ([]byte, error) {
	if id == "" {
		return nil, fmt.Errorf("marshal private key: key id is empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("marshal private key %q: private key is %d bytes, want %d",
			id, len(priv), ed25519.PrivateKeySize)
	}
	data, err := marshalDocument(rawPrivateKey{
		KeyID:      id,
		Algorithm:  signatureAlgorithm,
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal private key %q: %w", id, err)
	}
	return data, nil
}

// ParsePrivateKey decodes and validates a private key document from bytes
// already read from disk, returning the key id it names and the key itself.
// It applies the same rules the other parsers here do — unknown fields at any
// nesting level, trailing content after the document, an empty key_id, an
// algorithm that is not exactly "ed25519", a private_key that is not base64
// or not ed25519.PrivateKeySize bytes are all refused.
//
// No error it returns carries key material: the length case is reported as a
// length, and the base64 case as the decoder's own offset-based complaint.
// This matters more here than anywhere else in the package, because the
// document being parsed IS a secret.
//
// ParsePrivateKey does NOT check that the key's two halves agree — an
// Ed25519 private key is a seed followed by the public key it derives, and
// nothing stops a hand-edited file from pairing a seed with someone else's
// public half. Such a key signs happily and its signatures verify against
// nothing; catching that is the job of whoever signs, by verifying its own
// output before writing it.
func ParsePrivateKey(data []byte) (KeyID, ed25519.PrivateKey, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawPrivateKey
	if err := dec.Decode(&raw); err != nil {
		return "", nil, fmt.Errorf("parse private key: %w", err)
	}
	if dec.More() {
		return "", nil, fmt.Errorf("parse private key: unexpected content after the JSON document")
	}
	if raw.KeyID == "" {
		return "", nil, fmt.Errorf("parse private key: key_id is empty")
	}
	if raw.Algorithm != signatureAlgorithm {
		return "", nil, fmt.Errorf("parse private key %q: algorithm is %q, want %q",
			raw.KeyID, raw.Algorithm, signatureAlgorithm)
	}
	priv, err := base64.StdEncoding.DecodeString(raw.PrivateKey)
	if err != nil {
		return "", nil, fmt.Errorf("parse private key %q: private_key is not valid base64: %w", raw.KeyID, err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", nil, fmt.Errorf("parse private key %q: private_key is %d bytes, want %d",
			raw.KeyID, len(priv), ed25519.PrivateKeySize)
	}
	return raw.KeyID, ed25519.PrivateKey(priv), nil
}
