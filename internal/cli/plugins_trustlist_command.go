package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/plugin/sign"
	"github.com/stardust/legion-agent/internal/plugin/trustlist"
)

// trustlistShowCacheOnlyURL is the address `agent plugins trustlist show`
// hands trustlist.NewStore so that it can build a Store at all.
//
// A Store is built from a URL and a cache directory, and NewStore refuses an
// empty or malformed URL — reasonably, since a Store that cannot refresh is
// not a usable Store. But show only reads the cache, so it has no address to
// give and no way to ask the operator for one that would mean anything.
//
// Hence a placeholder, chosen so that it cannot mislead anyone if it ever
// does reach a human: .invalid is the reserved top-level domain that is
// guaranteed never to resolve (RFC 2606), and the host says in words what the
// command does. A plausible-looking placeholder such as localhost would read
// as a real misconfiguration and send an operator to look for a server that
// was never meant to exist. The placeholder is a fallback for a case that
// should not arise at all: the property this command holds to is the stronger
// one — its output never names an address.
const trustlistShowCacheOnlyURL = "https://show-reads-the-cache-and-fetches-nothing.invalid/trustlist.json"

// newPluginsTrustlistCommand builds `agent plugins trustlist`: the official
// trustlist's publishing side (sign) and its operations side (refresh, show).
//
// It lives in its own file rather than in plugins_command.go, which is
// already over 2200 lines.
func newPluginsTrustlistCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trustlist",
		Short: "Publish, refresh and inspect the official plugin trustlist",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newTrustlistSignCommand(out))
	cmd.AddCommand(newTrustlistRefreshCommand(out))
	cmd.AddCommand(newTrustlistShowCommand(out))
	return cmd
}

// newTrustlistSignCommand builds `agent plugins trustlist sign`, which signs a
// trustlist document on the publisher's own machine with the root private key.
func newTrustlistSignCommand(out io.Writer) *cobra.Command {
	var inPath, keyPath, outPath string
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Sign a trustlist document with the root private key",
		Long: "Sign a trustlist document with the root private key.\n\n" +
			"The document is fully validated before it is signed: signing a document the verifier\n" +
			"would reject teaches everyone that verification is broken, which is worse than not\n" +
			"signing at all. Its serial must then be strictly greater than the serial of the version\n" +
			"in git HEAD, because a list published without advancing the serial is refused by every\n" +
			"user machine with an error that reads like an attack. The signature is finally checked\n" +
			"against the embedded root public key before it is written.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrustlistSign(cmd.Context(), out, trustlist.VerifyDocument, gitHeadSerial,
				inPath, keyPath, outPath)
		},
	}
	cmd.Flags().StringVar(&inPath, "in", "", "the trustlist document to sign")
	cmd.Flags().StringVar(&keyPath, "key", "", "the root private key file")
	cmd.Flags().StringVar(&outPath, "out", "", "where to write the signature document")
	mustMarkFlagRequired(cmd, "in")
	mustMarkFlagRequired(cmd, "key")
	mustMarkFlagRequired(cmd, "out")
	return cmd
}

