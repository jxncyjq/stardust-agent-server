package loader

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	signPackageAs(t, dir, priv, testKeyID)
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
// "this deployment does not require an endorsement" path — staticTrust maps a
// nil keyring onto exactly that Config (no trust set, RequireSignature false).
// Without a test for it nobody watches the door it opens.
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

// TestNewSaysSoWhenTheDeploymentRequiresNoEndorsement pins the one thing New
// cannot check. A false RequireSignature is a legitimate policy and a forgotten
// field at the same time, and they are indistinguishable from inside the
// constructor -- so the state in which a package nobody endorses mounts with
// nothing recorded is announced once, at Warn, rather than being the quiet
// default it would otherwise be.
func TestNewSaysSoWhenTheDeploymentRequiresNoEndorsement(t *testing.T) {
	h := newHarness(t)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, keyring := newTestKey(t)
	trustSet := func() (manifest.TrustInput, error) { return manifest.TrustInput{Keyring: keyring}, nil }

	if _, err := New(Config{
		Ledger:               h.ledger,
		Deps:                 h.deps,
		Events:               h.events,
		Logger:               logger,
		Gate:                 h.gate,
		ApplyWait:            defaultTestApplyWait,
		MaxConsecutiveFaults: defaultTestMaxFaults,
		TrustSet:             trustSet,
	}); err != nil {
		t.Fatalf("New() error = %v, want nil: a false RequireSignature is a policy, not a wiring error", err)
	}
	if !strings.Contains(logs.String(), "does not require a publisher endorsement") {
		t.Errorf("New() log = %q, want it to say no endorsement is required", logs.String())
	}

	logs.Reset()
	if _, err := New(Config{
		Ledger:               h.ledger,
		Deps:                 h.deps,
		Events:               h.events,
		Logger:               logger,
		Gate:                 h.gate,
		ApplyWait:            defaultTestApplyWait,
		MaxConsecutiveFaults: defaultTestMaxFaults,
		TrustSet:             trustSet,
		RequireSignature:     true,
	}); err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if logs.Len() != 0 {
		t.Errorf("New() requiring an endorsement logged %q, want nothing at Warn or above", logs.String())
	}
}

// TestSignaturePolicyDistinguishesADifferentTrustSet pins what a policy
// comparison has to notice, and it is not just "on or off": `agent plugins
// reload` refuses to converge when the config's policy differs from the running
// Loader's, and a comparison that looked only at KeyringConfigured (or only at
// the number of keys) would call a ROTATED trust set unchanged -- quietly
// converging new plugins against keys the operator has already retired.
func TestSignaturePolicyDistinguishesADifferentTrustSet(t *testing.T) {
	_, keyring := newTestKey(t)
	_, rotated := newTestKeyWithID(t, sign.KeyID("rotated-key"))

	// The endorsement requirement is held at true on every side below, so each
	// comparison here is about the KEYRING half alone.
	if off := SignaturePolicyOf(nil, true); off.KeyringConfigured || len(off.KeyIDs) != 0 {
		t.Errorf("SignaturePolicyOf(nil, true) = %+v, want the policy with no local keyring and no keys", off)
	}
	on := SignaturePolicyOf(keyring, true)
	if !on.KeyringConfigured || len(on.KeyIDs) != 1 || on.KeyIDs[0] != testKeyID {
		t.Fatalf("SignaturePolicyOf(keyring, true) = %+v, want a configured keyring holding exactly %q", on, testKeyID)
	}
	if !on.Equal(SignaturePolicyOf(keyring, true)) {
		t.Errorf("SignaturePolicyOf(keyring, true).Equal(itself) = false, want true")
	}
	if on.Equal(SignaturePolicyOf(nil, true)) {
		t.Errorf("a configured keyring compares equal to no keyring at all; reload would apply a policy change silently")
	}
	if on.Equal(SignaturePolicyOf(rotated, true)) {
		t.Errorf("two trust sets of the same size but different key ids compare equal; a key rotation would look like no change")
	}
	if got := SignaturePolicyOf(rotated, true).String(); !strings.Contains(got, "rotated-key") {
		t.Errorf("SignaturePolicy.String() = %q, want it to name the trusted key so a changed trust set is visible", got)
	}
}

