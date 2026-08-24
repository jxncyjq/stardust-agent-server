package fetch

import (
	"bytes"
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
	requests := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Always redirect to itself: an unconditional redirect loop. Fetch's
		// own CheckRedirect must be what stops this, not the server and not
		// http.Client's built-in default (which also caps at 10 and also
		// mentions "redirect" in its message — see the exact-message and
		// exact-count assertions below, which a client-default fallback
		// cannot satisfy: the client's default would still stop the chain,
		// but only after issuing 10 requests with a different message).
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a redirect loop", got)
	}
	requireErrorContains(t, err, fmt.Sprintf("stopped after %d redirects", maxRedirects))

	// 1 original request + (maxRedirects-1) followed hops = maxRedirects
	// requests total, matching net/http's own semantics (see fetch.go's
	// Redirects doc comment). If CheckRedirect were never installed at all,
	// http.Client's built-in default would also stop the chain and also
	// produce a message containing "redirect", but only after issuing 10
	// requests — this count pins Fetch's own policy as what actually fired.
	if requests != maxRedirects {
		t.Fatalf("server received %d requests, want exactly %d (1 original + %d followed redirects)", requests, maxRedirects, maxRedirects-1)
	}
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
	// Not "http": that substring appears in almost every URL and error in
	// this suite and could never fail. "downgrade" is the distinctive word
	// in redirectPolicy's refusal message.
	requireErrorContains(t, err, "downgrade")
}

// A previous phase's review in this repository found an AllowedHosts check
// that validated only the first redirect hop — a 302 to a second hop then
// reached anything. This test proves Fetch's downgrade check is not that
// shape: hop 1 stays https (innocent), and only hop 2 downgrades to http.
// The client is configured to trust both TLS servers' self-signed certs
// (InsecureSkipVerify on a clone of the first server's transport) so that
// hop 1 succeeds on its own merits and only the hop-2 downgrade is what
// stops the chain.
func TestFetch_RedirectDowngradeOnSecondHop_IsRejected(t *testing.T) {
	target := []byte("plaintext content reached via a second-hop downgrade")
	digest := sha256Digest(target)

	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(target)
	}))
	defer insecure.Close()

	secondHop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecure.URL, http.StatusFound)
	}))
	defer secondHop.Close()

	firstHop := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secondHop.URL, http.StatusFound)
	}))
	defer firstHop.Close()

	client := firstHop.Client()
	baseTransport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("firstHop.Client().Transport is %T, want *http.Transport", client.Transport)
	}
	transport := baseTransport.Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true
	client.Transport = transport

	got, err := Fetch(context.Background(), client, mustURL(t, firstHop.URL), digest, testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a redirect that downgraded on the second hop", got)
	}
	requireErrorContains(t, err, "downgrade")
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

// --- Limits are validated up front. A bad limit must be reported as a bad
// limit — never let it misdiagnose itself as something else downstream. In
// particular, a negative MaxBytes previously made io.LimitReader yield zero
// bytes, so the failure surfaced as a "digest mismatch" against the empty
// string, which reads exactly like "the upstream artifact changed" when the
// real cause was a bad limit.

func TestFetch_InvalidLimits_AreRejectedWithoutDialing(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	cases := []struct {
		name   string
		limits Limits
		want   string
	}{
		{"MaxBytes -1", Limits{MaxBytes: -1, Timeout: 5 * time.Second}, "MaxBytes"},
		{"MaxBytes -5", Limits{MaxBytes: -5, Timeout: 5 * time.Second}, "MaxBytes"},
		{"Timeout zero", Limits{MaxBytes: 1 << 20, Timeout: 0}, "Timeout"},
		{"Timeout negative", Limits{MaxBytes: 1 << 20, Timeout: -1 * time.Second}, "Timeout"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), tc.limits)
			if got != nil {
				t.Fatalf("Fetch returned non-nil bytes %q for invalid limits %+v", got, tc.limits)
			}
			if err == nil {
				t.Fatalf("Fetch: want error for invalid limits %+v, got nil", tc.limits)
			}
			requireErrorContains(t, err, tc.want)
			if strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("Fetch error %q misdiagnoses invalid limits %+v as a digest mismatch", err.Error(), tc.limits)
			}
		})
	}

	if requests != 0 {
		t.Fatalf("server received %d requests for invalid limits, want 0 (Fetch must refuse before dialing)", requests)
	}
}

// --- The mid-body truncation path (fetch.go's "read body" error) has its
// own distinct test: a server that declares a Content-Length larger than
// what it actually writes, then closes the connection. The handler writes a
// fixed, small number of bytes and returns — bounded regardless of what the
// code under test does with them.

func TestFetch_BodyClosedMidRead_ReturnsReadBodyError(t *testing.T) {
	const declaredLength = 100
	const actuallyWritten = 10

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("test server's ResponseWriter does not support http.Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()

		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", declaredLength)
		buf.Write(bytes.Repeat([]byte("x"), actuallyWritten)) // fixed, small, short of declaredLength.
		buf.Flush()
		// conn.Close() (deferred above) then truncates the body mid-stream:
		// the client sees fewer bytes than Content-Length promised.
	}))
	defer srv.Close()

	got, err := Fetch(context.Background(), srv.Client(), mustURL(t, srv.URL), sha256Digest(nil), testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a body closed mid-read", got)
	}
	requireErrorContains(t, err, "read body")
}

// --- A schemeless URL is handled but was previously untested: it fails
// loudly with net/http's own "unsupported protocol scheme" error rather
// than being silently accepted or misinterpreted.

func TestFetch_SchemelessURL_IsRejected(t *testing.T) {
	u := mustURL(t, "example.com/artifact.zip")
	if u.Scheme != "" {
		t.Fatalf("test setup: url.Parse(%q) produced scheme %q, want empty", u, u.Scheme)
	}

	got, err := Fetch(context.Background(), http.DefaultClient, u, sha256Digest(nil), testLimits())
	if got != nil {
		t.Fatalf("Fetch returned non-nil bytes %q for a schemeless URL", got)
	}
	requireErrorContains(t, err, "unsupported protocol scheme")
}
