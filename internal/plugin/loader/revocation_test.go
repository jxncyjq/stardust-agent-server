package loader

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
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
//
// The row this leaves behind is "loaded" with an explanation that names a
// revocation, and the explanation has to say WHICH package that revocation is
// about or the row means two opposite things at once. That is what the last
// assertion here pins, and it is the reason
// TestNoRowIsLoadedWhileItsOwnEndorsementIsRevoked judges the mounted
// instance's own key rather than looking for the word "revoked" in the same
// explanation: the two would otherwise be asserting opposite things about this
// one row.
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
	// convergence that noticed nothing — and it reports it UNAMBIGUOUSLY. A row
	// that reads "loaded" while its explanation says "revoked" has to say which
	// of the two packages was revoked, or an operator cannot tell whether the
	// plugin they can watch serving requests is the revoked one. So the
	// explanation names the refused package by its own version, says it is a
	// replacement, and names the version that stays mounted.
	for _, want := range []string{revokedReason, "1.0.1", "REPLACEMENT", "1.0.0 stays mounted"} {
		if !strings.Contains(row.LastError, want) {
			t.Errorf("plugin %q reports State=%q with LastError = %q, want it to mention %q: a loaded row "+
				"whose explanation says \"revoked\" has to say which package that is about",
				echoPluginName, row.State, row.LastError, want)
		}
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

// TestNoRowIsLoadedWhileItsOwnEndorsementIsRevoked is the second symptom of the
// same finding, stated against the thing that actually decides it.
//
// server.PluginView.State is mergePluginStatus's copy of THIS row's State, so a
// row reporting "loaded" is this process telling an operator that the plugin is
// serving. What must never be true of such a row is that the key it was MOUNTED
// under — instance.keyID, the endorsement this deployment accepted when it let
// that code in — is one this deployment has since revoked. A panel that shows a
// running plugin under a withdrawn endorsement has nothing left to warn anybody
// with, and the emergency revocation has landed nowhere.
//
// It is deliberately NOT "no loaded row may mention a revocation in its
// explanation". A loaded row legitimately explains a REPLACEMENT that was
// refused as revoked while the trusted instance it failed to replace keeps
// serving — see TestARevokedReplacementLeavesTheTrustedRunningInstanceServing,
// which pins that row and the words that keep it unambiguous. Asserting the
// wording here instead of the endorsement would make these two tests demand
// opposite things of the same row, and each would go on passing only because
// its own scenario never produced the other's.
//
// The deployment converged in each case holds one entry whose key is revoked
// and one whose key is not, so both a loaded row and a failed row are examined;
// a convergence that produced only one kind would make this vacuous, which the
// counts at the end refuse.
//
// It is asserted after each of the ways an instance can come to be in the
// mounted state while its own endorsement is revoked, because the invariant has
// now been broken twice by a way nobody had written it into: once by pass 2
// walking past a mounted instance, and once by the rollback of a failed
// replacement handing it back. Writing it once and converging one scenario
// against it is how a global assertion goes on passing while the thing it
// asserts is reachable.
func TestNoRowIsLoadedWhileItsOwnEndorsementIsRevoked(t *testing.T) {
	// assertNoLoadedRowIsRevoked sweeps every row the Loader reports.
	assertNoLoadedRowIsRevoked := func(t *testing.T, h *harness) {
		t.Helper()

		// The deployment's own answer, asked of the same provider the
		// convergence asked: the invariant is about what THIS deployment
		// currently revokes, not about a keyring the test happens to be
		// holding.
		trust, err := h.loader.trustSet()
		if err != nil {
			t.Fatalf("read the trust set to judge the rows against: %v", err)
		}
		if trust.Keyring == nil {
			t.Fatal("the trust set carries no keyring, so it revokes nothing and every assertion below " +
				"is vacuous")
		}

		loaded, failed := 0, 0
		for _, row := range h.loader.Status() {
			switch row.State {
			case StateLoaded:
				loaded++
				inst := h.loader.instances[row.Name]
				if inst == nil {
					t.Fatalf("plugin %q reports State=%q with no instance behind it", row.Name, row.State)
				}
				if _, gone := trust.Keyring.Revoked(inst.keyID); gone {
					t.Fatalf("plugin %q reports State=%q while the key it was mounted under (%q) is one "+
						"this deployment has revoked; a panel showing a running plugin under a withdrawn "+
						"endorsement has nothing left to warn anybody with (LastError %q)",
						row.Name, row.State, inst.keyID, row.LastError)
				}
			case StateFailed:
				failed++
				if !strings.Contains(row.LastError, revokedReason) {
					t.Errorf("plugin %q reports State=%q with no revocation in its explanation (%q); "+
						"the state and the reason for it must come from the same verdict",
						row.Name, row.State, row.LastError)
				}
			}
		}
		if loaded == 0 || failed == 0 {
			t.Fatalf("examined %d loaded and %d failed rows, want at least one of each: this convergence "+
				"was supposed to revoke one entry's key and leave the other's alone", loaded, failed)
		}
	}

	// mount puts echo up under testKeyID and proxy up under spare-key, and
	// returns the two entries plus the switch that revokes testKeyID.
	mount := func(t *testing.T) (h *harness, echo, proxy manifest.Entry, spare ed25519.PrivateKey, revoke func()) {
		t.Helper()

		priv, spare, live, revokedKeyring := newRevokableTestKey(t)
		current := live
		h = newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
			local: live,
			trustSet: func() (manifest.TrustInput, error) {
				return manifest.TrustInput{Keyring: current}, nil
			},
			requireSignature: true,
		}, RemoteConfig{})

		echo = h.writeEcho("1.0.0")
		proxy = h.writeProxy("1.0.0")
		signPackage(t, filepath.Join(h.root, "echo"), priv)
		signPackageAs(t, filepath.Join(h.root, "proxy"), spare, "spare-key")
		h.apply(echo, proxy)
		return h, echo, proxy, spare, func() { current = revokedKeyring }
	}

	t.Run("the revocation reaches a mounted instance", func(t *testing.T) {
		h, echo, proxy, _, revoke := mount(t)

		revoke()
		if err := h.loader.Apply(context.Background(),
			manifest.Deployment{Plugins: []manifest.Entry{echo, proxy}}, h.root); err == nil {
			t.Fatal("Apply() error = nil after one entry's signing key was revoked, want a refusal")
		}

		assertNoLoadedRowIsRevoked(t, h)
	})

	// The same invariant, reached down the channel that broke it: the revoked
	// instance is displaced by a replacement a still-trusted key endorses, that
	// replacement fails to activate, and the rollback is the thing that would
	// put a revoked instance back. Nothing about the invariant changes; what
	// changes is that this scenario can reach it at all.
	t.Run("the replacement over a revoked instance fails to activate", func(t *testing.T) {
		h, _, proxy, spare, revoke := mount(t)

		revoke()
		broken := h.writeEcho("2.0.0", echoToolName, ghostToolName)
		signPackageAs(t, filepath.Join(h.root, "echo"), spare, "spare-key")
		if err := h.loader.Apply(context.Background(),
			manifest.Deployment{Plugins: []manifest.Entry{broken, proxy}}, h.root); err == nil {
			t.Fatal("Apply() error = nil when the replacement failed to activate, want a refusal")
		}

		assertNoLoadedRowIsRevoked(t, h)
	})
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