// TestSignaturePolicyDistinguishesTheEndorsementRequirement is the other half
// of the same guard, and it is the half a keyring comparison cannot reach.
//
// Turning "require_signature" off over an UNCHANGED keyring changes no key id
// at all. Without the requirement in the policy, `agent plugins reload` would
// find the two sides equal and converge — a deployment relaxed from "every
// package needs an endorsement" to "none does", applied with "reload succeeded"
// on screen and nothing anywhere saying the requirement moved.
func TestSignaturePolicyDistinguishesTheEndorsementRequirement(t *testing.T) {
	_, keyring := newTestKey(t)

	requiring := SignaturePolicyOf(keyring, true)
	relaxed := SignaturePolicyOf(keyring, false)

	if !requiring.RequireSignature || relaxed.RequireSignature {
		t.Fatalf("SignaturePolicyOf(keyring, true).RequireSignature = %t and (keyring, false) = %t, want true and false",
			requiring.RequireSignature, relaxed.RequireSignature)
	}
	if requiring.Equal(relaxed) {
		t.Error("a policy requiring an endorsement compares equal to one that does not over the same keyring; " +
			"reload would converge a relaxed requirement and report success")
	}
	// And with no keyring on either side, where the keyring half has nothing at
	// all to say: this is the deployment the comparison used to be blind to.
	if SignaturePolicyOf(nil, true).Equal(SignaturePolicyOf(nil, false)) {
		t.Error("with no local keyring, a policy requiring an endorsement compares equal to one that does not")
	}
}

// TestSignaturePolicyStringDescribesTheKeyringAndTheRequirementSeparately keeps
// what the task-4 review's Important #1 was actually about: the rendering must
// never derive the endorsement requirement from the keyring. Back then the type
// carried no requirement at all, so the only honest rendering was one that said
// nothing about it; now it carries the field, so the only honest rendering is
// the one that reports THAT field.
//
// The two still come apart in exactly one deployment: a keyring is configured
// and endorsements are not required. Rendering "an endorsement is required"
// there puts a false statement in front of the operator of a `plugins reload`
// refusal, which is the one place they have to be able to believe what they
// read.
//
// Both directions are pinned, because a rendering that omitted the requirement
// would print two policies that differ only there identically — and those two
// compare unequal, so that is precisely the refusal an operator would be left
// staring at.
func TestSignaturePolicyStringDescribesTheKeyringAndTheRequirementSeparately(t *testing.T) {
	_, keyring := newTestKey(t)

	configuredAndOff := SignaturePolicyOf(keyring, false).String()
	if !strings.Contains(configuredAndOff, "keyring") {
		t.Errorf("SignaturePolicy.String() = %q for a configured keyring, want it to say a local keyring is "+
			"what it describes", configuredAndOff)
	}
	if !strings.Contains(configuredAndOff, "no endorsement is required") {
		t.Errorf("SignaturePolicy.String() = %q for a configured keyring with the requirement OFF, want it to "+
			"say no endorsement is required: a keyring is not by itself a requirement", configuredAndOff)
	}

	configuredAndOn := SignaturePolicyOf(keyring, true).String()
	if !strings.Contains(configuredAndOn, "an endorsement is required") {
		t.Errorf("SignaturePolicy.String() = %q for a configured keyring with the requirement ON, want it to "+
			"say an endorsement is required", configuredAndOn)
	}
	if configuredAndOn == configuredAndOff {
		t.Errorf("SignaturePolicy.String() = %q for both requirement settings over the same keyring; the two "+
			"policies compare unequal, so a reload refusal would print the same sentence twice", configuredAndOn)
	}

	none := SignaturePolicyOf(nil, true).String()
	if !strings.Contains(none, "no local keyring") {
		t.Errorf("SignaturePolicy.String() = %q for no keyring, want it to say there is no local keyring", none)
	}
	if !strings.Contains(none, "an endorsement is required") {
		t.Errorf("SignaturePolicy.String() = %q for no keyring with the requirement ON, want it to say so: a "+
			"deployment with no local keyring may still require an endorsement, and the absence of a keyring is "+
			"not a statement that endorsements are off", none)
	}
}

// TestLoaderReportsThePolicyItWasBuiltWith is the other end of that wire: the
// policy a caller compares against has to be the one this Loader actually
// verifies with, not a value stored beside it.
func TestLoaderReportsThePolicyItWasBuiltWith(t *testing.T) {
	_, keyring := newTestKey(t)
	verifying := newHarnessWith(t, defaultTestApplyWait, keyring)

	// staticTrust makes a non-nil keyring a deployment that requires an
	// endorsement, so that is the requirement this harness was built with.
	if got := verifying.loader.SignaturePolicy(); !got.Equal(SignaturePolicyOf(keyring, true)) {
		t.Errorf("Loader.SignaturePolicy() = %+v, want the policy of the keyring it was built with %+v",
			got, SignaturePolicyOf(keyring, true))
	}
	if !verifying.loader.SignaturePolicy().RequireSignature {
		t.Error("Loader.SignaturePolicy().RequireSignature = false for a Loader built with RequireSignature " +
			"true; a policy that does not report the requirement it was built with cannot notice it changing")
	}

	if got := newHarness(t).loader.SignaturePolicy(); got.KeyringConfigured {
		t.Errorf("Loader.SignaturePolicy() = %+v for a Loader built with no keyring, want the policy with no "+
			"local keyring", got)
	}
}

