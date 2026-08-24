// Package fetch pulls one remote plugin artifact's bytes over HTTP(S) and
// verifies them against a caller-supplied sha256 digest before ever handing
// them back. It is the narrow, dangerous middle of the remote-source path —
// the first code in this repository that pulls bytes off a network — and it
// deliberately knows nothing beyond that: it does not unpack an archive
// (internal/plugin/loader's job), does not cache anything to disk (also the
// loader's job), does not read policy such as
// plugins.allow_insecure_sources (the caller's job — see Fetch's doc
// comment), and does not know what a "plugin" is at all. It returns bytes or
// an error; nothing it does ever touches the filesystem.
package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// maxRedirects is the largest number of redirect hops Fetch will follow
// before it gives up. It is enforced by Fetch's own CheckRedirect — not by
// relying on http.Client's built-in default of 10 — so that the cap and the
// scheme re-check below (see redirectPolicy) are installed together and
// cannot be separated by a future edit that touches one but not the other.
const maxRedirects = 10

// digestPattern matches the digest shape Task 1's manifest package requires
// of a remote Entry.Digest: the literal prefix "sha256:" followed by 64
// lowercase or uppercase hex digits. Fetch only supports sha256 — a second
// algorithm would be a second way for a remote source to be signed weakly.
var digestPattern = regexp.MustCompile(`^sha256:([0-9a-fA-F]{64})$`)

// Limits bounds a single Fetch call. Both fields are explicit and finite:
// there is no "zero means unlimited" reading of either one. A zero MaxBytes
// makes Fetch reject any response body at all (see the "+1" trick in
// Fetch's doc comment), and a zero Timeout makes the request's context
// already expired before it is even sent — both are degenerate but bounded
// outcomes, never an unbounded one. Callers are expected to pass values that
// actually make sense for the artifact being fetched; Limits only draws the
// boundary, it does not pick a sensible default within it.
type Limits struct {
	// MaxBytes is the hard cap, in bytes, on the response body Fetch will
	// read. A server that claims a small Content-Length but then streams
	// indefinitely cannot exceed this cap: Fetch never reads more than
	// MaxBytes+1 bytes from the response body, regardless of how much more
	// the server sends afterward.
	MaxBytes int64

	// Timeout bounds the entire fetch, including any redirects. It is
	// applied via context.WithTimeout around the outgoing request — not via
	// http.Client.Timeout — so that a caller-supplied ctx cancellation and
	// this deadline compose correctly (see Fetch's doc comment).
	Timeout time.Duration
}