// TestARevokedInstanceIsUnloadedWhenTheReplacementFailsAnEarlierCheck is the
// shape a revocation has to survive: the package on disk is refused for a
// reason that has nothing to do with trust, so no revoked verdict is ever
// reached for THAT package, while the instance whose own key this deployment
// has just revoked is still mounted.
//
// Only plugin.wasm is corrupted. plugin.json and plugin.sig are the bytes that
// mounted, so the signature still verifies and the digest check — which runs
// before any policy is applied — is what refuses the package. A convergence
// that discovered revocations only through that refusal would find none, and
// one byte written into the deployment directory would buy an attacker
// "the revoked plugin keeps serving until this process restarts", which is
// exactly what publishing the revocation was meant to prevent.
func TestARevokedInstanceIsUnloadedWhenTheReplacementFailsAnEarlierCheck(t *testing.T) {
	h, revoke, _, _ := newRevocationHarness(t)

	corrupted := appendCustomSection(t, fixtureWasm(t, echoWasmFile), "tampered")
	if err := os.WriteFile(filepath.Join(h.root, "echo", echoWasmFile), corrupted, 0o644); err != nil {
		t.Fatalf("overwrite plugin.wasm: %v", err)
	}

	revoke()
	err := applyEcho(t, h)

	if err == nil {
		t.Fatal("Apply() error = nil after the signing key was revoked, want a refusal")
	}
	// The premise, asserted rather than assumed: if the package on disk were
	// refused AS REVOKED, this test would be a second copy of
	// TestARevocationUnloadsAPluginThatIsAlreadyMounted and would prove nothing
	// about a revocation reached without such a verdict.
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("Apply() error = %v, want the digest check to be what refused the package on disk", err)
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher: an instance taken down "+
			"because its endorsement was withdrawn is a revocation, whatever the package on disk did", err)
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: the instance's own key was revoked, and a package on "+
			"disk that fails an earlier check does not buy it another convergence (LastError %q)",
			echoPluginName, row.State, StateFailed, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0: the record must name the version that was taken down",
			echoPluginName, row.Version)
	}
	if !strings.Contains(row.LastError, revokedReason) {
		t.Errorf("plugin %q: LastError = %q, want it to name the revocation reason %q rather than only the "+
			"digest mismatch: the revocation is why this plugin stopped running",
			echoPluginName, row.LastError, revokedReason)
	}

	if got := h.toolNames(); len(got) != 0 {
		t.Errorf("registered tools = %v, want none: a revoked plugin's tools stay callable", got)
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false: a revoked plugin still offers its tool", echoToolName)
	}
	if got := h.owners(); len(got) != 0 {
		t.Errorf("ledger owners = %v, want none: the revoked instance's resources are still filed", got)
	}
	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, "reason="+reasonRevoked) {
		t.Errorf("%s = %q, want reason=%s", RuntimeEventUnloaded, unloaded[0].Message, reasonRevoked)
	}
}