// newTestKeyringWithRevocation builds a keyring holding two keys, one of which
// is revoked — the realistic shape, because a revoked key stays listed so that
// a refusal can say "this key was trusted and is not any more".
func newTestKeyringWithRevocation(t *testing.T, revokedID sign.KeyID) *sign.Keyring {
	t.Helper()

	revokedPub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	livePub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := map[string]any{
		"keys": []map[string]string{
			{"id": string(revokedID), "algorithm": "ed25519",
				"public_key": base64.StdEncoding.EncodeToString(revokedPub)},
			{"id": string(testKeyID), "algorithm": "ed25519",
				"public_key": base64.StdEncoding.EncodeToString(livePub)},
		},
		"revoked": []map[string]string{{"key_id": string(revokedID), "reason": "laptop stolen"}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode keyring: %v", err)
	}
	keyring, err := sign.ParseKeyring(data)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return keyring
}

// TestSignaturePolicyDistinguishesARevocation is what makes revocation real on
// a running deployment.
//
// A revocation added to a keyring changes NOTHING else an observer can see:
// the revoked key stays listed, so the trusted ids are identical. Without the
// revocations in the policy, `agent plugins reload` would compare the new
// config against the running process, find them equal, converge the manifest
// under the OLD trust set — and print "reload succeeded" while the revoked key
// kept verifying packages.
func TestSignaturePolicyDistinguishesARevocation(t *testing.T) {
	revokedID := sign.KeyID("leaked-key")
	withRevocation := newTestKeyringWithRevocation(t, revokedID)

	policy := SignaturePolicyOf(withRevocation, true)
	if len(policy.RevokedIDs) != 1 || policy.RevokedIDs[0] != revokedID {
		t.Fatalf("SignaturePolicyOf(...).RevokedIDs = %v, want [%s]", policy.RevokedIDs, revokedID)
	}

	// A keyring on both sides, same trusted ids, same requirement, different
	// revocations: this MUST compare unequal, or the reload guard never fires.
	sameKeysNoRevocation := SignaturePolicy{
		KeyringConfigured: true,
		KeyIDs:            policy.KeyIDs,
		RequireSignature:  true,
	}
	if policy.Equal(sameKeysNoRevocation) {
		t.Error("a policy with a revocation compares equal to one without; reload would converge " +
			"under the old trust set and report success")
	}
	if !policy.Equal(SignaturePolicyOf(withRevocation, true)) {
		t.Error("a policy does not compare equal to itself")
	}

	// The same revocation under a deployment that requires NO endorsement. This
	// is the shape the policy used to be blind to end to end: the keyring the
	// policy was computed from went nil under that requirement, so every such
	// policy was the zero value and every revocation added to the document
	// compared unchanged.
	relaxed := SignaturePolicyOf(withRevocation, false)
	if len(relaxed.RevokedIDs) != 1 || relaxed.RevokedIDs[0] != revokedID {
		t.Fatalf("SignaturePolicyOf(..., false).RevokedIDs = %v, want [%s]: a revocation is not a statement "+
			"about whether endorsements are required", relaxed.RevokedIDs, revokedID)
	}
	relaxedNoRevocation := SignaturePolicy{
		KeyringConfigured: true,
		KeyIDs:            relaxed.KeyIDs,
	}
	if relaxed.Equal(relaxedNoRevocation) {
		t.Error(`with "require_signature": false, a policy with a revocation compares equal to one without; ` +
			"reload would mount a package signed by a key the operator had just revoked and report success")
	}
}

// TestSignaturePolicyStringNamesRevocations: the guard's error message is what
// an operator acts on, and "the policies differ" without saying HOW leaves
// them to diff two files by hand.
func TestSignaturePolicyStringNamesRevocations(t *testing.T) {
	got := SignaturePolicyOf(newTestKeyringWithRevocation(t, sign.KeyID("leaked-key")), true).String()

	if !strings.Contains(got, "revoked") || !strings.Contains(got, "leaked-key") {
		t.Errorf("SignaturePolicy.String() = %q, want it to name the revoked key", got)
	}
	// The revoked key is still a trusted-list entry on disk; the rendering must
	// not quietly drop it, or two policies would print identically while
	// comparing unequal.
	if !strings.Contains(got, string(testKeyID)) {
		t.Errorf("SignaturePolicy.String() = %q, want it to still name the live key", got)
	}
}

// --- graded install (S2) ---------------------------------------------------
//
// Everything below is about the three-state provenance verdict a package now
// arrives with, and the policy the Loader applies to it. The fixtures are the
// ones above: newTestKey mints the trust set, signPackage endorses a package
// under it, and the harness's staticTrust maps a keyring onto the Config
// fields.

// newRevokedTestKey mints a key pair and a keyring that registers its public
// half under testKeyID and then REVOKES it, so a package signPackage endorses
// is one this deployment has withdrawn trust from.
//
// A second, live key is registered alongside it because sign.ParseKeyring
// refuses a keyring whose every registered key is revoked — a trust set that
// trusts nothing is not one this test could load a package against, and it is
// also not the situation being tested: this is a deployment that still trusts
// somebody, and no longer trusts this signer.
func newRevokedTestKey(t *testing.T) (ed25519.PrivateKey, *sign.Keyring) {
	t.Helper()

	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	livePub, _, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	doc := map[string]any{
		"keys": []map[string]string{
			{"id": string(testKeyID), "algorithm": "ed25519",
				"public_key": base64.StdEncoding.EncodeToString(pub)},
			{"id": "live-key", "algorithm": "ed25519",
				"public_key": base64.StdEncoding.EncodeToString(livePub)},
		},
		"revoked": []map[string]string{{
			"key_id":     string(testKeyID),
			"reason":     revokedReason,
			"revoked_at": revokedAt,
		}},
	}
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

// What the revocation record in newRevokedTestKey's keyring says. Both are
// asserted on: a refusal that does not carry them leaves an operator with
// "revoked" and no way to tell which incident revoked it.
const (
	revokedReason = "laptop stolen"
	revokedAt     = "2026-08-29T10:00:00Z"
)

// digestOfEchoPackage is the digest of the echo package's plugin.json, computed
// through the SAME function the Loader compares an acceptance against. A test
// that hashed the file itself would pass even if production and test disagreed
// about the format, which is the one thing an acceptance cannot survive.
func digestOfEchoPackage(t *testing.T, h *harness) string {
	t.Helper()

	digest, err := manifest.ManifestDigest(filepath.Join(h.root, "echo"))
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	return digest
}

// staleDigest is a well-formed acceptance that covers some OTHER bytes: 64 hex
// digits no package in these tests hashes to. It stands for the acceptance an
// operator recorded before the package on disk was replaced.
const staleDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestUnsignedWithoutAcceptanceDoesNotMount: no registered publisher endorses
// the package and nobody has accepted it, so it does not mount — and the
// refusal names the file that would have carried an endorsement, the keys this
// deployment would have accepted one from, and the digest an operator has to
// accept to let it through.
func TestUnsignedWithoutAcceptanceDoesNotMount(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	// No signPackage, and entry.AcceptedUnsigned is empty.

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal: nobody endorses this package and nobody has accepted it")
	}
	if !errors.Is(err, manifest.ErrUnsignedNotAccepted) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrUnsignedNotAccepted", err)
	}
	if errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it NOT to wrap manifest.ErrRevokedPublisher: nothing was revoked, "+
			"and the two refusals offer different remedies", err)
	}
	for _, want := range []string{"plugin.sig", string(testKeyID), digestOfEchoPackage(t, h), "nobody has accepted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %v, want it to mention %q", err, want)
		}
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none: nothing may mount from a package nobody stands behind", h.owners())
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false: a refused plugin contributes nothing", echoToolName)
	}
}

