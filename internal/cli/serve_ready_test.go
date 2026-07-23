package cli

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// serveReadyTimeout is how long a test waits for an in-process `serve` to start
// listening. It is generous on purpose: the wait ends as soon as the port
// answers, so the budget only has to cover the slowest plausible startup
// (SQLite open plus migrations on a loaded CI runner), not the common case.
const serveReadyTimeout = 30 * time.Second

// serveReadyPollInterval paces the readiness probes.
const serveReadyPollInterval = 10 * time.Millisecond

// waitForServeListening blocks until addr accepts a TCP connection, returning
// an error if serve exits first or the timeout elapses.
//
// serveDone is the channel the test's `go root.Execute()` reports on; it may be
// nil. Watching it matters as much as the timeout: a serve that failed during
// startup would otherwise be reported as "did not listen in 30s", burying the
// actual error under a wait that was never going to succeed.
//
// Both failure modes return an error naming the address, how long was spent and
// how many probes were made — a readiness wait that gave up quietly would turn
// a slow start into a mystery "connection refused" at the first request, which
// is exactly how this flake presented.
func waitForServeListening(addr string, serveDone <-chan error, timeout time.Duration) error {
	started := time.Now()
	attempts := 0
	var lastErr error
	for {
		select {
		case err := <-serveDone:
			return fmt.Errorf("serve exited after %s without listening on %s: %v", time.Since(started).Round(time.Millisecond), addr, err)
		default:
		}
		attempts++
		conn, err := net.DialTimeout("tcp", addr, serveReadyPollInterval*20)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				return fmt.Errorf("close readiness probe to %s: %w", addr, closeErr)
			}
			return nil
		}
		lastErr = err
		if time.Since(started) >= timeout {
			return fmt.Errorf("serve did not listen on %s within %s (%d dial attempts): %w", addr, timeout, attempts, lastErr)
		}
		time.Sleep(serveReadyPollInterval)
	}
}

// freeAddr reserves a loopback port and releases it, yielding an address that
// nothing is listening on yet — the same trick the serve tests use to hand
// `serve --addr` a port before it starts.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v, want nil", err)
	}
	return addr
}

// The previous wait was 100 fixed 10ms POST retries — a hard 1s ceiling. On CI
// serve's startup (SQLite open + migrations) sometimes crosses it, and the test
// failed with "connection refused" as if the endpoint were broken (runs
// 29938476692 and 29986168680). Readiness must be waited for on a real budget.
func TestWaitForServeListeningWaitsBeyondOneSecond(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)
	go func() {
		time.Sleep(1300 * time.Millisecond)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer listener.Close()
		time.Sleep(2 * time.Second)
	}()

	started := time.Now()
	if err := waitForServeListening(addr, nil, 10*time.Second); err != nil {
		t.Fatalf("waitForServeListening() error = %v, want nil", err)
	}
	if waited := time.Since(started); waited < time.Second {
		t.Fatalf("waitForServeListening() returned after %s, want it to have waited past the old 1s ceiling", waited)
	}
}

func TestWaitForServeListeningReturnsImmediatelyWhenAlreadyListening(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if err := waitForServeListening(listener.Addr().String(), nil, time.Second); err != nil {
		t.Fatalf("waitForServeListening() error = %v, want nil", err)
	}
}

// A serve that died during startup must be reported as that, not as a timeout:
// the exit error is the diagnosis, and waiting out the full budget for it would
// bury it.
func TestWaitForServeListeningReportsServeExit(t *testing.T) {
	t.Parallel()
	done := make(chan error, 1)
	done <- errors.New("open sqlite: permission denied")

	err := waitForServeListening(freeAddr(t), done, 10*time.Second)

	if err == nil {
		t.Fatal("waitForServeListening() error = nil, want the serve exit reported")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("waitForServeListening() error = %v, want it to carry serve's own error", err)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Fatalf("waitForServeListening() error = %v, want it to say serve exited", err)
	}
}

func TestWaitForServeListeningTimesOutLoudly(t *testing.T) {
	t.Parallel()
	addr := freeAddr(t)

	err := waitForServeListening(addr, nil, 150*time.Millisecond)

	if err == nil {
		t.Fatal("waitForServeListening() error = nil, want a timeout error")
	}
	for _, want := range []string{addr, "150ms", "attempt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("waitForServeListening() error = %v, want it to mention %q (address, budget waited, attempts made)", err, want)
		}
	}
}