// TestATrustSetThatCannotBeReadJudgesNoMountedInstance pins the honest answer
// to "is this plugin's endorsement still good?" when the deployment cannot read
// what it trusts.
//
// The answer is NOT "not revoked". A provider that fails says this deployment
// does not know what it trusts (see the TrustSet type), and unloading every
// mounted plugin over a corrupt cache would be a far worse cure than the
// disease — so the instances are left exactly as they are. What must not happen
// is that this passes quietly: the entries report the failure they always did,
// and the one thing no entry can report — that the mounted set went unjudged
// this convergence — is said once, at ERROR.
func TestATrustSetThatCannotBeReadJudgesNoMountedInstance(t *testing.T) {
	priv, _, live, _ := newRevokableTestKey(t)
	broken := errors.New("the trust list cache is corrupt")
	answerable := true
	logs := &bytes.Buffer{}
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			if !answerable {
				return manifest.TrustInput{}, broken
			}
			return manifest.TrustInput{Keyring: live}, nil
		},
		requireSignature: true,
		logger:           slog.New(slog.NewTextHandler(logs, nil)),
	}, RemoteConfig{})

	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	h.apply(entry)
	if row := h.statusOf(echoPluginName); row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q before the provider breaks (LastError %q)",
			echoPluginName, row.State, StateLoaded, row.LastError)
	}

	logs.Reset()
	answerable = false
	err := applyEcho(t, h)

	if err == nil {
		t.Fatal("Apply() error = nil when the trust set could not be read, want a refusal")
	}
	if !errors.Is(err, broken) {
		t.Errorf("Apply() error = %v, want it to wrap the provider's own error", err)
	}

	// The instance is exactly where it was: this convergence judged it against
	// nothing, so it decided nothing about it.
	row := h.statusOf(echoPluginName)
	if row.State != StateLoaded {
		t.Fatalf("plugin %q: State = %q, want %q: a trust set that could not be read is not a verdict about "+
			"anything that is running (LastError %q)", echoPluginName, row.State, StateLoaded, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0", echoPluginName, row.Version)
	}
	wantStrings(t, "registered tools", h.toolNames(), []string{echoToolName})
	if !toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = false, want true: the mounted instance stopped offering its tool",
			echoToolName)
	}
	wantStrings(t, "ledger owners", h.owners(), []string{"plugin:" + echoPluginName + "@1.0.0"})
	if got := h.eventsOfType(RuntimeEventUnloaded); len(got) != 0 {
		t.Errorf("%s events = %v, want none: nothing was judged, so nothing was unloaded",
			RuntimeEventUnloaded, got)
	}

	// Not passed over in silence, in either place. The row says the entry could
	// not be judged, and the convergence says the mounted set could not be.
	if !strings.Contains(row.LastError, "read the trust set to judge plugin") {
		t.Errorf("plugin %q: LastError = %q, want it to report that the trust set could not be read",
			echoPluginName, row.LastError)
	}
	if got := logs.String(); !strings.Contains(got,
		"this convergence cannot judge whether a mounted plugin's endorsement was revoked") {
		t.Errorf("the convergence said nothing about being unable to judge its mounted plugins:\n%s", got)
	}
}