// TestUnsignedWithMatchingAcceptanceMounts is the other half: the very same
// unendorsed package mounts once an operator has accepted these exact bytes.
func TestUnsignedWithMatchingAcceptanceMounts(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	entry.AcceptedUnsigned = digestOfEchoPackage(t, h)

	h.apply(entry)

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true: an accepted package mounts", echoToolName)
	}
}

// TestAnAcceptanceMatchesWhicheverHexCaseItWasWrittenIn: manifest.ManifestDigest
// writes lowercase hex, but Entry.AcceptedUnsigned is validated against a
// pattern that accepts either case, so an operator who hand-wrote the digest
// upper-cased has written a legal acceptance. A byte-for-byte comparison would
// answer that with "this package changed" — a tampering alarm about a package
// nothing happened to, which is the one refusal an operator must be able to
// trust.
func TestAnAcceptanceMatchesWhicheverHexCaseItWasWrittenIn(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	lower := digestOfEchoPackage(t, h)
	entry.AcceptedUnsigned = "sha256:" + strings.ToUpper(strings.TrimPrefix(lower, "sha256:"))
	if entry.AcceptedUnsigned == lower {
		t.Fatalf("the upper-cased acceptance %q is identical to the lower-cased one; this test would prove "+
			"nothing about case", entry.AcceptedUnsigned)
	}

	h.apply(entry)

	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
}

