package loader

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// testKeyID is the key id every fixture in this file signs and trusts under.
const testKeyID = sign.KeyID("test-key")

// newTestKey mints a key pair and the keyring that trusts its public half. The
// private key is generated per test and never leaves memory: committing a
// private key — even a test one — trains the wrong muscle.
func newTestKey(t *testing.T) (ed25519.PrivateKey, *sign.Keyring) {
	t.Helper()

	return newTestKeyWithID(t, testKeyID)
}

// newTestKeyWithID is newTestKey under a caller-chosen key id, for the tests
// that need two DIFFERENT trust sets rather than two copies of one.
func newTestKeyWithID(t *testing.T, id sign.KeyID) (ed25519.PrivateKey, *sign.Keyring) {
	t.Helper()

	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := map[string]any{"keys": []map[string]string{{
		"id":         string(id),
		"algorithm":  "ed25519",
		"public_key": base64.StdEncoding.EncodeToString(pub),
	}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode keyring: %v", err)
	}
	keyring, err := sign.ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return priv, keyring
}

// signPackage signs dir/plugin.json's RAW BYTES and writes dir/plugin.sig
// beside it, which is exactly what LoadPackage verifies.
func signPackage(t *testing.T, dir string, priv ed25519.PrivateKey) {
	t.Helper()

	manifestData, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json in %s: %v", dir, err)
	}
	sig, err := sign.Sign(priv, testKeyID, manifestData)
	if err != nil {
		t.Fatalf("sign plugin.json in %s: %v", dir, err)
	}
	doc, err := sign.MarshalSignature(sig)
	if err != nil {
		t.Fatalf("encode plugin.sig for %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), doc, 0o644); err != nil {
		t.Fatalf("write plugin.sig in %s: %v", dir, err)
	}
}

// retagVersion rewrites dir/plugin.json with a different version, leaving the
// declared sha256 correct. It is how a test tampers with a package WITHOUT
// breaking the digest check, so that the failure it provokes can only be the
// signature check — the reason the signature exists at all.
func retagVersion(t *testing.T, dir, version string) {
	t.Helper()

	path := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plugin.json in %s: %v", dir, err)
	}
	var pm manifest.PluginManifest
	if err := json.Unmarshal(data, &pm); err != nil {
		t.Fatalf("decode plugin.json in %s: %v", dir, err)
	}
	pm.Version = version
	wasm, err := os.ReadFile(filepath.Join(dir, "plugin.wasm"))
	if err != nil {
		t.Fatalf("read plugin.wasm in %s: %v", dir, err)
	}
	sum := sha256.Sum256(wasm)
	pm.SHA256 = hex.EncodeToString(sum[:])
	rewritten, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("encode plugin.json for %s: %v", dir, err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatalf("write plugin.json in %s: %v", dir, err)
	}
}

// TestApplyMountsASignedPackageWhenTheDeploymentRequiresSignatures is the
// positive half: with a keyring configured, a package carrying a signature
// made by a trusted key mounts exactly as it always did.
func TestApplyMountsASignedPackageWhenTheDeploymentRequiresSignatures(t *testing.T) {
	priv, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)

	h.apply(entry)

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0", echoPluginName, row.Version)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true: a verified plugin must be mounted", echoToolName)
	}
}

// TestApplyRefusesAnUnsignedPackageWhenAKeyringIsConfigured is the control
// itself: a package with no plugin.sig cannot mount into a deployment that
// requires signatures, and the failure says which file was missing.
func TestApplyRefusesAnUnsignedPackageWhenAKeyringIsConfigured(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error: an unsigned package must not mount where signatures are required")
	}
	if !strings.Contains(err.Error(), "plugin.sig") {
		t.Errorf("Apply() error = %v, want it to name plugin.sig", err)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if !strings.Contains(row.LastError, "plugin.sig") {
		t.Errorf("plugin %q: LastError = %q, want it to say the signature was the problem", echoPluginName, row.LastError)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none: nothing may mount from an unsigned package", h.owners())
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false: a refused plugin contributes nothing", echoToolName)
	}
}

// TestApplyRefusesAPackageWhoseManifestChangedAfterSigning is the whole reason
// the signature exists. The tampered plugin.json keeps a CORRECT sha256, so the
// digest check passes and only the signature can catch it.
func TestApplyRefusesAPackageWhoseManifestChangedAfterSigning(t *testing.T) {
	priv, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	dir := filepath.Join(h.root, "echo")
	signPackage(t, dir, priv)
	retagVersion(t, dir, "9.9.9")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error: plugin.json changed after it was signed")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("Apply() error = %v, want it to say the signature did not verify", err)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none", h.owners())
	}
}

// TestApplyKeepsConvergingWhenOneEntryFailsVerification pins rule 4 of the
// deployment policy: a verification failure is an ordinary activation failure.
// It does not block the other entries, Apply reports it, and the entry shows up
// as failed rather than in some new state of its own.
func TestApplyKeepsConvergingWhenOneEntryFailsVerification(t *testing.T) {
	priv, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	echo := h.writeEcho("1.0.0")
	proxy := h.writeProxy("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	// The proxy package is left unsigned on purpose.

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{echo, proxy}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want the unsigned entry's failure joined into the result")
	}
	if !strings.Contains(err.Error(), proxyPluginName) {
		t.Errorf("Apply() error = %v, want it to name the entry that failed verification", err)
	}

	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q): one entry's signature failure must not block another",
			echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row := h.statusOf(proxyPluginName); row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: a verification failure is an activation failure, not a new state",
			proxyPluginName, row.State, StateFailed)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true: the verified entry must still be mounted", echoToolName)
	}
}