// Fetch retrieves u's bytes over client and verifies them against digest —
// which must be "sha256:" followed by 64 hex digits, the shape Task 1's
// manifest package requires of a remote Entry.Digest — before ever
// returning them. On any failure Fetch returns a nil byte slice: there is
// no partial or "best effort" result, only bytes that have been fully
// verified or no bytes at all.
//
// # Digest verification happens before disk, and before the caller
//
// Fetch streams the response body through both a sha256 hash and an
// in-memory buffer at once (io.TeeReader), and only compares the computed
// digest against the expected one after the full (bounded) body has been
// read. A mismatch is reported as an error — naming both the expected and
// the actual digest, so an operator can tell "the upstream artifact
// changed" from "I typed the digest wrong" — and the mismatched bytes are
// discarded, never returned. This is deliberately NOT "store it, then check
// it": there is nowhere here to store it (Fetch never touches the
// filesystem), and the whole point of checking before returning is that a
// caller can never receive bytes that failed verification, not even
// briefly.
//
// An empty response body is not a special case: it is hashed and compared
// exactly like any other body, so a 0-byte response with the wrong digest
// is still rejected.
//
// # Limits are hard, not advisory
//
// limits.MaxBytes bounds how much of the response body Fetch will ever
// read, via io.LimitReader(body, limits.MaxBytes+1): reading exactly
// MaxBytes+1 bytes is what lets Fetch tell "the body is exactly at the
// limit" (legal) apart from "the body exceeds the limit" (rejected) without
// ever reading more than one byte past the boundary. A response whose
// status code is not 2xx is rejected before any of this, naming the status
// code and u.
//
// limits.Timeout bounds the whole fetch (via context.WithTimeout around
// ctx), and ctx's own cancellation is honored independently: Fetch attaches
// its derived context to the outgoing request (http.NewRequestWithContext),
// so a caller that cancels ctx interrupts the download immediately, even
// when limits.Timeout has not yet elapsed. Relying on client.Timeout alone
// would not give a caller that control, which is why Fetch does not depend
// on it (client's own Timeout, if the caller set one, still applies too,
// but it is never the only bound in effect).
//
// # Redirects
//
// Fetch follows up to 10 redirect hops (installed via a CheckRedirect on a
// shallow copy of client — client itself is never mutated, so it remains
// safe to share across concurrent Fetch calls) and refuses an 11th. If u's
// own scheme is "https", every redirect target must also be "https": a hop
// that downgrades to "http://" is refused immediately, regardless of which
// hop it occurs on, because the caller authorized one URL, not a chain that
// can leave TLS behind on a later hop. If u's own scheme is "http", Fetch
// places no such restriction on redirect targets — whether an http:// u is
// permitted at all is a policy decision this package does not make (see
// below); once made, restricting redirects further would only re-litigate
// a decision that was already accepted for u itself.
//
// # What Fetch does not decide
//
// Fetch does not read plugins.allow_insecure_sources or any other
// configuration: whether an http:// u may be fetched at all is the
// caller's policy decision (a later task's), made before Fetch is ever
// called. Fetch fetches whatever u it is given.
func Fetch(ctx context.Context, client *http.Client, u *url.URL, digest string, limits Limits) ([]byte, error) {
	if client == nil {
		panic("fetch: client is nil")
	}
	if u == nil {
		panic("fetch: u is nil")
	}

	wantHex, err := parseDigest(digest)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: build request: %w", u, err)
	}

	// Shallow-copy client so its CheckRedirect can be set per call without
	// mutating (and therefore racing on) a client instance the caller may
	// be sharing across concurrent Fetch calls. Transport and Jar are
	// reference types and are safe to share via this copy.
	c := *client
	c.CheckRedirect = redirectPolicy(u.Scheme)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: unexpected status %d %s", u, resp.StatusCode, resp.Status)
	}

	hasher := sha256.New()
	// limited never yields more than MaxBytes+1 bytes, no matter how much
	// more the server sends afterward — that is what bounds the read
	// against a hostile or malfunctioning server. Given that cap, ReadAll
	// can only come back in one of two shapes: at most MaxBytes bytes (the
	// body ended within the limit), or exactly MaxBytes+1 bytes (the body
	// reached or exceeded it — LimitReader can never report more). Reading
	// exactly MaxBytes+1 is therefore both the *only* signal "over limit"
	// needs and, deliberately, the only one checked below: the check is
	// written so that it can only ever fire because limited actually
	// capped the read, not as a second, independent measurement of the
	// full body's length that would happen to agree with it.
	limited := io.LimitReader(resp.Body, limits.MaxBytes+1)
	tee := io.TeeReader(limited, hasher)
	body, err := io.ReadAll(tee)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: read body: %w", u, err)
	}
	if int64(len(body)) == limits.MaxBytes+1 {
		return nil, fmt.Errorf("fetch %s: response body exceeds the %d byte limit", u, limits.MaxBytes)
	}

	actualHex := hex.EncodeToString(hasher.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(actualHex)), []byte(strings.ToLower(wantHex))) != 1 {
		return nil, fmt.Errorf("fetch %s: digest mismatch: expected sha256:%s, actual sha256:%s", u, wantHex, actualHex)
	}

	return body, nil
}

// parseDigest validates digest against digestPattern and returns its 64 hex
// digits (without the "sha256:" prefix). Fetch cannot compare against a
// digest that is not this exact shape, so a malformed digest is refused
// here, before any request is even built — a caller that passes a bad
// digest gets a clear, immediate error naming the digest it passed, not a
// dial or a partial download that is then thrown away.
func parseDigest(digest string) (string, error) {
	m := digestPattern.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("digest %q is not \"sha256:\" followed by 64 hex digits", digest)
	}
	return m[1], nil
}

// redirectPolicy builds the CheckRedirect func for one Fetch call, closing
// over originalScheme (u.Scheme, lowercased and compared case-insensitively
// per RFC 3986). It caps the redirect chain at maxRedirects hops and, when
// originalScheme is "https", refuses any hop whose target is not also
// "https" — see Fetch's doc comment for why the restriction only applies
// when the original request was already secure.
func redirectPolicy(originalScheme string) func(req *http.Request, via []*http.Request) error {
	requireHTTPS := strings.EqualFold(originalScheme, "https")
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if requireHTTPS && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf(
				"redirect to %s downgrades from https to %q; the caller authorized one https URL, not a "+
					"chain that can leave TLS behind on a later hop",
				req.URL, req.URL.Scheme,
			)
		}
		return nil
	}
}