// TestUnsignedWithStaleAcceptanceIsReportedAsAChangedPackage: an acceptance
// that covers other bytes is refused, and the refusal must be DISTINGUISHABLE
// from "nobody has accepted it".
//
// The two mean opposite things to whoever reads them. "Nobody has accepted it"
// asks an operator to look at a package and decide. "The package changed since
// it was accepted" tells them somebody already decided about DIFFERENT bytes,
// and the bytes on disk are not those — which is a tampering report, and the
// worst possible answer to it is to accept it again without looking.
//
// So this asserts on both messages at once: each has to carry its own sentence
// and NOT the other's. One message covering both cases would pass a test that
// only checked that each is refused.
func TestUnsignedWithStaleAcceptanceIsReportedAsAChangedPackage(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	onDisk := digestOfEchoPackage(t, h)

	entry.AcceptedUnsigned = staleDigest
	staleErr := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if staleErr == nil {
		t.Fatal("Apply() error = nil for an acceptance that covers other bytes, want a refusal")
	}
	if !errors.Is(staleErr, manifest.ErrUnsignedNotAccepted) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrUnsignedNotAccepted", staleErr)
	}
	for _, want := range []string{"changed since it was accepted", staleDigest, onDisk} {
		if !strings.Contains(staleErr.Error(), want) {
			t.Errorf("Apply() error = %v, want it to mention %q", staleErr, want)
		}
	}
	if strings.Contains(staleErr.Error(), "nobody has accepted") {
		t.Errorf("Apply() error = %v: it reports a CHANGED package with the sentence reserved for one nobody "+
			"ever accepted, which sends the operator to accept it instead of to find out what changed it", staleErr)
	}

	entry.AcceptedUnsigned = ""
	neverErr := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if neverErr == nil {
		t.Fatal("Apply() error = nil for a package nobody accepted, want a refusal")
	}
	if strings.Contains(neverErr.Error(), "changed since it was accepted") {
		t.Errorf("Apply() error = %v: it reports a package nobody ever accepted as one that CHANGED, which "+
			"raises a tampering alarm about a package nothing happened to", neverErr)
	}
	if !strings.Contains(neverErr.Error(), "nobody has accepted") {
		t.Errorf("Apply() error = %v, want it to say nobody has accepted the package", neverErr)
	}
}

// TestStaleAcceptanceIsRefusedEvenWhenSignaturesAreNotRequired pins the rule
// that require_signature false does NOT cover.
//
// That switch says "I do not require an endorsement". An acceptance that does
// not match says something else entirely — that THIS PACKAGE CHANGED since
// somebody looked at it — and whether an endorsement is required has no bearing
// on that. Letting the switch swallow it would turn the one alarm an
// unendorsed deployment still has into a no-op.
func TestStaleAcceptanceIsRefusedEvenWhenSignaturesAreNotRequired(t *testing.T) {
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		trustSet:         func() (manifest.TrustInput, error) { return manifest.TrustInput{}, nil },
		requireSignature: false,
	}, RemoteConfig{})
	entry := h.writeEcho("1.0.0")
	entry.AcceptedUnsigned = staleDigest

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal: the acceptance on record covers other bytes")
	}
	if !strings.Contains(err.Error(), "changed since it was accepted") {
		t.Errorf("Apply() error = %v, want it to report a changed package", err)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none", h.owners())
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false", echoToolName)
	}
}

