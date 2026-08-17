# testdata: WASM test fixtures

Two prebuilt WASM guests are used by `internal/plugin/host`'s tests. Both are
committed so CI does not need a Rust toolchain.

| fixture | source | imports from the `legion` host module |
|---|---|---|
| `plugin.wasm` | `guest-rust/` | none (WASI only) |
| `hostcall.wasm` | `guest-hostcall-rust/` | all seven host functions |

They are separate binaries on purpose: `plugin.wasm` must instantiate against a
runtime with no host module at all, while `hostcall.wasm` must fail to
instantiate unless every capability is granted. One fixture cannot do both.

## Build commands

```
cd guest-rust
cargo build --release --target wasm32-wasip1
cp target/wasm32-wasip1/release/guest_rust.wasm ../plugin.wasm

cd ../guest-hostcall-rust
cargo build --release --target wasm32-wasip1
cp target/wasm32-wasip1/release/guest_hostcall_rust.wasm ../hostcall.wasm
```

Requires `rustup target add wasm32-wasip1`. `guest-hostcall-rust` has no
dependencies, so `--offline` works for it.

# plugin.wasm

## Exports

The fixture is a WASI **reactor**: it exports `_initialize` and no `_start`.
`_initialize` sets a flag that op 91 reports, so a host that instantiates
without `WithStartFunctions("_initialize")` is detected by the tests rather
than silently getting an uninitialized guest.

Exported functions are exactly `_initialize`, `plugin_alloc`, `plugin_free`,
`plugin_invoke`, plus the linear memory export `memory`
(`NewInstance` requires a memory). Imports are WASI only
(`wasi_snapshot_preview1.*`): this fixture must instantiate against a runtime
that registers no host functions at all, which is what the lifecycle tests
use it for. Host function calls are exercised by `hostcall.wasm` instead. This
export/import set is pinned by `TestFixtureExportsAndImportsArePinned`, so a
rebuild that changes it fails the suite instead of quietly moving the ground
the other tests stand on.

## Op table (plugin_invoke's `op` argument)

| op | name                | defined in         | behavior |
|----|---------------------|---------------------|----------|
| 0  | `abi.OpManifest`    | `internal/plugin/abi` | input ignored; returns `{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}` |
| 1  | `abi.OpCallTool`    | `internal/plugin/abi` | input is a JSON tool call `{"call_id":...,"tool":...,"arguments":{...}}`; returns a JSON `domain.ToolResult` (see below) |
| 90 | test-only echo      | `internal/plugin/host` test files | parses `{"name":string,"n":int}`, returns `{"greeting":"hello <name>","doubled":<n*2>}` |
| 91 | test-only probe     | `internal/plugin/host` test files | input ignored; returns `{"initialized":bool,"alloc_calls":int,"free_calls":int,"slow_free_calls":int,"in_ptr":int,"in_len":int}` (see below) |
| 92 | test-only arm slow free | `internal/plugin/host` test files | input `{"spin_iters":int}`; arms the **next** `plugin_free` call to spin that many iterations, then returns `{"armed":true,"spin_iters":<n>}`. Missing `spin_iters` returns `{"error":"missing spin_iters"}` |
| 93 | test-only bogus result | `internal/plugin/host` test files | allocates nothing and returns a packed result whose pointer (`0xFFFF0000`) is far outside linear memory, so the host's result read fails |
| 94 | test-only empty result | `internal/plugin/host` test files | input ignored; allocates nothing and returns `PackResult(0, 0)` directly (raw `0i64`, no `write_out`), exercising `Invoke`'s `outLen == 0 -> (nil, nil)` branch |
| 97 | test-only unknown op | `internal/plugin/host` test files | reserved and deliberately **not** implemented: it falls through to the unsupported-op branch, so the host's unknown-op test does not depend on a low literal a future `abi` op could claim |
| 98 | test-only mem bomb  | `internal/plugin/host` test files | allocates 1MiB chunks in a loop until trapped by the host's memory page cap; never returns normally under a cap |
| 99 | test-only busy loop | `internal/plugin/host` test files | pure-compute infinite loop (`black_box`-guarded so LLVM cannot delete it); only stops via host-side context cancellation |
| *  | (any other value)   | —                   | never traps; returns `{"error":"unsupported op"}` |

