package host

import (
	"context"
	"errors"
)

// ErrGuestABI marks a failure in the ABI mechanics themselves: the guest's
// allocator returned a null pointer, a result slot pointed outside linear
// memory, or a region the guest named could not be read or freed. It is a
// fault of the plugin — the contract in internal/plugin/abi is the plugin
// author's to keep — which is why calls that fail this way are counted
// against the plugin's health.
//
// It exists because the failures it names are otherwise indistinguishable
// from any other error: every one of them was a bare fmt.Errorf string that
// errors.Is could not recognise, so no counter could tell an ABI violation
// from a trap or from a caller walking away.
var ErrGuestABI = errors.New("guest abi violation")

// ErrGuestTrap marks a wasm trap: an out-of-bounds access, an unreachable
// instruction, a division by zero — anything wazero aborted the module for.
// Like ErrGuestABI it counts against the plugin's health.
var ErrGuestTrap = errors.New("guest trap")

// ClassifyCallFault decides which plugin/call_failed category one guest call
// failure belongs to, and whether it counts toward the plugin's health.
//
// The two judgements it makes that are NOT obvious:
//
//   - A caller's cancellation is not a fault. The plugin never got the chance
//     to answer, and counting it would let an operator unload a perfectly
//     healthy plugin by interrupting it enough times.
//   - An error matching nothing is counted as a trap rather than ignored.
//     "Unclassified means not a fault" is exactly how a health counter stops
//     counting without anyone noticing; a failure nobody could name is still
//     a call the plugin did not answer.
//
// A deadline IS a fault, and it is checked before everything else: the plugin
// had its whole configured budget (plugins.limits.timeout_ms) and did not
// answer within it. ctx is consulted alongside err because wazero's
// context-done handling can surface a closed module rather than the context
// error itself, so the error chain alone is not a reliable witness.
func ClassifyCallFault(ctx context.Context, err error) (category string, isFault bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CategoryTimeout, true
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "", false
	}
	if errors.Is(err, ErrGuestABI) {
		return CategoryABI, true
	}
	return CategoryTrap, true
}