// TestRevokedIsRefusedEvenWithAnAcceptance: a revocation outranks every
// acceptance. An acceptance says "I looked at these bytes and I am fine with
// them"; a revocation says the publisher who endorsed them is not one this
// deployment runs code from any more, and no amount of accepting changes that.
func TestRevokedIsRefusedEvenWithAnAcceptance(t *testing.T) {
	priv, keyring := newRevokedTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	entry.AcceptedUnsigned = digestOfEchoPackage(t, h)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil for a package signed by a revoked key, want a refusal")
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher", err)
	}
	if errors.Is(err, manifest.ErrUnsignedNotAccepted) {
		t.Errorf("Apply() error = %v, want it NOT to wrap manifest.ErrUnsignedNotAccepted: accepting the "+
			"package is not a remedy for a revoked publisher, and offering it would be a lie", err)
	}
	for _, want := range []string{string(testKeyID), revokedReason, revokedAt} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %v, want it to mention %q", err, want)
		}
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none", h.owners())
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false", echoToolName)
	}
}

// unendorsedMountWarning is the message the Loader logs on every mount of a
// package no registered publisher endorses, in a deployment that has said it
// does not require one.
const unendorsedMountWarning = "plugin package is admitted with no endorsement because this deployment does not require one"

// TestRequireSignatureFalseMountsUnsigned is the upgrade path: a deployment
// that has explicitly said it does not require an endorsement keeps mounting
// its unendorsed plugins, and does not silently lose them all on the release
// that introduced acceptances.
//
// It also pins that the warning is written for every enabled entry on every
// convergence, rather than once per Loader. converge's pass 1 runs prepare for
// each enabled entry, and prepare runs admit before it fingerprints anything —
// so an entry whose package has not changed is judged, and warned about, again.
// A deployment running unendorsed code should be reminded every time it
// converges over one; a single startup line scrolls away and then never appears
// again, however many plugins mount afterwards.
//
// The two convergences below change the package each time, so they assert the
// weaker half of that: two convergences, two warnings. The code satisfies the
// stronger half as well.
func TestRequireSignatureFalseMountsUnsigned(t *testing.T) {
	logs := &bytes.Buffer{}
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		trustSet:         func() (manifest.TrustInput, error) { return manifest.TrustInput{}, nil },
		requireSignature: false,
		logger:           slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}, RemoteConfig{})

	h.apply(h.writeEcho("1.0.0"))
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true", echoToolName)
	}
	if got := strings.Count(logs.String(), unendorsedMountWarning); got != 1 {
		t.Fatalf("the first mount logged %d warnings about running unendorsed code, want 1\nlogs:\n%s", got, logs)
	}

	h.apply(h.writeEcho("1.0.1"))
	if row := h.statusOf(echoPluginName); row.Version != "1.0.1" {
		t.Fatalf("plugin %q: Version = %q, want 1.0.1: the second mount did not happen, so this test would "+
			"prove nothing about the second warning", echoPluginName, row.Version)
	}
	if got := strings.Count(logs.String(), unendorsedMountWarning); got != 2 {
		t.Errorf("two mounts logged %d warnings about running unendorsed code, want 2: the warning is per "+
			"mount, not once per Loader\nlogs:\n%s", got, logs)
	}
}

// TestRequireSignatureFalseStillRefusesRevoked: the switch says "I do not
// require an endorsement", not "I am willing to run code that was withdrawn".
// Only the first sentence was ever said, and this is the test that keeps the
// second from being read into it.
func TestRequireSignatureFalseStillRefusesRevoked(t *testing.T) {
	priv, keyring := newRevokedTestKey(t)
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local:            keyring,
		trustSet:         func() (manifest.TrustInput, error) { return manifest.TrustInput{Keyring: keyring}, nil },
		requireSignature: false,
	}, RemoteConfig{})
	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal: an unrequired endorsement does not extend to a revoked key")
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher", err)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none", h.owners())
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false", echoToolName)
	}
}

