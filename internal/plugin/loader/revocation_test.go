package loader

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/toolauth"
)

// newRevokableTestKey mints ONE key pair and returns two trust sets over it:
// one that trusts it and one that has revoked it.
//
// The single key pair is the whole point, and it is what separates this from
// newRevokedTestKey: the package these tests mount is signed once and never
// touched again, so the only thing that changes between the two convergences is
// the deployment's own answer about the key. With two different key pairs under
// one id (newRevokedTestKey's shape) the second convergence would fail
// VERIFICATION instead — a different refusal, reached before any revocation is
// consulted, which would leave the revocation path untested.
//
// The revoking trust set registers a second, live key because sign.ParseKeyring
// refuses a keyring whose every registered key is revoked; it stands for the
// deployment that still trusts somebody, and no longer trusts this signer.
func newRevokableTestKey(t *testing.T) (priv, sparePriv ed25519.PrivateKey, live, revoked *sign.Keyring) {
	t.Helper()

	pub, priv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sparePub, sparePriv, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keys := []map[string]string{
		{"id": string(testKeyID), "algorithm": "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(pub)},
		{"id": "spare-key", "algorithm": "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(sparePub)},
	}
	parse := func(doc map[string]any) *sign.Keyring {
		t.Helper()

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
	live = parse(map[string]any{"keys": keys})
	revoked = parse(map[string]any{
		"keys": keys,
		"revoked": []map[string]string{{
			"key_id":     string(testKeyID),
			"reason":     revokedReason,
			"revoked_at": revokedAt,
		}},
	})
	return priv, sparePriv, live, revoked
}

// revocationHarness mounts the echo package under a trust set that endorses it
// and hands back the switch that revokes its key, plus the log the Loader
// writes. Nothing about the package changes when the switch is thrown: the
// bytes on disk, the signature over them and the deployment entry are the ones
// the mount already succeeded on, so the only thing a later convergence can
// react to is the revocation itself.
//
// It is the shape of the emergency this whole mechanism exists for — a key
// turns out to be compromised, a new trust list says so, and nobody restarts
// the process.
//
// restore puts the trust set back the way it started, which is the correction
// that follows an incident: the list that carried the revocation is itself
// corrected, and the entry may mount again.
func newRevocationHarness(t *testing.T) (h *harness, revoke, restore func(), logs *bytes.Buffer) {
	t.Helper()

	priv, _, live, revoked := newRevokableTestKey(t)
	current := live
	logs = &bytes.Buffer{}
	h = newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			return manifest.TrustInput{Keyring: current}, nil
		},
		requireSignature: true,
		logger:           slog.New(slog.NewTextHandler(logs, nil)),
	}, RemoteConfig{})

	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q before the revocation (LastError %q)",
			echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if !toolauth.IsGateable(echoToolName) {
		t.Fatalf("IsGateable(%q) = false before the revocation: this test's teardown assertions would be vacuous",
			echoToolName)
	}
	return h, func() { current = revoked }, func() { current = live }, logs
}

// applyEcho converges the same, unchanged echo entry again and returns the
// error. It is the "trigger a convergence without touching anything" step: in
// production that is what a grant, a deny or a reload does.
func applyEcho(t *testing.T, h *harness) error {
	t.Helper()

	return h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{entryFor(echoPluginName, "echo", nil, echoToolName)}}, h.root)
}