// TestARevocationDoesNotResurrectAnEntryThatLeftTheTargetState pins the one
// name a revocation has nothing to add to: an entry the operator has removed
// from the deployment, whose mounted instance happens to be endorsed by a key
// the same convergence revokes.
//
// It is unloaded either way — that is what leaving the target state means — and
// what must not change is the account of it. An entry that is no longer
// supposed to be running is not a diagnosis, so converge deletes its failure
// record and Status stops reporting it altogether. Taking it down as REVOKED
// instead would file a record the same convergence has just deleted, and an
// operator who removed a plugin would find it back on the panel, failed,
// explaining a revocation they never needed to act on.
func TestARevocationDoesNotResurrectAnEntryThatLeftTheTargetState(t *testing.T) {
	h, revoke, _, _ := newRevocationHarness(t)

	revoke()
	if err := h.loader.Apply(context.Background(), manifest.Deployment{}, h.root); err != nil {
		t.Fatalf("Apply() error = %v, want nil: an entry that left the target state is unloaded, not "+
			"refused", err)
	}

	for _, row := range h.loader.Status() {
		if row.Name == echoPluginName {
			t.Fatalf("plugin %q is still reported (State=%q, LastError=%q) after it left the target "+
				"state; an entry that is not supposed to be running is not a diagnosis",
				row.Name, row.State, row.LastError)
		}
	}

	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, "reason="+reasonManifestRemoved) {
		t.Errorf("%s = %q, want reason=%s: the entry was removed from the deployment, which is why it "+
			"went down", RuntimeEventUnloaded, unloaded[0].Message, reasonManifestRemoved)
	}
	if got := h.toolNames(); len(got) != 0 {
		t.Errorf("registered tools = %v, want none", got)
	}
	if got := h.owners(); len(got) != 0 {
		t.Errorf("ledger owners = %v, want none", got)
	}
}