// TestTrustSetIsReadOnEveryMount is why the trust set is a provider rather than
// a value.
//
// The fetched trust list refreshes underneath a running process. A trust set
// captured when serve assembled the Loader would leave an emergency revocation
// — the one message the whole distribution mechanism exists to carry — with no
// way to reach the plugins it was published to stop, until somebody restarted
// the agent. So: mount once, widen the revocations WITHOUT restarting, and the
// next mount has to refuse.
func TestTrustSetIsReadOnEveryMount(t *testing.T) {
	priv, live := newTestKey(t)
	revokedPriv, revoked := newRevokedTestKey(t)

	// The mutable trust set is HERE, in the test, and the counter with it: a
	// package-level variable in the Loader would be the very thing this test is
	// checking the Loader does not have.
	current := live
	calls := 0
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			calls++
			return manifest.TrustInput{Keyring: current}, nil
		},
		requireSignature: true,
	}, RemoteConfig{})

	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if calls != 1 {
		t.Fatalf("the trust set was read %d times for one mount, want 1", calls)
	}

	// The revocation arrives. Nothing is restarted and no Config is rebuilt;
	// the only thing that changed is what the provider answers. The new
	// package is signed by the key that trust set revokes.
	current = revoked
	next := h.writeEcho("1.0.1")
	signPackage(t, filepath.Join(h.root, "echo"), revokedPriv)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{next}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil after the trust set revoked the signing key, want a refusal: the " +
			"revocation reached nothing")
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher", err)
	}
	if calls != 2 {
		t.Errorf("the trust set was read %d times across two mounts, want 2: a set read once and kept cannot "+
			"see a revocation that arrives afterwards", calls)
	}
	if row := h.statusOf(echoPluginName); row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0: the revoked 1.0.1 package became the mounted one",
			echoPluginName, row.Version)
	}
}

// TestTrustSetFailureIsAnActivationFailure: a provider that cannot answer is
// not "no trust set". It is a deployment that does not know what it trusts, and
// mounting under that is exactly the silent degradation the provider exists to
// prevent.
func TestTrustSetFailureIsAnActivationFailure(t *testing.T) {
	broken := errors.New("the trust list cache is corrupt")
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		trustSet:         func() (manifest.TrustInput, error) { return manifest.TrustInput{}, broken },
		requireSignature: true,
	}, RemoteConfig{})
	entry := h.writeEcho("1.0.0")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil when the trust set could not be read, want a refusal")
	}
	if !errors.Is(err, broken) {
		t.Errorf("Apply() error = %v, want it to wrap the provider's own error", err)
	}
	if len(h.owners()) != 0 {
		t.Errorf("ledger owners = %v, want none: nothing mounts while it is unknown what this deployment trusts",
			h.owners())
	}
}

// TestAdmitRefusesAProvenanceStateItDoesNotKnow covers admit's default branch,
// which nothing on the three defined states can reach.
//
// It is worth a test precisely because it is unreachable today: the branch
// exists so that ADDING a fourth state is a refusal until somebody decides what
// it means, and a default branch nobody ever executed is one nobody would
// notice had been written to fall through instead.
func TestAdmitRefusesAProvenanceStateItDoesNotKnow(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")
	unknown := manifest.Provenance{State: manifest.ProvenanceState(99)}

	err := h.loader.admit(entry, filepath.Join(h.root, "echo"), manifest.TrustInput{Keyring: keyring}, unknown)
	if err == nil {
		t.Fatal("admit() error = nil for a provenance state it does not know how to judge, want a refusal: " +
			"treating an unhandled state as fine is what a silent trust escalation looks like")
	}
	if !errors.Is(err, manifest.ErrUnsignedNotAccepted) {
		t.Errorf("admit() error = %v, want it to wrap manifest.ErrUnsignedNotAccepted", err)
	}
	if !strings.Contains(err.Error(), unknown.State.String()) {
		t.Errorf("admit() error = %v, want it to name the state it could not judge (%s)", err, unknown.State)
	}
}

// endorsedMountLog is the message the Loader logs on every convergence over a
// package a registered publisher endorses.
const endorsedMountLog = "plugin package is endorsed by a registered publisher"

// testPublisher is the display name the trust set carries for testKeyID. It has
// a space in it on purpose: a log handler quotes such a value, so a test that
// finds it has found the VALUE and not an accidental substring of a key name.
const testPublisher = "Acme Plugin Works"

// TestAnEndorsedMountIsLoggedWithItsPublisherAndKey is the task-4 review's
// Important #3. The graded-install policy table says a package a registered
// publisher endorses mounts AND that the mount is recorded with the publisher
// and the key id — and the second half had no guard at all: deleting the log
// line left all three packages green.
//
// The line is not decoration. It is the only record that says WHICH key an
// endorsed package was admitted on, so it is what an operator reads after a
// key turns out to have been compromised, when the question is which mounts
// have to be undone. A record that names neither the publisher nor the key
// answers that question with "some of them".
func TestAnEndorsedMountIsLoggedWithItsPublisherAndKey(t *testing.T) {
	priv, keyring := newTestKey(t)
	logs := &bytes.Buffer{}
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: keyring,
		trustSet: func() (manifest.TrustInput, error) {
			return manifest.TrustInput{
				Keyring:    keyring,
				Publishers: map[sign.KeyID]string{testKeyID: testPublisher},
			}, nil
		},
		requireSignature: true,
		logger:           slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, RemoteConfig{})
	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)

	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q): an unmounted package would make every "+
			"assertion below vacuous", echoPluginName, row.State, StateLoaded, row.LastError)
	}

	written := logs.String()
	if !strings.Contains(written, endorsedMountLog) {
		t.Fatalf("the mount of an endorsed package logged nothing saying so, want %q\nlogs:\n%s",
			endorsedMountLog, written)
	}
	// The field NAMES are asserted as well as the values: a line that carried
	// the right ids under different keys would be one nothing greps for.
	for _, want := range []string{"key_id", string(testKeyID), "publisher", testPublisher} {
		if !strings.Contains(written, want) {
			t.Errorf("the endorsed-mount log does not mention %q, so it cannot say which key admitted this "+
				"package\nlogs:\n%s", want, written)
		}
	}
}

