package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sha256Digest formats data's sha256 digest exactly as Task 1's manifest
// package requires it: "sha256:" followed by 64 lowercase hex digits.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// mustURL parses raw and fails the test loudly if it does not parse — every
// test server URL httptest hands back is expected to parse, so a failure
// here means the test itself is broken, not the code under test.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// requireErrorContains asserts err is non-nil and its message names want.
func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

func testLimits() Limits {
	return Limits{MaxBytes: 1 << 20, Timeout: 5 * time.Second}
}

// --- Rule 1 + positive case: stream-hash while reading, compare at the end.

func TestFetch_Success_ReturnsExactBytesWhenDigestMatches(t *testing.T) {
	want := []byte("the quick brown fox jumps over the lazy dog")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(want), testLimits())
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Fetch returned %q, want %q", got, want)
	}
}

func TestFetch_DigestMismatch_ReturnsErrorAndDiscardsBytes(t *testing.T) {
	body := []byte("actual bytes served by the upstream")
	wrongDigest := sha256Digest([]byte("some other content entirely"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), wrongDigest, testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q on digest mismatch; mismatched bytes must be discarded", got)
	}
	requireErrorContains(t, err, "digest mismatch")

	// The error must name BOTH the expected and the actual digest, so an
	// operator can tell "the upstream artifact changed" from "I typed the
	// digest wrong" (brief's mismatch-error requirement).
	wantHex := strings.TrimPrefix(wrongDigest, "sha256:")
	sum := sha256.Sum256(body)
	actualHex := hex.EncodeToString(sum[:])
	requireErrorContains(t, err, wantHex)
	requireErrorContains(t, err, actualHex)
}

// --- Rule 7: empty body is not a special case; it still goes through the
// digest comparison.

func TestFetch_EmptyBody_DigestMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write nothing: a legitimately empty artifact.
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), testLimits())
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Fetch returned %d bytes for an empty body, want 0", len(got))
	}
}

func TestFetch_EmptyBody_DigestMismatchIsStillRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write nothing, but claim a digest for non-empty content.
	}))
	defer srv.Close()

	wrongDigest := sha256Digest([]byte("not actually empty"))
	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), wrongDigest, testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a mismatched empty body", got)
	}
	requireErrorContains(t, err, "digest mismatch")
}

// --- Rule 2: MaxBytes is a hard, finite limit. The test's own server is
// bounded — it writes a fixed number of bytes and returns, never an
// unbounded stream — so that if the mutation in Step 3 (removing the
// LimitReader bound) is applied, this test fails fast on a fixed-size
// over-limit response instead of hanging the process reading forever.

func TestFetch_MaxBytesExceeded_IsRejected(t *testing.T) {
	const maxBytes = 16
	const overBy = 5
	fixedOverLimitBody := make([]byte, maxBytes+overBy)
	for i := range fixedOverLimitBody {
		fixedOverLimitBody[i] = byte('a' + i%26)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixedOverLimitBody) // fixed size; handler returns immediately after.
	}))
	defer srv.Close()

	digest := sha256Digest(fixedOverLimitBody)
	limits := Limits{MaxBytes: maxBytes, Timeout: 5 * time.Second}

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), digest, limits)
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for an over-limit body", got)
	}
	requireErrorContains(t, err, fmt.Sprintf("%d", maxBytes))
}

func TestFetch_BodyExactlyAtMaxBytes_Succeeds(t *testing.T) {
	const maxBytes = 16
	body := make([]byte, maxBytes)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	limits := Limits{MaxBytes: maxBytes, Timeout: 5 * time.Second}
	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(body), limits)
	if err != nil {
		t.Fatalf("Fetch: unexpected error for a body exactly at MaxBytes: %v", err)
	}
	if len(got) != maxBytes {
		t.Fatalf("Fetch returned %d bytes, want exactly %d", len(got), maxBytes)
	}
}

// --- Rule 3: a non-2xx response is an error naming the status code and URL.

func TestFetch_NonSuccessStatus_ReturnsErrorWithStatusAndURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a 404 response", got)
	}
	requireErrorContains(t, err, "404")
	requireErrorContains(t, err, srv.URL)
}

// --- Rule 4: redirects are capped and must not downgrade to http://.

func TestFetch_TooManyRedirects_IsRejected(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always redirect to itself: an unconditional redirect loop. Fetch's
		// own CheckRedirect must be what stops this, not the server.
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a redirect loop", got)
	}
	requireErrorContains(t, err, "redirect")
}

func TestFetch_RedirectDowngradeToHTTP_IsRejected(t *testing.T) {
	target := []byte("plaintext content reached via downgraded redirect")
	digest := sha256Digest(target)

	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(target)
	}))
	defer insecure.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecure.URL, http.StatusFound)
	}))
	defer secure.Close()

	got, err := Fetch(context.Background(), secure.Client(), mustURL(t, secure.URL), digest, testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a redirect that downgraded to http://", got)
	}
	requireErrorContains(t, err, "http")
}

// --- Rule 5: Limits.Timeout bounds the fetch, and ctx cancellation from the
// caller must also interrupt it (not rely solely on client.Timeout).

func TestFetch_TimeoutExceeded_IsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fixed, bounded sleep — longer than the test's Limits.Timeout, but
		// finite regardless of what Fetch does with the deadline.
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("too slow"))
	}))
	defer srv.Close()

	limits := Limits{MaxBytes: 1 << 20, Timeout: 30 * time.Millisecond}
	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), limits)
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q despite exceeding Limits.Timeout", got)
	}
	if err == nil {
		t.Fatalf("Fetch: want error when Limits.Timeout is exceeded, got nil")
	}
}

func TestFetch_ContextCanceledByCaller_InterruptsEvenWithLargeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client goes away (ctx cancellation propagates to
		// the request's context), instead of relying on a fixed sleep.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before Fetch is ever called.

	limits := Limits{MaxBytes: 1 << 20, Timeout: 30 * time.Second} // deliberately large
	got, err := Fetch(ctx, srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), limits)
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q despite an already-canceled context", got)
	}
	if err == nil {
		t.Fatalf("Fetch: want error when ctx is already canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch error %v does not wrap context.Canceled", err)
	}
}

// --- Malformed digest input: Fetch cannot compare against a digest that is
// not "sha256:" + 64 hex digits, so it must refuse before ever dialing.

func TestFetch_MalformedDigest_IsRejectedWithoutDialing(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	for _, digest := range []string{
		"",
		"not-a-digest",
		"sha256:tooshort",
		"md5:d41d8cd98f00b204e9800998ecf8427e",
		"a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4", // missing "sha256:" prefix
	} {
		t.Run(digest, func(t *testing.T) {
			got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), digest, testLimits())
			if got != nil {
				t.Fatalf("Fetch returned non-nil bytes %q for malformed digest %q", got, digest)
			}
			if err == nil {
				t.Fatalf("Fetch: want error for malformed digest %q, got nil", digest)
			}
		})
	}

	if requests != 0 {
		t.Fatalf("server received %d requests for malformed digests, want 0 (Fetch must refuse before dialing)", requests)
	}
}
