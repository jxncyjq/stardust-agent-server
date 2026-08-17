# testdata: plugin.wasm test fixture

`plugin.wasm` is a prebuilt WASM guest used by `internal/plugin/host`'s tests.
It is committed so CI does not need a Rust toolchain. Source is in
`guest-rust/`.

## Build command

```
cd guest-rust
cargo build --release --target wasm32-wasip1
cp target/wasm32-wasip1/release/guest_rust.wasm ../plugin.wasm
```

Requires `rustup target add wasm32-wasip1`.

## Exports

The fixture is a WASI **reactor**: it exports `_initialize` and no `_start`.
`_initialize` sets a flag that op 91 reports, so a host that instantiates
without `WithStartFunctions("_initialize")` is detected by the tests rather
than silently getting an uninitialized guest.

Exported functions are exactly `_initialize`, `plugin_alloc`, `plugin_free`,
`plugin_invoke`, plus the linear memory export `memory`
(`NewInstance` requires a memory). Imports are WASI only
(`wasi_snapshot_preview1.*`) — `internal/plugin/host`'s `NewRuntime` (Task 2)
registers no host functions, so this guest must instantiate against WASI
alone; host function calls are exercised starting Task 3. This export/import
set is pinned by `TestFixtureExportsAndImportsArePinned`, so a rebuild that
changes it fails the suite instead of quietly moving the ground the other
tests stand on.

## Op table (plugin_invoke's `op` argument)

| op | name                | defined in         | behavior |
|----|---------------------|---------------------|----------|
| 0  | `abi.OpManifest`    | `internal/plugin/abi` | input ignored; returns `{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}` |
| 1  | `abi.OpCallTool`    | `internal/plugin/abi` | input is a JSON tool call `{"tool":...,"args":...}`; returns `{"result": <args>}` |
| 90 | test-only echo      | `internal/plugin/host` test files | parses `{"name":string,"n":int}`, returns `{"greeting":"hello <name>","doubled":<n*2>}` |
| 91 | test-only probe     | `internal/plugin/host` test files | input ignored; returns `{"initialized":bool,"alloc_calls":int,"free_calls":int,"slow_free_calls":int,"in_ptr":int,"in_len":int}` (see below) |
| 92 | test-only arm slow free | `internal/plugin/host` test files | input `{"spin_iters":int}`; arms the **next** `plugin_free` call to spin that many iterations, then returns `{"armed":true,"spin_iters":<n>}`. Missing `spin_iters` returns `{"error":"missing spin_iters"}` |
| 93 | test-only bogus result | `internal/plugin/host` test files | allocates nothing and returns a packed result whose pointer (`0xFFFF0000`) is far outside linear memory, so the host's result read fails |
| 97 | test-only unknown op | `internal/plugin/host` test files | reserved and deliberately **not** implemented: it falls through to the unsupported-op branch, so the host's unknown-op test does not depend on a low literal a future `abi` op could claim |
| 98 | test-only mem bomb  | `internal/plugin/host` test files | allocates 1MiB chunks in a loop until trapped by the host's memory page cap; never returns normally under a cap |
| 99 | test-only busy loop | `internal/plugin/host` test files | pure-compute infinite loop (`black_box`-guarded so LLVM cannot delete it); only stops via host-side context cancellation |
| *  | (any other value)   | —                   | never traps; returns `{"error":"unsupported op"}` |

Ops 90-99 are test-only: their numeric constants are declared unexported in
`internal/plugin/host`'s `_test.go` files, not in the production `abi`
package.

### Op 91's report

- `initialized` — `_initialize` ran.
- `alloc_calls` / `free_calls` — every `plugin_alloc` / `plugin_free` entry so
  far, **including the guest's own allocation of each response body** and
  including calls the guest refuses (`plugin_free` ignores a non-positive
  pointer). Both counters are snapshotted into the body *before* this op's own
  response is allocated, so op 91 never counts itself.
- `slow_free_calls` — `plugin_free` calls that actually spun (see op 92).
- `in_ptr` / `in_len` — the request pointer and length this very
  `plugin_invoke` call was handed, so the host can verify that a nil input
  really becomes `(0, 0)`.

### Why op 92 spins instead of sleeping

wazero's default `ModuleConfig` installs a fake nanosleep (returns
immediately) and a fake nanotime (advances 1ms per reading), so a guest can
neither sleep nor measure real time. The host therefore calibrates an
iteration count against a first, uncancelled call and passes it in
`spin_iters`. `TestInvokeFreesInputAfterContextCancellation` uses this to
widen the window between a successful `plugin_invoke` and `Invoke`'s deferred
input free deterministically, instead of racing context cancellation against
many fast calls.