// mountRevokedEchoWithABrokenLiveSignedReplacement sets up the one shape a
// revocation has to survive that nothing before it covered: the emergency
// revocation and a broken upgrade arrive in the SAME convergence.
//
// echo is mounted under testKeyID and running. Then the trust list revokes
// testKeyID, and the package on disk is replaced by one that is correctly
// signed by a key this deployment STILL trusts — so it passes admit, prepare
// succeeds, and pass 2 unloads the running instance to make room for it — but
// that package declares a tool its guest does not export, so its activation
// fails and something has to be put back.
//
// The something is an instance mounted under a key this deployment has just
// revoked. It must not come back. An operator pushing a bad upgrade at the
// moment a revocation lands is an ordinary Tuesday, and a rollback that undoes
// the revocation would put the emergency's arrival back at "whenever this
// process next restarts" — every convergence, for as long as the bad package
// sits on disk.
//
// It returns the harness, the deployment entry for the broken replacement and
// the log the Loader wrote, with nothing converged yet after the mount.
func mountRevokedEchoWithABrokenLiveSignedReplacement(t *testing.T) (*harness, manifest.Entry, *bytes.Buffer) {
	t.Helper()

	priv, spare, live, revokedKeyring := newRevokableTestKey(t)
	current := live
	logs := &bytes.Buffer{}
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
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
	if inst := h.loader.instances[echoPluginName]; inst == nil || inst.keyID != testKeyID {
		t.Fatalf("the mounted instance was not endorsed by %q, so revoking that key would decide "+
			"nothing about it", testKeyID)
	}

	current = revokedKeyring
	broken := h.writeEcho("2.0.0", echoToolName, ghostToolName)
	signPackageAs(t, filepath.Join(h.root, "echo"), spare, "spare-key")
	logs.Reset()
	return h, broken, logs
}

// TestARevokedInstanceIsNotHandedBackByARollback pins the channel through
// which "an instance whose own endorsement was withdrawn goes on serving" was
// reached a third time.
//
// Pass 2 unloaded it correctly. Then the replacement that displaced it failed
// to activate, and the rollback that exists to undo a failed replacement put
// the revoked instance back — mounted, tools registered, gateable, owning its
// ledger entries, and reporting "loaded" with an explanation that mentioned
// only the activation failure. The revocation had arrived, been acted on, and
// been undone by the same Apply.
//
// So the rollback asks the same question pass 2 asked, of the same reading, and
// refuses. What is left is an entry with nothing mounted and two facts to
// report, and both have to be in the record: the replacement did not activate,
// and what was running was not put back because this deployment withdrew its
// endorsement. Either one alone tells an operator to do the wrong thing —
// "just roll back" for the first, "the upgrade is fine" for the second.
func TestARevokedInstanceIsNotHandedBackByARollback(t *testing.T) {
	h, broken, logs := mountRevokedEchoWithABrokenLiveSignedReplacement(t)

	err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root)

	if err == nil {
		t.Fatal("Apply() error = nil when a replacement failed to activate over a revoked instance, " +
			"want both the activation failure and the refused rollback")
	}
	// The premise, asserted rather than assumed: if the replacement had been
	// refused by admit this would be a second copy of
	// TestARevokedInstanceIsUnloadedWhenTheReplacementFailsAnEarlierCheck and
	// would prove nothing about the rollback.
	if !strings.Contains(err.Error(), ghostToolName) {
		t.Fatalf("Apply() error = %v, want the replacement to have failed at ACTIVATION (its guest does "+
			"not export %q); a replacement refused earlier never reaches the rollback", err, ghostToolName)
	}
	if !strings.Contains(err.Error(), revokedReason) {
		t.Errorf("Apply() error = %v, want it to also say that the instance it displaced was not put back "+
			"because its endorsement was withdrawn (%q)", err, revokedReason)
	}
	if !errors.Is(err, manifest.ErrRevokedPublisher) {
		t.Errorf("Apply() error = %v, want it to wrap manifest.ErrRevokedPublisher: refusing to remount an "+
			"instance whose key was revoked is a revocation, whatever the replacement did", err)
	}

	if inst := h.loader.instances[echoPluginName]; inst != nil {
		t.Fatalf("plugin %q is mounted again under key %q, which this deployment has revoked: the "+
			"rollback undid the revocation this same Apply acted on", echoPluginName, inst.keyID)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: nothing is mounted under this name (LastError %q)",
			echoPluginName, row.State, StateFailed, row.LastError)
	}
	if !strings.Contains(row.LastError, ghostToolName) {
		t.Errorf("plugin %q: LastError = %q, want it to say the replacement failed to activate",
			echoPluginName, row.LastError)
	}
	if !strings.Contains(row.LastError, revokedReason) {
		t.Errorf("plugin %q: LastError = %q, want it to ALSO say the previous instance was not put back "+
			"because its endorsement was withdrawn; an operator reading only the activation failure "+
			"rolls back to a plugin this deployment has revoked", echoPluginName, row.LastError)
	}

	if got := h.toolNames(); len(got) != 0 {
		t.Errorf("registered tools = %v, want none: the revoked instance's tool is callable again", got)
	}
	if toolauth.IsGateable(echoToolName) {
		t.Errorf("IsGateable(%q) = true, want false: the revoked instance still offers its tool",
			echoToolName)
	}
	if got := h.owners(); len(got) != 0 {
		t.Errorf("ledger owners = %v, want none: the revoked instance's resources are filed again", got)
	}

	// The unload's own account. A replacement in flight does not make
	// "replaced" the honest reason when the instance's endorsement had been
	// withdrawn: the event stream is where an operator watches revocations
	// arrive, and this one arrived.
	unloaded := h.eventsOfType(RuntimeEventUnloaded)
	if len(unloaded) != 1 {
		t.Fatalf("want one %s event, got %v", RuntimeEventUnloaded, unloaded)
	}
	if !strings.Contains(unloaded[0].Message, "reason="+reasonRevoked) {
		t.Errorf("%s = %q, want reason=%s rather than reason=%s: the instance went down because this "+
			"deployment withdrew the endorsement it was mounted under",
			RuntimeEventUnloaded, unloaded[0].Message, reasonRevoked, reasonReplaced)
	}
	if got := logs.String(); !strings.Contains(got,
		"a revoked plugin instance was not put back after its replacement failed") {
		t.Errorf("the convergence never said aloud that it refused the rollback:\n%s", got)
	}
}