Ops 90-99 are test-only: their numeric constants are declared unexported in
`internal/plugin/host`'s `_test.go` files, not in the production `abi`
package.

### Op 1's tool result

Op 1 is the guest side of a contributed tool call (`internal/plugin/host`'s
`contribute.go`). The host sends `guestToolCall`
(`{"call_id":…,"tool":…,"arguments":{…}}`) and expects a JSON
`domain.ToolResult` back. The fixture answers:

| arguments | answer |
|---|---|
| (anything else) | `{"call_id":"guest-call-id","success":true,"output":"<tool>:<arguments as compact JSON>"}` |
| contains `fail` (string) | `{"call_id":"guest-call-id","success":false,"error":"<that string>"}` — the tool's own failure, which the host must pass on as a result rather than as a Go error |
| contains `malformed` | `{"unexpected":"shape"}` — valid JSON that is not a `ToolResult`, so the host's strict decode must report it instead of handing on an empty result |
| (`tool` missing/empty) | `{"call_id":"guest-call-id","success":false,"error":"missing tool"}` |
| (input is not JSON) | `{"call_id":"guest-call-id","success":false,"error":"bad json"}` |

`call_id` is **always** answered with the literal `guest-call-id`, never with
the id the host sent. That is deliberate: the host owns the correlation id and
overwrites whatever the guest returns, so a fixture that echoed the right one
could not tell a host that overwrites it from one that trusts the guest.

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

# hostcall.wasm

## Exports and imports

Same ABI exports as `plugin.wasm` (`_initialize`, `plugin_alloc`,
`plugin_free`, `plugin_invoke`, plus the `memory` export). It additionally
imports **every** host function of the `legion` module — `log`, `config_get`,
`kv_get`, `kv_put`, `http_request`, `read_file`, `call_tool` — so that:

- a grant missing any capability makes this module fail to **instantiate**
  (the capability whitelist is a link-time absence, not a runtime `DENIED`),
  and
- with the capability granted, each op below calls the matching host function
  so the host-side argument checks (`allowed_hosts` / `allowed_paths`) can be
  observed from the guest side.

The export/import set is pinned by `TestHostcallFixtureContractIsPinned`.

## Op table (all test-only, constants live in `hostfunc_test.go`)

Every op that has a host return value returns the host's packed `(ptr, len)`
result **verbatim** as its own `plugin_invoke` result. The host wrote that
region through this guest's own `plugin_alloc`, so `Instance.Invoke` reads and
`plugin_free`s it exactly as it would a guest-produced body.

| op | name | behavior |
|----|------|----------|
| 70 | call log | calls `log(1 /* info */, body)`; returns `PackResult(0, 0)` (log has no host return value) |
| 71 | call config_get | calls `config_get()` |
| 72 | call kv_get | calls `kv_get(body)`: the whole request body is the key |
| 73 | call kv_put | request body is framed `"<key>\n<value>"`; calls `kv_put(key, value)`. No newline returns `{"error":"missing newline separator"}` |
| 74 | call http_request | calls `http_request(body)` |
| 75 | call read_file | calls `read_file(body)` |
| 76 | call call_tool | calls `call_tool(body)` |
| 77 | arm alloc failure | makes the **next** `plugin_alloc` return 0 and disarms itself, so the host's "cannot hand the result back" path can be observed. Returns `PackResult(0, 0)` directly, because allocating its own response would consume the arming |
| *  | (any other value) | never traps; returns `{"error":"unsupported op"}` |

Op 77 must be invoked with a **nil** request body: a non-empty body makes
`Instance.Invoke` allocate for the input first, which would consume the
arming.