// TestARevocationUnloadsAPluginThatIsAlreadyMounted is the defect the S2
// end-to-end verification found (step 6, finding C).
//
// The revocation verdict already reached the convergence and was already
// logged, but the entry was still in the target state, so pass 2 walked past it
// and the wasm instance kept running with its tool registered — `/v1/plugins`
// answered "loaded" while carrying the sentence explaining why the same plugin
// had just been refused. An emergency revocation therefore reached a RUNNING
// plugin only at the next restart, which is the one plugin it was published to
// stop.
func TestARevocationUnloadsAPluginThatIsAlreadyMounted(t *testing.T) {
	h, revoke, _, _ := newRevocationHarness(t)

	revoke()
	err := applyEcho(t, h)

	if err == nil {
		t.Fatal("Apply() error = nil after the signing key was revoked, want a refusal")
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher", err)
	}

	// The instance itself, not a report about it: StateLoaded's only source is
	// l.instances, so a row that is no longer loaded is an instance that is no
	// longer mounted.
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: the revoked instance is still mounted",
			echoPluginName, row.State, StateFailed)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0: the record must name the version that was taken down",
			echoPluginName, row.Version)
	}
	if !strings.Contains(row.LastError, revokedReason) {
		t.Errorf("plugin %q: LastError = %q, want it to name the revocation reason %q",
			echoPluginName, row.LastError, revokedReason)
	}

	// Its contributions went with it. Both halves are checked because they are
	// filed under two different owners (see unload): the registry entry is what
	// a call resolves through, and the gateable catalog is what an authorization
	// decision reads.
	if got := h.toolNames(); len(got) != 0 {
		t.Errorf("registered tools = %v, want none: a revoked plugin's tools stay callable", got)
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false: a revoked plugin still offers its tool", echoToolName)
	}
	if got := h.owners(); len(got) != 0 {
		t.Errorf("ledger owners = %v, want none: the revoked instance's resources are still filed", got)
	}

	// The operator-facing account of it, in the reason field that tells a
	// withdrawal of trust apart from a manifest edit.
	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, "reason="+reasonRevoked) {
		t.Errorf("%s = %q, want reason=%s", RuntimeEventUnloaded, unloaded[0].Message, reasonRevoked)
	}

	// The version survives the convergences that follow. From here on nothing
	// is mounted under this name, so every later refusal rewrites the failure
	// record — and a record that answered "" would leave an operator holding
	// "some version of this plugin ran here", which is not something they can
	// match against an inventory of where that code was deployed.
	if err := applyEcho(t, h); err == nil {
		t.Fatal("Apply() error = nil on the convergence after the revocation, want the same refusal")
	}
	if again := h.statusOf(echoPluginName); again.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q on the next convergence, want 1.0.0", echoPluginName, again.Version)
	}
}

// TestARevokedReplacementLeavesTheTrustedRunningInstanceServing draws the line
// between the two things a revocation can be about.
//
// The instance that is running was endorsed by a key this deployment still
// trusts. What is revoked is the key behind a package the operator has since
// dropped into the deployment directory — a REPLACEMENT, and an unfit one. Pass
// 2 unloads over a revocation that reaches the running instance's own
// endorsement, not over one that reaches a package on disk it was never built
// from: judging the second would hand anyone who can write into that directory
// a way to take a healthy, trusted plugin offline by dropping in a package
// signed by a revoked key — while dropping in a plainly corrupt one, which
// every other test here pins as harmless, would not.
func TestARevokedReplacementLeavesTheTrustedRunningInstanceServing(t *testing.T) {
	revokedPriv, livePriv, live, revoked := newRevokableTestKey(t)
	current := live
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			return manifest.TrustInput{Keyring: current}, nil
		},
		requireSignature: true,
	}, RemoteConfig{})

	// The mounted instance is endorsed by "spare-key", which neither trust set
	// ever revokes.
	entry := h.writeEcho("1.0.0")
	signPackageAs(t, filepath.Join(h.root, "echo"), livePriv, "spare-key")
	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q before the replacement (LastError %q)",
			echoPluginName, row.State, StateLoaded, row.LastError)
	}

	// The replacement on disk is endorsed by the key the new trust set revokes.
	current = revoked
	next := h.writeEcho("1.0.1")
	signPackageAs(t, filepath.Join(h.root, "echo"), revokedPriv, testKeyID)

	err := h.loader.Apply(context.Background(), manifest.Deployment{Plugins: []manifest.Entry{next}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil for a replacement signed by a revoked key, want a refusal")
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher", err)
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q: the running instance's own endorsement was never "+
			"revoked, and an unfit replacement does not unmount the plugin it failed to replace "+
			"(LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0: the refused 1.0.1 must not have become the mounted one",
			echoPluginName, row.Version)
	}
	// The refusal is still reported — this is a rejected replacement, not a
	// convergence that noticed nothing.
	if !strings.Contains(row.LastError, revokedReason) {
		t.Errorf("plugin %q: LastError = %q, want it to name the revocation reason %q that refused the "+
			"replacement", echoPluginName, row.LastError, revokedReason)
	}

	// Still serving, in both places a call has to pass through.
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true: the trusted instance stopped offering its tool",
			echoToolName)
	}
	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	if got := h.eventsOfType(RuntimeEventUnloaded); len(got) != 0 {
		t.Errorf("%s events = %v, want none: nothing was unloaded", RuntimeEventUnloaded, got)
	}
}