// runTrustlistSign validates, checks that the serial advanced, signs, verifies
// its own output and only then writes it. The order is not interchangeable.
//
// Validation comes first because signing a document the verifier would reject
// teaches everyone that verification is broken, which is worse than not
// signing at all — and it is checked before the key file is even read, so an
// operator with two problems is told about the one they can fix without
// touching a private key. The serial check follows it for the same reason: a
// document that is not a trustlist has no serial worth comparing, and the
// answer an operator can act on is the earlier of the two problems.
//
// The serial check (step 2) asks previousSerial for the serial of the version
// already published, and refuses anything that is not strictly greater.
// Forgetting to advance the serial is the easiest and most hidden mistake in
// this flow: nothing about the resulting file looks wrong, every user machine
// refuses it, and the error those machines report says the serial went
// backwards — which reads like a rollback attack rather than a forgotten
// bump, so an emergency revocation silently fails to arrive while everyone
// looks for a man in the middle.
//
// previousSerial is a parameter for the same reason verify is: the serial
// already published is recorded in git history, and a step that can only run
// where a repository already carries the document is a step whose refusals and
// whose success path nothing can exercise. Whatever is passed must answer with
// the serial of the version already published, and must report every other
// outcome as an error; one that answers with a number it did not read turns
// this check into a formality.
//
// The self-check (step 4) is not a formality. sign.ParsePrivateKey's
// documentation states that it does not check whether a private key's two
// halves agree: an Ed25519 private key is a seed followed by the public key
// that seed derives, and a hand-edited file can pair one pair's seed with
// another pair's public half. Such a key signs happily and produces a
// signature that verifies against nothing. This is the only place that error
// can be caught while the operator who made it is still standing in front of
// it; anywhere else it surfaces as trustlist.ErrUntrustedList — "trustlist is
// not trusted" — which points in an entirely wrong direction.
//
// The check runs through verify, whose contract is the one
// trustlist.VerifyDocument holds to: check the signature against the embedded
// root public key — the same trust set trustlist.Store applies to a list it
// fetches or reads back from its cache, rather than a second verification
// path assembled here that could drift from it. A verify that holds to that
// contract therefore also refuses a key that is well formed and
// self-consistent but simply is not the root key — the signature such a key
// makes is one no verifier will accept.
//
// verify is a parameter rather than a fixed call because this package cannot
// produce a signature the embedded root accepts: the root private key does
// not live in the repository, by design, and a step that can never be run
// through is a step whose success path nothing can check. Whatever is passed
// must verify against the embedded root and nothing else; a verify that
// accepts more than that turns every check documented above into a formality.
//
// NOTHING IS WRITTEN unless every step above passed. The signature goes out
// through writeFileAtomically so that an interrupted write cannot leave a
// truncated document where a good signature used to be; re-signing a
// trustlist (a new serial supersedes the old one) is a normal operation, so
// an existing file at --out is replaced.
func runTrustlistSign(
	ctx context.Context,
	out io.Writer,
	verify func(listData, sigData []byte) (trustlist.Document, error),
	previousSerial func(ctx context.Context, listPath string) (int64, error),
	inPath, keyPath, outPath string,
) error {
	if verify == nil {
		return errors.New("plugins trustlist sign: no verification step was supplied; signing without " +
			"one would write a signature nothing has checked")
	}
	if previousSerial == nil {
		return errors.New("plugins trustlist sign: no previous-serial lookup was supplied; signing " +
			"without one would let a forgotten serial bump through, and every user machine would then " +
			"refuse the list with an error that says the serial went backwards")
	}
	listPath := strings.TrimSpace(inPath)
	privatePath := strings.TrimSpace(keyPath)
	signaturePath := strings.TrimSpace(outPath)
	switch {
	case listPath == "":
		return errors.New("plugins trustlist sign: --in is empty; name the trustlist document to sign")
	case privatePath == "":
		return errors.New("plugins trustlist sign: --key is empty; there is no key to sign with")
	case signaturePath == "":
		return errors.New("plugins trustlist sign: --out is empty; there is nowhere to write the signature")
	}

	listData, err := os.ReadFile(listPath)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: read %s: %w", listPath, err)
	}
	doc, err := trustlist.ParseDocument(listData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %s is not a valid trustlist, so it will not be "+
			"signed: %w", listPath, err)
	}

	published, err := previousSerial(ctx, listPath)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: the serial already published for %s could not be "+
			"read, so the document will NOT be signed. This step exists because a list published "+
			"without advancing its serial is refused by every user machine, with an error that says "+
			"the serial went backwards and therefore reads like an attack; skipping the check when it "+
			"cannot be answered would hand out exactly that: %w", listPath, err)
	}
	if doc.Serial <= published {
		return fmt.Errorf("plugins trustlist sign: %s carries serial %d, but the version already "+
			"published carries serial %d; a new list must carry a strictly greater one, so this "+
			"document was NOT signed. Published as it stands, every user machine would refuse it and "+
			"report that the serial went backwards — which reads like an attack rather than a "+
			"forgotten serial bump, and an urgent revocation would silently fail to arrive while "+
			"everyone looked for a man in the middle. If the two numbers are equal, the other "+
			"explanation is that this exact document is already committed — re-signing a published "+
			"list (a lost or damaged .sig, a re-sign after fixing line endings, or simply committing "+
			"before signing) lands here too, and advancing the serial by one is the way through",
			listPath, doc.Serial, published)
	}

	keyData, err := os.ReadFile(privatePath)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: read %s: %w", privatePath, err)
	}
	defer clear(keyData)
	keyID, priv, err := sign.ParsePrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: parse %s: %w", privatePath, err)
	}
	defer clear(priv)

	signature, err := sign.Sign(priv, keyID, listData)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %w", err)
	}
	sigData, err := sign.MarshalSignature(signature)
	if err != nil {
		return fmt.Errorf("plugins trustlist sign: %w", err)
	}
	if _, err := verify(listData, sigData); err != nil {
		return fmt.Errorf("plugins trustlist sign: the signature this command just produced does not "+
			"verify against the embedded root public key, so it was NOT written. Either --key is not "+
			"the root key, or its two halves disagree (an Ed25519 private key is a seed followed by "+
			"the public key it derives, and nothing stops a hand-edited file from pairing a seed with "+
			"someone else's public half): %w", err)
	}

	if err := writeFileAtomically(signaturePath, sigData, signatureFileMode); err != nil {
		return fmt.Errorf("plugins trustlist sign: write %s: %w", signaturePath, err)
	}
	if _, err := fmt.Fprintf(out, "signed %s (serial %d, expires %s) with key %q -> %s\n",
		listPath, doc.Serial, doc.ExpiresAt.Format("2006-01-02"), keyID, signaturePath); err != nil {
		return fmt.Errorf("plugins trustlist sign: write output: %w", err)
	}
	return nil
}