// TestARollbackRefusedAsRevokedStillReportsAnUnconfirmedDisposal covers the
// worst version of the same convergence: the revoked instance was taken down,
// its disposal reported a failure, its replacement then failed to activate, and
// it is not coming back.
//
// The revoked code may still be running in this process, and that fact has to
// outlive the convergence that found it — see failure.unconfirmedDisposal. It
// is the one path where nothing else would file the note: pass 2 leaves the
// record to whatever comes next when a replacement is in flight, and what came
// next was a failure.
func TestARollbackRefusedAsRevokedStillReportsAnUnconfirmedDisposal(t *testing.T) {
	h, broken, logs := mountRevokedEchoWithABrokenLiveSignedReplacement(t)
	h.ledger.Add(ownerFor(echoPluginName, "1.0.0"), "test-failing-disposer", func() error {
		return errors.New("boom")
	})

	err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root)

	if err == nil {
		t.Fatal("Apply() error = nil, want the disposal failure, the activation failure and the refused " +
			"rollback")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Apply() error = %v, want the failed disposal in it", err)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)",
			echoPluginName, row.State, StateFailed, row.LastError)
	}
	if !strings.Contains(row.LastError, "may still be running in this process") {
		t.Errorf("plugin %q: LastError = %q, want it to report that the revoked plugin's resources were "+
			"never confirmed released", echoPluginName, row.LastError)
	}
	if got := logs.String(); !strings.Contains(got, "a revoked plugin's unload reported a failure") {
		t.Errorf("the failed disposal of a revoked plugin was never said aloud:\n%s", got)
	}

	// Still on the row after the NEXT convergence, whose own failure record
	// rewrites this one. An unload that failed once and was reported once reads,
	// from the second convergence on, exactly like an unload that succeeded, and
	// the row is where an operator looks.
	if err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root); err == nil {
		t.Fatal("Apply() error = nil on the second convergence, want the activation to fail again")
	}
	row = h.statusOf(echoPluginName)
	if !strings.Contains(row.LastError, "may still be running in this process") {
		t.Errorf("plugin %q: LastError = %q after a second convergence, want the unconfirmed disposal "+
			"still in it: the record that replaced it says nothing about those resources either way",
			echoPluginName, row.LastError)
	}
}