// TestARevocationIsJudgedAgainstTheTrustSetItWasReadFrom pins which answer the
// unload decision is taken against when the provider is free to give a
// different one every time it is asked — which it is, by contract (see the
// TrustSet type).
//
// The provider here answers with the revoking trust set exactly once and with
// the live one on every call after that. One convergence therefore has two
// possible answers available to it, and only one of them is the answer that
// produced the refusal. A convergence that asked a second time for the unload
// decision would refuse this entry under one trust set and keep it mounted
// under another, converging toward a state nobody declared.
func TestARevocationIsJudgedAgainstTheTrustSetItWasReadFrom(t *testing.T) {
	priv, _, live, revoked := newRevokableTestKey(t)
	current := live
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			answer := current
			current = live
			return manifest.TrustInput{Keyring: answer}, nil
		},
		requireSignature: true,
	}, RemoteConfig{})

	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q before the revocation (LastError %q)",
			echoPluginName, row.State, StateLoaded, row.LastError)
	}

	current = revoked
	if err := applyEcho(t, h); err == nil {
		t.Fatal("Apply() error = nil after the signing key was revoked, want a refusal")
	}
	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: the entry was refused under the trust set this "+
			"convergence read, so that is the trust set its unload has to be decided under",
			echoPluginName, row.State, StateFailed)
	}
}

// TestAnUnconfirmedDisposalSurvivesTheEntryMountingAgain follows the note about
// a release that was never acknowledged past the one event that would otherwise
// erase it.
//
// The sequence: the key is revoked, the forced unload's disposal fails (so the
// revoked plugin's resources were never confirmed released and its code may
// still be running in this process), and then the trust set is CORRECTED and
// the entry mounts again. Mounting again says nothing whatsoever about the
// resources the previous mount never released — but it clears the failure
// record that was carrying that fact, and the row goes back to reading
// "loaded". So the note moves onto the new instance and is said aloud once
// more; a row that reported a clean "loaded" here would be the "unload failed,
// still reported as running" outcome arriving one convergence late.
func TestAnUnconfirmedDisposalSurvivesTheEntryMountingAgain(t *testing.T) {
	h, revoke, restore, logs := newRevocationHarness(t)
	h.ledger.Add(ownerFor(echoPluginName, "1.0.0"), "test-failing-disposer", func() error {
		return errors.New("boom")
	})

	revoke()
	if err := applyEcho(t, h); err == nil {
		t.Fatal("Apply() error = nil when a revoked plugin's disposal failed, want both failures reported")
	}
	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q after the revocation", echoPluginName, row.State, StateFailed)
	}

	logs.Reset()
	restore()
	if err := applyEcho(t, h); err != nil {
		t.Fatalf("Apply() error = %v, want nil once the trust set endorses this package again", err)
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q: the correction should let the entry mount again "+
			"(LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if !strings.Contains(row.LastError, "may still be running") {
		t.Errorf("plugin %q: LastError = %q on the row that now reads %q, want it to still report the "+
			"disposal that was never confirmed: a remount releases nothing the previous mount held",
			echoPluginName, row.LastError, StateLoaded)
	}
	if got := logs.String(); !strings.Contains(got, "a revoked plugin's unload was never confirmed") {
		t.Errorf("the convergence that mounted the entry again said nothing about the unconfirmed "+
			"disposal:\n%s", got)
	}
}

// TestARevokedPluginIsNeverReportedAsLoadedAndRevokedAtOnce is the second
// symptom of the same finding, in the two fields a plugin panel puts side by
// side.
//
// server.PluginView.State is mergePluginStatus's copy of THIS row's State, and
// its Detail is that row's explanation, so a row that is loaded while its
// explanation says the deployment revoked it renders as "running" and "revoked"
// at the same time, with nothing on screen to say which is true. The two must
// agree, and after a revocation they agree by the plugin not running.
func TestARevokedPluginIsNeverReportedAsLoadedAndRevokedAtOnce(t *testing.T) {
	h, revoke, _, _ := newRevocationHarness(t)

	revoke()
	if err := applyEcho(t, h); err == nil {
		t.Fatal("Apply() error = nil after the signing key was revoked, want a refusal")
	}

	for _, row := range h.loader.Status() {
		revokedExplanation := strings.Contains(row.LastError, revokedReason)
		if row.State == StateLoaded && revokedExplanation {
			t.Fatalf("plugin %q reports State=%q while its explanation says it was revoked (%q); "+
				"a panel showing both has no way to say which one is true",
				row.Name, row.State, row.LastError)
		}
		if row.State == StateFailed && !revokedExplanation {
			t.Errorf("plugin %q reports State=%q with no revocation in its explanation (%q); "+
				"the state and the reason for it must come from the same verdict",
				row.Name, row.State, row.LastError)
		}
	}
}