// gitLookupTimeout bounds the whole HEAD lookup — both git subprocesses share
// this one budget, they are not given it each. A publisher's repository is
// small and the lookup reads one blob out of it, so a lookup that has not
// finished by now is stuck — on a lock another process holds, on a credential
// prompt, on a filesystem that stopped responding — and a signing command that
// hangs forever is worse than one that says what it was waiting for.
//
// Sharing the budget is the deliberate choice: what a publisher waiting at the
// terminal cares about is how long the command can hang in total, not how the
// time is divided between two invocations they cannot see. The cost is that a
// slow first call leaves the second one less room, which shows up as the second
// call being cancelled rather than the whole lookup timing out cleanly; the
// error names the git command that was running when the deadline passed, so
// that case is still diagnosable.
const gitLookupTimeout = 30 * time.Second

// gitHeadSerial reports the serial carried by the version of listPath that is
// committed in git HEAD, or 0 when HEAD does not carry that path at all.
//
// Zero is the answer for a first publication, and it is a safe one to give
// rather than a special case to plumb through: a trustlist document is only
// valid with a serial of 1 or greater, so "greater than 0" admits every first
// list and nothing else.
//
// Every other way of not getting an answer is an error — not in a git
// repository, no HEAD to read, git missing from PATH, a committed version that
// no longer parses. Answering 0 in those cases would silently disable the one
// check that catches a forgotten serial bump, and would do it in exactly the
// situations where the publisher's setup is already not what it is assumed to
// be.
//
// Presence is asked with ls-tree rather than by reading the blob, because
// ls-tree separates the two answers that matter here: it exits successfully
// with no output when HEAD simply does not carry the path, and fails when the
// question could not be asked at all. Reading the blob conflates them into one
// non-zero exit.
func gitHeadSerial(ctx context.Context, listPath string) (int64, error) {
	dir := filepath.Dir(listPath)
	name := filepath.Base(listPath)
	ctx, cancel := context.WithTimeout(ctx, gitLookupTimeout)
	defer cancel()

	listed, err := runGit(ctx, dir, "ls-tree", "HEAD", "--", name)
	if err != nil {
		return 0, err
	}
	if len(bytes.TrimSpace(listed)) == 0 {
		return 0, nil
	}
	// HEAD:./name resolves name against the directory git runs in, which is
	// the directory holding the document.
	committed, err := runGit(ctx, dir, "show", "HEAD:./"+name)
	if err != nil {
		return 0, err
	}
	doc, err := trustlist.ParseDocument(committed)
	if err != nil {
		return 0, fmt.Errorf("the version of %s committed in git HEAD is not a valid trustlist, so it "+
			"carries no serial to compare against: %w", name, err)
	}
	return doc.Serial, nil
}