// TestARollbackRefusedAsRevokedRepeatsTheUnconfirmedDisposalNote pins the log
// side of the same scenario TestARollbackRefusedAsRevokedStillReportsAnUnconfirmedDisposal
// pins on the row.
//
// The rollback-refused-as-revoked path is a second way (besides prepare
// refusing the entry outright) that a convergence can leave an entry unmounted
// while an earlier convergence's unconfirmed disposal note is still sitting on
// it. "Said again on EVERY convergence that leaves this entry unmounted, not
// only on the one that first recorded it" is a promise about EVERY such way,
// not just the one pass 1 handles — so the second convergence here, which
// finds the same broken package failing activation for the same reason and the
// rollback refused for the same reason, must say the note aloud again, exactly
// as it would if prepare itself had refused the entry.
//
// It must NOT say "a revoked plugin's unload reported a failure" again on that
// second convergence: that sentence is filed once, by the convergence whose
// own unload actually failed, and re-filing it every convergence after would
// read like the disposal kept failing anew rather than having failed once and
// never been confirmed.
func TestARollbackRefusedAsRevokedRepeatsTheUnconfirmedDisposalNote(t *testing.T) {
	h, broken, logs := mountRevokedEchoWithABrokenLiveSignedReplacement(t)
	h.ledger.Add(ownerFor(echoPluginName, "1.0.0"), "test-failing-disposer", func() error {
		return errors.New("boom")
	})

	if err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root); err == nil {
		t.Fatal("Apply() error = nil, want the disposal failure, the activation failure and the refused " +
			"rollback")
	}
	if got := logs.String(); !strings.Contains(got, "a revoked plugin's unload reported a failure") {
		t.Fatalf("the first convergence never said the disposal failed:\n%s", got)
	}
	if got := logs.String(); strings.Contains(got, "a revoked plugin's unload was never confirmed") {
		t.Errorf("the convergence that just filed the disposal failure ALSO announced it as one carried "+
			"over from an earlier convergence, which double-counts the same fact under two different "+
			"sentences on the very convergence that found it:\n%s", got)
	}

	logs.Reset()
	if err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root); err == nil {
		t.Fatal("Apply() error = nil on the second convergence, want the activation and the rollback to " +
			"fail again")
	}
	got := logs.String()
	if !strings.Contains(got, "a revoked plugin's unload was never confirmed") {
		t.Errorf("the second convergence said nothing about the unconfirmed disposal, though the row "+
			"still carries it (see the sibling test on the row):\n%s", got)
	}
	if strings.Contains(got, "a revoked plugin's unload reported a failure") {
		t.Errorf("the second convergence re-filed the disposal failure as if it just happened; it should "+
			"only ever be filed once, by the convergence whose own unload failed:\n%s", got)
	}
}

// TestARollbackRefusedAsRevokedNamesTheVersionThatWasRevoked pins the row's
// Version against the same object its own LastError explains.
//
// revokedInstanceRefusal's text names the version that was mounted under the
// revoked key — prev, the instance the rollback refused to put back — and
// nothing else on the row may name a different one. The version that never
// activated, the broken replacement, has never mounted a single instance under
// this name; an operator comparing the row's Version against their own
// inventory of where the revoked code ran must not be handed that version
// instead. See recordRevokedUnload for why the plain-revocation path (no
// replacement in flight) already gets this right — this pins the rollback path
// getting it right too.
func TestARollbackRefusedAsRevokedNamesTheVersionThatWasRevoked(t *testing.T) {
	h, broken, _ := mountRevokedEchoWithABrokenLiveSignedReplacement(t)

	if err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{broken}}, h.root); err == nil {
		t.Fatal("Apply() error = nil when a replacement failed to activate over a revoked instance, " +
			"want a refusal")
	}

	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q (LastError %q)",
			echoPluginName, row.State, StateFailed, row.LastError)
	}
	if row.Version != "1.0.0" {
		t.Errorf("plugin %q: Version = %q, want 1.0.0: the row must name the version this deployment "+
			"revoked and took down, never 2.0.0, the replacement that failed to activate and never "+
			"mounted a single instance under this name", echoPluginName, row.Version)
	}
	if !strings.Contains(row.LastError, "version 1.0.0 was mounted under key") {
		t.Errorf("plugin %q: LastError = %q, want it to explain the SAME version 1.0.0 that the row's "+
			"own Version field names", echoPluginName, row.LastError)
	}
}