// TestARevokedUnloadThatFailsIsNotReportedAsLoaded pins the worst outcome of
// all: the deployment revoked the plugin, tried to release what it holds, and
// was NOT told the release succeeded. The revoked code may still be running in
// this process.
//
// Two things must hold, and the second is the one that rots quietly. The state
// must not be "loaded" — a failed disposal is not a working plugin, and
// reporting one would hide the leak behind a green row. And the fact must be
// re-stated on EVERY convergence afterwards: an unload that failed once and was
// mentioned once reads, from the next convergence on, exactly like an unload
// that succeeded.
func TestARevokedUnloadThatFailsIsNotReportedAsLoaded(t *testing.T) {
	h, revoke, _, logs := newRevocationHarness(t)
	h.ledger.Add(ownerFor(echoPluginName, "1.0.0"), "test-failing-disposer", func() error {
		return errors.New("boom")
	})

	revoke()
	err := applyEcho(t, h)

	if err == nil {
		t.Fatal("Apply() error = nil when a revoked plugin's disposal failed, want both failures reported")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Apply() error = %v, want it to carry the disposal's own failure", err)
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to still wrap manifest.ErrRevokedPublisher: a disposal that "+
			"failed does not make the revocation something else", err)
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: a revoked plugin whose disposal failed is the LAST thing "+
			"that may be reported as running", echoPluginName, row.State, StateFailed)
	}
	for _, want := range []string{revokedReason, "boom", "may still be running"} {
		if !strings.Contains(row.LastError, want) {
			t.Errorf("plugin %q: LastError = %q, want it to mention %q", echoPluginName, row.LastError, want)
		}
	}
	if got := logs.String(); !strings.Contains(got, "a revoked plugin's unload reported a failure") {
		t.Errorf("the log does not report the failed unload of a revoked plugin:\n%s", got)
	}

	// The next convergence: nothing new happens to this entry (it is not
	// mounted any more, so there is nothing left to unload), and that is
	// exactly when an unconfirmed disposal would go quiet.
	logs.Reset()
	if err := applyEcho(t, h); err == nil {
		t.Fatal("Apply() error = nil on the convergence after the revocation, want the same refusal")
	}
	again := h.statusOf(echoPluginName)
	if again.State != StateFailed {
		t.Fatalf("plugin %q: State = %q on the next convergence, want %q", echoPluginName, again.State, StateFailed)
	}
	if !strings.Contains(again.LastError, "may still be running") {
		t.Errorf("plugin %q: LastError = %q on the next convergence, want it to still report the disposal that "+
			"was never confirmed: an unload that failed silently is an unload that did not happen",
			echoPluginName, again.LastError)
	}
	if got := logs.String(); !strings.Contains(got, "a revoked plugin's unload was never confirmed") {
		t.Errorf("the convergence after the failed unload said nothing about it:\n%s", got)
	}
}

// TestARevocationLeavesTheOtherEntriesConverged: a revocation takes down the
// entry it names and nothing else. Every unload in pass 2 shares one loop, and
// a revocation that also unmounted its neighbours would be a far worse cure
// than the disease.
func TestARevocationLeavesTheOtherEntriesConverged(t *testing.T) {
	priv, spare, live, revokedKeyring := newRevokableTestKey(t)
	current := live
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			return manifest.TrustInput{Keyring: current}, nil
		},
		requireSignature: true,
	}, RemoteConfig{})

	echo := h.writeEcho("1.0.0")
	proxy := h.writeProxy("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	// The neighbour is endorsed by the key that is NOT revoked, so the second
	// convergence has one verdict per entry rather than one verdict for both.
	signPackageAs(t, filepath.Join(h.root, "proxy"), spare, "spare-key")
	h.apply(echo, proxy)

	current = revokedKeyring
	err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{echo, proxy}}, h.root)
	if err == nil {
		t.Fatal("Apply() error = nil after one entry's signing key was revoked, want a refusal for that entry")
	}

	if row := h.statusOf(echoPluginName); row.State != StateFailed {
		t.Errorf("plugin %q: State = %q, want %q", echoPluginName, row.State, StateFailed)
	}
	row := h.statusOf(proxyPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q: the neighbour's own endorsement was never withdrawn "+
			"(LastError %q)", proxyPluginName, row.State, StateLoaded, row.LastError)
	}
	wantStrings(t, "registered tools", h.toolNames(), []string{proxyToolName})
	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + proxyPluginName + "@1.0.0"})
}