// runGit runs one git command in dir and returns its standard output.
//
// git explains a failure on stderr, so an error carrying only the exit status
// would leave an operator holding "exit status 128" and nothing else; the
// stderr text goes into the error instead. A deadline that fired is named as
// well, because a subprocess killed by one reports how it died and never why
// it was killed.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("git %s in %s: %w (%v) %s",
				strings.Join(args, " "), dir, err, ctxErr, detail)
		}
		return nil, fmt.Errorf("git %s in %s: %w %s", strings.Join(args, " "), dir, err, detail)
	}
	return stdout, nil
}

// newTrustlistRefreshCommand builds `agent plugins trustlist refresh`, which
// fetches the trustlist once, now, and reports what came of it.
//
// It runs the whole chain on demand and in the foreground: fetch, verify the
// signature, compare the serial against the cached one, accumulate
// revocations, write the cache, assemble the trust set — and then says what
// came out, so the answer arrives as output rather than as a log line
// somewhere.
func newTrustlistRefreshCommand(out io.Writer) *cobra.Command {
	var url, cacheDir string
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Fetch the trustlist now and report what happened",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := trustlist.NewStore(trustlist.Config{URL: url, CacheDir: cacheDir})
			if err != nil {
				return fmt.Errorf("plugins trustlist refresh: %w", err)
			}
			trust, refreshErr := store.Refresh(cmd.Context())
			// Printed BEFORE the failure is reported, and printed either way:
			// an operator needs two answers, not one — whether this refresh
			// worked, and whether what they already have is still usable. A
			// bare error answers only the first and sends them to run show to
			// learn the second.
			printErr := printTrust(out, trust)
			if printErr != nil {
				printErr = fmt.Errorf("plugins trustlist refresh: %w", printErr)
			}
			if refreshErr != nil {
				return errors.Join(fmt.Errorf("plugins trustlist refresh: %w", refreshErr), printErr)
			}
			return printErr
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the trustlist document's https address")
	cmd.Flags().StringVar(&cacheDir, "cache", "", "the trustlist cache directory")
	mustMarkFlagRequired(cmd, "url")
	mustMarkFlagRequired(cmd, "cache")
	return cmd
}

