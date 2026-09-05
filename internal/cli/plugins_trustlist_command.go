package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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
			"signing at all. The signature is then checked against the embedded root public key\n" +
			"before it is written.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTrustlistSign(out, trustlist.VerifyDocument, inPath, keyPath, outPath)
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

// runTrustlistSign validates, signs, verifies its own output and only then
// writes it. The order is not interchangeable.
//
// Validation comes first because signing a document the verifier would reject
// teaches everyone that verification is broken, which is worse than not
// signing at all — and it is checked before the key file is even read, so an
// operator with two problems is told about the one they can fix without
// touching a private key.
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
	out io.Writer,
	verify func(listData, sigData []byte) (trustlist.Document, error),
	inPath, keyPath, outPath string,
) error {
	if verify == nil {
		return errors.New("plugins trustlist sign: no verification step was supplied; signing without " +
			"one would write a signature nothing has checked")
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