// TestAnUnendorsedRefusalDoesNotImplyASignatureExists is the task-4 review's
// Important #4, and the rule it guards is the one describeMissingEndorsement
// spends five lines arguing for.
//
// The refusal for a package carrying no signature at all must not be worded as
// though one were there. "This package claims dev-abc signed it" sends an
// operator to find out why dev-abc is not registered; there is no dev-abc here
// and no plugin.sig either, so that sentence would send them looking for
// something that does not exist. The mirror-image mistake is asserting the file
// is ABSENT: this same branch is reached by a deployment that recognises no key
// at all, where a plugin.sig may well be sitting right there and simply never
// got placed against anything.
//
// Neither claim is checkable from what the branch has in hand, so the message
// must make neither.
func TestAnUnendorsedRefusalDoesNotImplyASignatureExists(t *testing.T) {
	_, keyring := newTestKey(t)
	h := newHarnessWith(t, defaultTestApplyWait, keyring)
	entry := h.writeEcho("1.0.0")

	// Not signPackage: this package really carries no plugin.sig, which is what
	// makes the wording below a lie rather than a debatable phrasing.
	sigPath := filepath.Join(h.root, "echo", "plugin.sig")
	if _, statErr := os.Stat(sigPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("os.Stat(%q) error = %v, want fs.ErrNotExist: this test proves nothing unless the package "+
			"under it carries no signature", sigPath, statErr)
	}

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal: nobody endorses this package and nobody has accepted it")
	}
	msg := err.Error()

	// Nothing signed it, so nothing may be reported as having signed it.
	for _, forbidden := range []string{"claims a signature", "claims to be signed", "claims a key"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("Apply() error = %v, want it NOT to contain %q: no plugin.sig exists, so there is no "+
				"claimed signer, and an operator would go looking for a key nobody offered", err, forbidden)
		}
	}
	// And the file's absence is not something this branch knows either.
	for _, forbidden := range []string{"is absent", "is missing", "does not exist", "was not found"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("Apply() error = %v, want it NOT to contain %q: this branch is also reached by a "+
				"deployment that recognises no key, where plugin.sig may be right there", err, forbidden)
		}
	}
	// What it must say instead: where an endorsement would have to live, and
	// which keys it would have to come from.
	for _, want := range []string{sigPath, string(testKeyID)} {
		if !strings.Contains(msg, want) {
			t.Errorf("Apply() error = %v, want it to mention %q", err, want)
		}
	}
}

// TestARefusalSpellsOutADeploymentThatRecognisesNoKey is the task-4 review's
// Minor #1. trustedKeyIDs spells the nil-keyring case out in words instead of
// rendering an empty list, and nothing held it to that.
//
// "trusted keys: []" reads like a keyring that failed to load, which is a
// different place to go and look than a deployment that was configured to
// recognise nobody. The refusal is the only thing an operator sees, so the
// difference has to survive into it.
func TestARefusalSpellsOutADeploymentThatRecognisesNoKey(t *testing.T) {
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		trustSet:         func() (manifest.TrustInput, error) { return manifest.TrustInput{}, nil },
		requireSignature: true,
	}, RemoteConfig{})
	entry := h.writeEcho("1.0.0")

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal: this deployment recognises no signing key and requires " +
			"an endorsement")
	}
	if strings.Contains(err.Error(), "trusted keys: []") {
		t.Errorf("Apply() error = %v, want the empty trust set spelled out rather than rendered as an empty "+
			"list: [] reads like a keyring that failed to load", err)
	}
	if !strings.Contains(err.Error(), "no signing key") {
		t.Errorf("Apply() error = %v, want it to say this deployment recognises no signing key at all", err)
	}
}