// TestATrustSetThatCannotBeReadSaysNothingAboutAnEmptyMountedSet is the other
// half of TestATrustSetThatCannotBeReadJudgesNoMountedInstance.
//
// That ERROR exists to say one thing: this convergence could not tell whether
// its MOUNTED instances lost their endorsement. With none mounted there is
// nothing it failed to judge, and logging it anyway would put a permanent ERROR
// under every deployment that runs no plugins at all — noise that trains an
// operator to scroll past the line that matters when one IS mounted.
//
// What must not change is the per-entry report: the read failure is still each
// entry's own activation failure, never downgraded to "so nothing was revoked".
// And a convergence with no entry to prepare and no instance to judge has no
// question to ask, so it does not ask the provider one.
func TestATrustSetThatCannotBeReadSaysNothingAboutAnEmptyMountedSet(t *testing.T) {
	priv, _, live, _ := newRevokableTestKey(t)
	broken := errors.New("the trust list cache is corrupt")
	reads := 0
	logs := &bytes.Buffer{}
	h := newHarnessWithOptions(t, defaultTestApplyWait, trustOptions{
		local: live,
		trustSet: func() (manifest.TrustInput, error) {
			reads++
			return manifest.TrustInput{}, broken
		},
		requireSignature: true,
		logger:           slog.New(slog.NewTextHandler(logs, nil)),
	}, RemoteConfig{})

	if err := h.loader.Apply(context.Background(), manifest.Deployment{}, h.root); err != nil {
		t.Fatalf("Apply() error = %v, want nil for a deployment with no entries and nothing mounted", err)
	}
	if reads != 0 {
		t.Errorf("the trust-set provider answered %d question(s) for a convergence with no entry to "+
			"prepare and no instance to judge, want 0", reads)
	}

	entry := h.writeEcho("1.0.0")
	signPackage(t, filepath.Join(h.root, "echo"), priv)
	err := h.loader.Apply(context.Background(),
		manifest.Deployment{Plugins: []manifest.Entry{entry}}, h.root)

	if err == nil {
		t.Fatal("Apply() error = nil when the trust set could not be read, want a refusal")
	}
	if !errors.Is(err, broken) {
		t.Errorf("Apply() error = %v, want it to wrap the provider's own error", err)
	}
	if reads != 1 {
		t.Errorf("the trust-set provider was asked %d time(s) by a convergence with one entry, want 1", reads)
	}
	row := h.statusOf(echoPluginName)
	if row.State != StateFailed {
		t.Fatalf("plugin %q: State = %q, want %q: a deployment that does not know what it trusts mounts "+
			"nothing (LastError %q)", echoPluginName, row.State, StateFailed, row.LastError)
	}
	if !strings.Contains(row.LastError, "read the trust set to judge plugin") {
		t.Errorf("plugin %q: LastError = %q, want the entry to report the read failure as its own",
			echoPluginName, row.LastError)
	}
	if got := logs.String(); strings.Contains(got,
		"this convergence cannot judge whether a mounted plugin's endorsement was revoked") {
		t.Errorf("the convergence reported being unable to judge a mounted set that is empty; there was "+
			"nothing to judge:\n%s", got)
	}
}
