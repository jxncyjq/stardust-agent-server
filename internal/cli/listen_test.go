package cli

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These tests cover the ephemeral-port rescue: a machine whose dynamic port
// range has been reserved away (Hyper-V / WSL2 / Docker on Windows) or
// exhausted refuses a bind to port 0 with WSAENOBUFS, which used to make the
// GUI impossible to start on that machine.
//
// The refusal itself cannot be provoked from a test — it is a property of the
// operating system's state — so the two halves are tested separately: the
// decision (isEphemeralAddr / isBufferExhaustion) against constructed errors,
// and the happy path against a real listener.

func TestListenServeAddrReturnsAListenerAndNoWarningOnTheFirstTry(t *testing.T) {
	listener, warning, err := listenServeAddr("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenServeAddr(127.0.0.1:0) error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	})
	if warning != "" {
		t.Errorf("warning = %q, want empty: nothing fell back", warning)
	}
	if _, port, splitErr := net.SplitHostPort(listener.Addr().String()); splitErr != nil {
		t.Errorf("listener address %q is unparseable: %v", listener.Addr(), splitErr)
	} else if port == "0" {
		t.Errorf("listener reports port 0; the OS must have assigned a real one")
	}
}

// TestListenServeAddrDoesNotRescueANamedPort is the rule that keeps this from
// becoming a silent substitution: an operator who named a port meant it, so a
// refusal there is reported, never worked around.
func TestListenServeAddrDoesNotRescueANamedPort(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open the port to collide with: %v", err)
	}
	t.Cleanup(func() {
		if err := taken.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	})

	_, _, err = listenServeAddr(taken.Addr().String())
	if err == nil {
		t.Fatalf("listenServeAddr(%q) error = nil, want the address-in-use failure to propagate",
			taken.Addr())
	}
	if !strings.Contains(err.Error(), "listen on") {
		t.Errorf("error = %v, want it to name the address that was refused", err)
	}
}

func TestIsEphemeralAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:0", true},
		{":0", true},
		{"0.0.0.0:0", true},
		{"127.0.0.1:8080", false},
		{":8080", false},
		{"127.0.0.1", false}, // no port at all: not an ephemeral request
		{"", false},
	}
	for _, tt := range tests {
		if got := isEphemeralAddr(tt.addr); got != tt.want {
			t.Errorf("isEphemeralAddr(%q) = %t, want %t", tt.addr, got, tt.want)
		}
	}
}

// TestIsBufferExhaustionMatchesTheErrnoNotTheText pins why the check is on the
// errno: Windows localizes the message, so a machine running a Chinese-language
// Windows reports "系统缓冲区空间不足" and a substring match would quietly stop
// rescuing exactly where it is needed.
func TestIsBufferExhaustionMatchesTheErrnoNotTheText(t *testing.T) {
	wrapped := fmt.Errorf("listen tcp 127.0.0.1:0: bind: %w", windowsWSAENOBUFS)
	if !isBufferExhaustion(wrapped) {
		t.Errorf("isBufferExhaustion(%v) = false, want true for WSAENOBUFS", wrapped)
	}

	localized := errors.New("listen tcp 127.0.0.1:0: bind: 系统缓冲区空间不足或队列已满")
	if isBufferExhaustion(localized) {
		t.Error("isBufferExhaustion matched on message text; it must key on the errno, " +
			"or a localized Windows silently stops being rescued")
	}
}

func TestIsBufferExhaustionIgnoresUnrelatedFailures(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EADDRINUSE} {
		wrapped := fmt.Errorf("listen tcp 127.0.0.1:0: bind: %w", errno)
		if isBufferExhaustion(wrapped) {
			t.Errorf("isBufferExhaustion(%v) = true, want false: a permission or in-use refusal "+
				"is an answer to the question that was asked, and hiding it behind a retry "+
				"elsewhere would make it unfindable", wrapped)
		}
	}
}

// TestFallbackPortRangeSitsBelowTheWindowsDynamicRange pins the one thing that
// makes the rescue work at all: retrying inside 49152-65535 would hit the very
// range Hyper-V/WSL2/Docker reserved away, so the fallback ports must be below
// it (and above the well-known range).
func TestFallbackPortRangeSitsBelowTheWindowsDynamicRange(t *testing.T) {
	const windowsDynamicStart = 49152
	if fallbackPortMin < 1024 {
		t.Errorf("fallbackPortMin = %d, want it above the well-known range", fallbackPortMin)
	}
	if fallbackPortMax >= windowsDynamicStart {
		t.Errorf("fallbackPortMax = %d, want it below the Windows dynamic range start %d: "+
			"retrying inside the reserved range is what failed in the first place",
			fallbackPortMax, windowsDynamicStart)
	}
	if fallbackPortMin >= fallbackPortMax {
		t.Errorf("fallback range %d-%d is empty", fallbackPortMin, fallbackPortMax)
	}
	if _, err := strconv.Atoi(strconv.Itoa(fallbackPortMax)); err != nil {
		t.Fatalf("unreachable: %v", err)
	}
}
