package cli

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"syscall"
)

// ephemeralFallbackPorts is how many explicit ports listenServeAddr tries after
// the operating system refuses to hand out an ephemeral one.
//
// Small on purpose: this is a rescue for a machine whose ephemeral range is
// unusable, not a port scanner. If ten explicit ports in a row also fail, the
// problem is not which port was asked for and the error must say so instead of
// grinding.
const ephemeralFallbackPorts = 10

// fallbackPortRange is where those explicit attempts are drawn from.
//
// It sits BELOW the Windows dynamic range (49152-65535) deliberately. The
// failure this exists for — WSAENOBUFS on a bind to port 0 — is almost always
// that the dynamic range itself has been reserved out from under the process
// (Hyper-V, WSL2 and Docker Desktop each reserve large blocks through winnat),
// so retrying inside that same range would fail for the same reason. Ports in
// the 20000s are outside it and outside the IANA well-known range.
const (
	fallbackPortMin = 20000
	fallbackPortMax = 39999
)

// listenServeAddr opens the service's listener, with one rescue path.
//
// The rescue exists for a real, reproducible Windows failure: binding
// 127.0.0.1:0 returns WSAENOBUFS ("the system lacked sufficient buffer space
// or a queue was full") on a machine whose dynamic port range has been
// reserved away — Hyper-V, WSL2 and Docker Desktop all do this — or exhausted.
// The agent is not the cause and cannot fix the machine, but "ask for a
// specific port instead" gets a working service on it, and the alternative is
// a GUI that cannot start at all.
//
// The rescue is deliberately narrow:
//
//   - only when the caller asked for an EPHEMERAL port (":0"). An operator who
//     named a port meant that port; silently landing on a different one would
//     be the kind of quiet substitution this project forbids.
//   - only for a buffer/queue refusal. A port already in use, a permission
//     denial or an unresolvable host are all answers to the question that was
//     asked, and retrying elsewhere would hide them.
//
// Every attempt after the first is reported by the returned warning, so a
// service that ended up somewhere other than where it asked never does so
// silently. The caller logs it.
func listenServeAddr(addr string) (net.Listener, string, error) {
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener, "", nil
	}
	if !isEphemeralAddr(addr) || !isBufferExhaustion(err) {
		return nil, "", fmt.Errorf("listen on %q: %w", addr, err)
	}

	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		// Unreachable for an address isEphemeralAddr accepted, and still not
		// assumed: returning the original failure keeps the diagnosis about
		// the bind rather than about this parse.
		return nil, "", fmt.Errorf("listen on %q: %w", addr, err)
	}

	for range ephemeralFallbackPorts {
		port := fallbackPortMin + rand.IntN(fallbackPortMax-fallbackPortMin+1)
		candidate := net.JoinHostPort(host, strconv.Itoa(port))
		fallback, ferr := net.Listen("tcp", candidate)
		if ferr == nil {
			warning := fmt.Sprintf(
				"the operating system refused an ephemeral port on %s (%v); listening on %s instead. "+
					"On Windows this usually means the dynamic port range is reserved by Hyper-V/WSL2/Docker "+
					"(`netsh int ipv4 show excludedportrange protocol=tcp`) or exhausted; "+
					"`net stop winnat && net start winnat` releases the reservations",
				addr, err, candidate)
			return fallback, warning, nil
		}
	}

	return nil, "", fmt.Errorf("listen on %q: %w; and %d explicit ports in %d-%d were refused too. "+
		"On Windows this is usually the dynamic port range being reserved away (check "+
		"`netsh int ipv4 show excludedportrange protocol=tcp`; `net stop winnat && net start winnat` "+
		"releases Hyper-V/WSL2/Docker reservations) or ephemeral ports being exhausted "+
		"(`netstat -ano | find /c \"TIME_WAIT\"`)",
		addr, err, ephemeralFallbackPorts, fallbackPortMin, fallbackPortMax)
}

// isEphemeralAddr reports whether addr asks the operating system to choose the
// port — the only case the rescue above applies to.
func isEphemeralAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return strings.TrimSpace(port) == "0"
}

// isBufferExhaustion reports whether err is the "no buffer space / queue full"
// refusal (WSAENOBUFS on Windows, ENOBUFS elsewhere).
//
// It matches on the syscall errno rather than on message text: the message is
// localized — a Chinese-language Windows says "系统缓冲区空间不足" — so a
// substring match would work on the developer's machine and fail on the
// operator's.
func isBufferExhaustion(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	// syscall.ENOBUFS is the POSIX spelling; on Windows the same constant is
	// WSAENOBUFS (10055), which is what a failed bind reports there.
	return errno == syscall.ENOBUFS || errno == windowsWSAENOBUFS
}
