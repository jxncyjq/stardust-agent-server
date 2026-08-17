// Package host wraps wazero to compile, instantiate, and invoke a Legion
// WASM plugin guest module against the ABI defined in
// internal/plugin/abi: a runtime constructor, a compile step separated from
// instantiation (so many Instances can share one CompiledModule), and an
// Instance type exposing the three ABI exports as a single Invoke call.
package host

import (
	"context"
	"fmt"

	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// NewRuntime creates a wazero Runtime configured for hosting Legion WASM
// plugins:
//
//   - WithCloseOnContextDone(true), so a cancelled or expired context
//     interrupts an in-flight guest call — including a pure-compute loop the
//     guest never yields from on its own — by closing the module.
//   - WithMemoryLimitPages(memoryPages), capping how far a guest can grow
//     its linear memory, so a runaway allocation traps instead of exhausting
//     host memory.
//
// wasi_snapshot_preview1 is instantiated on the returned Runtime because a
// wasm32-wasip1 guest imports it unconditionally; instantiating a guest
// module without it fails on missing imports.
//
// memoryPages must be greater than zero: it is a required sizing decision
// for every plugin instantiated against this runtime, not a value with a
// sensible default. NewRuntime panics if memoryPages == 0.
func NewRuntime(ctx context.Context, memoryPages uint32) wazero.Runtime {
	if memoryPages == 0 {
		panic("host: NewRuntime: memoryPages must be > 0")
	}

	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(memoryPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)

	// MustInstantiate panics on failure; NewRuntime has no error return and
	// a runtime that cannot host WASI plugins at all is not a state any
	// caller can meaningfully recover from.
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	return rt
}

// Compile parses and validates wasm against rt, producing a CompiledModule
// that can be instantiated one or more times via NewInstance. Compilation is
// kept separate from instantiation because a single compiled module is
// instantiated repeatedly — an instance pool instantiates many Instances
// from one CompiledModule rather than recompiling per instance.
func Compile(ctx context.Context, rt wazero.Runtime, wasm []byte) (wazero.CompiledModule, error) {
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compile plugin module: %w", err)
	}
	return compiled, nil
}

// Instance is one instantiation of a compiled Legion plugin module: a live
// wazero module together with its three resolved ABI exports
// (abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke). An Instance is not
// safe for concurrent use — callers must serialize Invoke calls against a
// given Instance.
type Instance struct {
	mod    api.Module
	alloc  api.Function
	free   api.Function
	invoke api.Function
	dead   bool
}

// NewInstance instantiates compiled against rt as a WASI reactor module
// (WithStartFunctions("_initialize"): reactor modules export no _start, so
// the default start-function configuration would silently do nothing useful)
// and with WithName(""), so that instantiating the same CompiledModule more
// than once — as an instance pool does — never collides on module name.
//
// It resolves the three ABI exports and fails, naming the missing export, if
// any of them is absent.
func NewInstance(ctx context.Context, rt wazero.Runtime, compiled wazero.CompiledModule) (*Instance, error) {
	mod, err := rt.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithStartFunctions("_initialize").WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate plugin module: %w", err)
	}

	alloc := mod.ExportedFunction(abi.ExportAlloc)
	if alloc == nil {
		return nil, fmt.Errorf("instantiate plugin module: missing export %q", abi.ExportAlloc)
	}
	free := mod.ExportedFunction(abi.ExportFree)
	if free == nil {
		return nil, fmt.Errorf("instantiate plugin module: missing export %q", abi.ExportFree)
	}
	invoke := mod.ExportedFunction(abi.ExportInvoke)
	if invoke == nil {
		return nil, fmt.Errorf("instantiate plugin module: missing export %q", abi.ExportInvoke)
	}

	return &Instance{mod: mod, alloc: alloc, free: free, invoke: invoke}, nil
}