// newTrustlistShowCommand builds `agent plugins trustlist show`, which reports
// the cached trustlist's status and MAKES NO NETWORK REQUEST.
//
// That is the whole point of having it beside refresh: an operator asking
// "what does this machine trust right now" must get an answer that does not
// depend on the network being up, and must not have their question silently
// turned into a fetch that changes the answer.
//
// It exits zero even when there is nothing to show. An empty cache is the
// normal state of a fresh install, not a failure, and a command whose job is
// to report state has not failed by reporting that the state is empty. A
// damaged cache is a different matter, but trustlist.Store.Current reports
// both through one error value and the sentinel that separates them is not
// exported by that package, so show prints the reason verbatim and leaves
// failing on it to refresh, which does fail.
func newTrustlistShowCommand(out io.Writer) *cobra.Command {
	var cacheDir string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the cached trustlist's status without touching the network",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			store, err := trustlist.NewStore(trustlist.Config{
				URL:      trustlistShowCacheOnlyURL,
				CacheDir: cacheDir,
			})
			if err != nil {
				return fmt.Errorf("plugins trustlist show: %w", err)
			}
			trust, readErr := store.Current()
			if err := printTrust(out, trust); err != nil {
				return fmt.Errorf("plugins trustlist show: %w", err)
			}
			if readErr != nil {
				if _, err := fmt.Fprintf(out, "note: %v\n", readErr); err != nil {
					return fmt.Errorf("plugins trustlist show: write output: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache", "", "the trustlist cache directory")
	mustMarkFlagRequired(cmd, "cache")
	return cmd
}

// printTrust renders one trustlist.Trust.
//
// It is meant to be the only rendering of a Trust in this package. Reporting
// the same thing — what this machine trusts — in two formats would leave an
// operator believing they are looking at two different things, so anything
// that needs to report a Trust must render it here rather than grow a second
// format of its own.
func printTrust(out io.Writer, trust trustlist.Trust) error {
	if _, err := fmt.Fprintf(out, "status: %s\n", trust.Status); err != nil {
		return fmt.Errorf("write trustlist status: %w", err)
	}
	if trust.Keyring == nil {
		// A nil keyring is trustlist's way of saying there is no trust set at
		// all, and the consequence is worth spelling out: with nothing to
		// check a signature against, every plugin is unregistered.
		if _, err := fmt.Fprintln(out,
			"no trust set on this machine; every plugin will be treated as unregistered"); err != nil {
			return fmt.Errorf("write trustlist status: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(out, "serial: %d\nissued: %s\nexpires: %s\n",
		trust.Serial,
		trust.IssuedAt.Format("2006-01-02 15:04:05Z07:00"),
		trust.ExpiresAt.Format("2006-01-02 15:04:05Z07:00")); err != nil {
		return fmt.Errorf("write trustlist status: %w", err)
	}

	ids := trust.Keyring.IDs()
	revoked := trust.Keyring.RevokedIDs()
	revokedSet := make(map[sign.KeyID]bool, len(revoked))
	for _, id := range revoked {
		revokedSet[id] = true
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		label := string(id)
		if p, ok := trust.Publishers[id]; ok {
			label = fmt.Sprintf("%s (%s)", p.DisplayName, id)
		}
		if revokedSet[id] {
			label += " [REVOKED]"
		}
		names = append(names, label)
	}
	sort.Strings(names)
	if _, err := fmt.Fprintf(out, "publishers (%d):\n", len(names)); err != nil {
		return fmt.Errorf("write trustlist publishers: %w", err)
	}
	for _, n := range names {
		if _, err := fmt.Fprintf(out, "  %s\n", n); err != nil {
			return fmt.Errorf("write trustlist publishers: %w", err)
		}
	}

	// The accumulated revocation set can name key ids that are no longer in
	// the current list's keys — that is exactly what "revocations are never
	// forgotten" produces, and it is the only visible evidence of it. Left
	// unsaid, the numbers stop adding up and an operator reads that as
	// something being broken.
	extra := 0
	for _, id := range revoked {
		found := false
		for _, known := range ids {
			if known == id {
				found = true
				break
			}
		}
		if !found {
			extra++
		}
	}
	if extra > 0 {
		if _, err := fmt.Fprintf(out, "%d revoked key(s) no longer listed in the current trustlist are "+
			"still refused on this machine (revocations are never forgotten)\n", extra); err != nil {
			return fmt.Errorf("write trustlist revocations: %w", err)
		}
	}
	return nil
}