// TestApplySkipsVerificationWhenTheDeploymentHasNoKeyring guards the EXPLICIT
// "this deployment does not require signatures" path. It is the only way a nil
// keyring may arise, and without a test for it nobody watches the door it opens.
func TestApplySkipsVerificationWhenTheDeploymentHasNoKeyring(t *testing.T) {
	h := newHarnessWith(t, defaultTestApplyWait, nil)
	entry := h.writeEcho("1.0.0")
	// No signPackage: there is no plugin.sig anywhere near this package.

	h.apply(entry)

	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true", echoToolName)
	}
}

// TestApplyStillChecksTheDigestWithoutAKeyring pins the half of the contract a
// nil keyring does NOT concede: signatures are off, the sha256 check is not.
func TestApplyStillChecksTheDigestWithoutAKeyring(t *testing.T) {
	h := newHarnessWith(t, defaultTestApplyWait, nil)
	entry := h.writeEcho("1.0.0")
	corrupted := filepath.Join(h.root, "echo", "plugin.wasm")
	if err := os.WriteFile(corrupted, []byte("not the module that was declared"), 0o644); err != nil {
		t.Fatalf("corrupt plugin.wasm: %v", err)
	}

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want an error: the digest check runs with or without a keyring")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("Apply() error = %v, want it to name the sha256 mismatch", err)
	}
}

// TestNewSaysSoWhenItIsBuiltWithoutAKeyring pins the one thing New cannot
// check. A nil Keyring is a legitimate policy and a forgotten field at the same
// time, and they are indistinguishable from inside the constructor -- so the
// state that verifies nothing is announced once, at Warn, rather than being the
// quiet default it would otherwise be.
func TestNewSaysSoWhenItIsBuiltWithoutAKeyring(t *testing.T) {
	h := newHarness(t)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if _, err := New(Config{
		Ledger:    h.ledger,
		Deps:      h.deps,
		Events:    h.events,
		Logger:    logger,
		Gate:      h.gate,
		ApplyWait: defaultTestApplyWait,
	}); err != nil {
		t.Fatalf("New() error = %v, want nil: a nil Keyring is a policy, not a wiring error", err)
	}
	if !strings.Contains(logs.String(), "signature verification is disabled") {
		t.Errorf("New() log = %q, want it to say signature verification is off", logs.String())
	}

	logs.Reset()
	_, keyring := newTestKey(t)
	if _, err := New(Config{
		Ledger:    h.ledger,
		Deps:      h.deps,
		Events:    h.events,
		Logger:    logger,
		Gate:      h.gate,
		ApplyWait: defaultTestApplyWait,
		Keyring:   keyring,
	}); err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if logs.Len() != 0 {
		t.Errorf("New() with a keyring logged %q, want nothing at Warn or above", logs.String())
	}
}

// TestSignaturePolicyDistinguishesADifferentTrustSet pins what a policy
// comparison has to notice, and it is not just "on or off": `agent plugins
// reload` refuses to converge when the config's policy differs from the running
// Loader's, and a comparison that looked only at Enforced (or only at the
// number of keys) would call a ROTATED trust set unchanged -- quietly
// converging new plugins against keys the operator has already retired.
func TestSignaturePolicyDistinguishesADifferentTrustSet(t *testing.T) {
	_, keyring := newTestKey(t)
	_, rotated := newTestKeyWithID(t, sign.KeyID("rotated-key"))

	if off := SignaturePolicyOf(nil); off.Enforced || len(off.KeyIDs) != 0 {
		t.Errorf("SignaturePolicyOf(nil) = %+v, want the unenforced policy with no keys", off)
	}
	on := SignaturePolicyOf(keyring)
	if !on.Enforced || len(on.KeyIDs) != 1 || on.KeyIDs[0] != testKeyID {
		t.Fatalf("SignaturePolicyOf(keyring) = %+v, want it enforced over exactly %q", on, testKeyID)
	}
	if !on.Equal(SignaturePolicyOf(keyring)) {
		t.Errorf("SignaturePolicyOf(keyring).Equal(itself) = false, want true")
	}
	if on.Equal(SignaturePolicyOf(nil)) {
		t.Errorf("an enforcing policy compares equal to the unenforced one; reload would apply a policy change silently")
	}
	if on.Equal(SignaturePolicyOf(rotated)) {
		t.Errorf("two trust sets of the same size but different key ids compare equal; a key rotation would look like no change")
	}
	if got := SignaturePolicyOf(rotated).String(); !strings.Contains(got, "rotated-key") {
		t.Errorf("SignaturePolicy.String() = %q, want it to name the trusted key so a changed trust set is visible", got)
	}
	if got := SignaturePolicyOf(nil).String(); !strings.Contains(got, "not required") {
		t.Errorf("SignaturePolicy.String() = %q, want it to say signatures are not required", got)
	}
}

// TestLoaderReportsThePolicyItWasBuiltWith is the other end of that wire: the
// policy a caller compares against has to be the one this Loader actually
// verifies with, not a value stored beside it.
func TestLoaderReportsThePolicyItWasBuiltWith(t *testing.T) {
	_, keyring := newTestKey(t)
	verifying := newHarnessWith(t, defaultTestApplyWait, keyring)

	if got := verifying.loader.SignaturePolicy(); !got.Equal(SignaturePolicyOf(keyring)) {
		t.Errorf("Loader.SignaturePolicy() = %+v, want the policy of the keyring it was built with %+v",
			got, SignaturePolicyOf(keyring))
	}

	if got := newHarness(t).loader.SignaturePolicy(); got.Enforced {
		t.Errorf("Loader.SignaturePolicy() = %+v for a Loader built with no keyring, want the unenforced policy", got)
	}
}