// Invoke calls the guest's plugin_invoke export with op and the request body
// in, returning the guest's response body.
//
// A nil or empty in never calls plugin_alloc and never touches guest
// memory: plugin_invoke is called directly with ptr=0, len=0, and it is the
// guest's plugin_invoke contract that governs how it interprets an empty
// body for a given op.
//
// If ctx is cancelled or its deadline expires while a call is in flight,
// wazero closes the underlying module (see NewRuntime's
// WithCloseOnContextDone); Invoke returns an error and the Instance becomes
// permanently Dead. A dead Instance must be discarded — every subsequent
// Invoke call on it fails.
func (i *Instance) Invoke(ctx context.Context, op int32, in []byte) (out []byte, err error) {
	var ptr uint64
	if len(in) > 0 {
		res, aerr := i.alloc.Call(ctx, uint64(len(in)))
		if aerr != nil {
			i.dead = true
			return nil, fmt.Errorf("alloc %d bytes: %w", len(in), aerr)
		}
		ptr = res[0]
		if ptr == 0 {
			return nil, fmt.Errorf("alloc %d bytes: guest returned null pointer", len(in))
		}
		if !i.mod.Memory().Write(uint32(ptr), in) {
			return nil, fmt.Errorf("write %d bytes at %d: out of range", len(in), ptr)
		}
		// Free the input with a cancellation-scrubbed context: if ctx is
		// already cancelled by the time this defer runs, calling free with
		// ctx unchanged would itself fail immediately and leak the guest
		// memory plugin_alloc just reserved.
		defer func() {
			if _, ferr := i.free.Call(context.WithoutCancel(ctx), ptr, uint64(len(in))); ferr != nil {
				// A free call failing means wazero closed the module (a
				// trap or context-death aborts the whole module, not just
				// this call); the Instance must not be reused even though
				// the earlier invoke succeeded.
				i.dead = true
				if err == nil {
					err = fmt.Errorf("free input %d bytes at %d: %w", len(in), ptr, ferr)
				}
			}
		}()
	}

	res, ierr := i.invoke.Call(ctx, uint64(uint32(op)), ptr, uint64(len(in)))
	if ierr != nil {
		i.dead = true
		return nil, fmt.Errorf("invoke op=%d: %w", op, ierr)
	}

	outPtr, outLen := abi.UnpackResult(res[0])
	if outLen == 0 {
		return nil, nil
	}

	buf, ok := i.mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("read result at %d len %d: out of range", outPtr, outLen)
	}
	// Copy immediately: buf aliases the guest's linear memory directly, and
	// that memory can grow (and therefore move) or be reused by the guest's
	// allocator on a later call. Returning buf itself would hand the caller
	// a slice that can silently change or become invalid after this call
	// returns.
	result := make([]byte, len(buf))
	copy(result, buf)

	if _, ferr := i.free.Call(context.WithoutCancel(ctx), uint64(outPtr), uint64(outLen)); ferr != nil {
		i.dead = true
		return nil, fmt.Errorf("free result %d bytes at %d: %w", outLen, outPtr, ferr)
	}

	return result, nil
}

// Dead reports whether the Instance's underlying module has been closed,
// either explicitly via Close or implicitly by wazero closing it in
// response to a call failure (most notably ctx cancellation during Invoke;
// see NewRuntime's WithCloseOnContextDone). Once Dead reports true the
// Instance must not be reused.
func (i *Instance) Dead() bool {
	return i.dead
}

// Close closes the Instance's underlying module, releasing the resources
// wazero holds for it. Close is idempotent: closing an Instance that is
// already Dead — whether from a prior Close call or from wazero closing the
// module on its own — is a no-op that returns nil.
func (i *Instance) Close(ctx context.Context) error {
	if i.dead {
		return nil
	}
	i.dead = true
	if err := i.mod.Close(ctx); err != nil {
		return fmt.Errorf("close plugin module: %w", err)
	}
	return nil
}
