// Package host wraps wazero to compile, instantiate, and invoke a Legion
// WASM plugin guest module against the ABI defined in
// internal/plugin/abi: a runtime constructor, a compile step separated from
// instantiation (so many Instances can share one CompiledModule), and an
// Instance type exposing the three ABI exports as a single Invoke call.
package host

import (
	"context"
	"errors"
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
// (abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke) and its linear memory.
// An Instance is not safe for concurrent use — callers must serialize Invoke
// calls against a given Instance.
type Instance struct {
	mod    api.Module
	mem    api.Memory
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
// It resolves the three ABI exports plus the guest's linear memory and
// fails, naming what is absent, if any of them is missing. The exports are
// checked before the memory, so a guest missing both is reported by export
// name — the more specific fault.
func NewInstance(ctx context.Context, rt wazero.Runtime, compiled wazero.CompiledModule) (inst *Instance, err error) {
	mod, err := rt.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithStartFunctions("_initialize").WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate plugin module: %w", err)
	}
	// A module that fails validation below is never handed to a caller, so
	// nothing would ever close it: release it here instead of leaking it
	// until the whole Runtime is closed. A failing close is joined onto the
	// validation error rather than dropped.
	defer func() {
		if err == nil {
			return
		}
		if cerr := mod.Close(ctx); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close rejected plugin module: %w", cerr))
		}
	}()

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

	// Memory is as mandatory as the three exports: Invoke writes request
	// bodies into it and reads response bodies out of it. It must be checked
	// here rather than relied upon at call time, because wazero's
	// api.Module.Memory documents "nil if there are none" but implements it
	// as a bare field read — for a module with no memory that is a nil
	// *wasm.MemoryInstance boxed inside a non-nil api.Memory interface, so a
	// plain nil check misses it and the first Read/Write panics the host
	// process instead of failing this call. Gate on the module's exported
	// memory definitions instead: a wasm module has at most one memory, and a
	// wasm32-wasip1 guest must export it (the WASI application ABI requires a
	// memory export named "memory"), so a non-empty definition map means
	// Memory() is a usable memory.
	mem := mod.Memory()
	if mem == nil || len(mod.ExportedMemoryDefinitions()) == 0 {
		return nil, fmt.Errorf("instantiate plugin module: guest exports no linear memory")
	}

	return &Instance{mod: mod, mem: mem, alloc: alloc, free: free, invoke: invoke}, nil
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
		// Registered before the write below, not after: from this point on the
		// guest holds a reservation that leaks unless it is freed, and the
		// write is itself a step that can fail.
		//
		// Free the input with a cancellation-scrubbed context: if ctx is
		// already cancelled by the time this defer runs, calling free with
		// ctx unchanged would itself fail immediately and leak the guest
		// memory plugin_alloc just reserved.
		defer func() {
			if _, ferr := i.free.Call(context.WithoutCancel(ctx), ptr, uint64(len(in))); ferr != nil {
				// A free call failing means wazero closed the module (a
				// trap or context-death aborts the whole module, not just
				// this call); the Instance must not be reused even though
				// the earlier invoke succeeded. The failure is joined onto
				// any primary error rather than dropped when one exists:
				// this is the diagnostic for a guest whose allocator died.
				i.dead = true
				err = errors.Join(err, fmt.Errorf("free input %d bytes at %d: %w", len(in), ptr, ferr))
			}
		}()
		if !i.mem.Write(uint32(ptr), in) {
			return nil, fmt.Errorf("write %d bytes at %d: out of range", len(in), ptr)
		}
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

	buf, ok := i.mem.Read(outPtr, outLen)
	if !ok {
		// The guest handed back a region outside its own memory: its result
		// allocation (whatever it really is) still has to be released, and a
		// guest whose result pointers are out of range is not fit for reuse.
		rerr := fmt.Errorf("read result at %d len %d: out of range", outPtr, outLen)
		if _, ferr := i.free.Call(context.WithoutCancel(ctx), uint64(outPtr), uint64(outLen)); ferr != nil {
			rerr = errors.Join(rerr, fmt.Errorf("free result %d bytes at %d: %w", outLen, outPtr, ferr))
		}
		i.dead = true
		return nil, rerr
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

// Dead reports whether this Instance must no longer be used, which is true
// in two independent cases:
//
//   - a call on it failed (a guest trap, a failed alloc/free, ctx
//     cancellation during Invoke) or Close was called, both of which the
//     Instance records itself;
//   - wazero closed the underlying module without any call reporting an
//     error. wazero documents this for a runtime built with
//     WithCloseOnContextDone (see NewRuntime): a context completion closes
//     the module, and a caller must check for closure "even if you didn't
//     formerly receive a sys.ExitError" — so a ctx that goes Done just as an
//     Invoke returns successfully leaves a closed module behind.
//
// The second case is why Dead consults the module itself and not only the
// Instance's own flag: a pool handing out instances on the strength of
// Dead() == false must never hand out a closed one.
func (i *Instance) Dead() bool {
	return i.dead || i.mod.IsClosed()
}

// Close closes the Instance's underlying module, releasing the resources
// wazero holds for it (its filesystem context, its memory buffer and its
// compiled code closer). Close is idempotent: wazero's own
// CloseWithExitCode is a no-op on an already-closed module, so calling Close
// twice — or calling it on an Instance that is already Dead — returns nil
// without doing damage.
//
// Close always calls through to wazero even when the Instance is already
// Dead, because the paths that kill an Instance do not release its
// resources: a guest trap leaves the module fully open, and a module closed
// by ctx completion has its resource release deferred until a real close.
// Those are exactly the instances a pool discards, so skipping the call
// would leak wazero resources for the common failure case.
func (i *Instance) Close(ctx context.Context) error {
	i.dead = true
	if err := i.mod.Close(ctx); err != nil {
		return fmt.Errorf("close plugin module: %w", err)
	}
	return nil
}
