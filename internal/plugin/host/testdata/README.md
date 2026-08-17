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

## Op table (plugin_invoke's `op` argument)

| op | name                | defined in         | behavior |
|----|---------------------|---------------------|----------|
| 0  | `abi.OpManifest`    | `internal/plugin/abi` | input ignored; returns `{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}` |
| 1  | `abi.OpCallTool`    | `internal/plugin/abi` | input is a JSON tool call `{"tool":...,"args":...}`; returns `{"result": <args>}` |
| 90 | test-only echo      | `internal/plugin/host` test files | parses `{"name":string,"n":int}`, returns `{"greeting":"hello <name>","doubled":<n*2>}` |
| 98 | test-only mem bomb  | `internal/plugin/host` test files | allocates 1MiB chunks in a loop until trapped by the host's memory page cap; never returns normally under a cap |
| 99 | test-only busy loop | `internal/plugin/host` test files | pure-compute infinite loop (`black_box`-guarded so LLVM cannot delete it); only stops via host-side context cancellation |
| *  | (any other value)   | —                   | never traps; returns `{"error":"unsupported op"}` |

Ops 90/98/99 are test-only: their numeric constants are declared unexported
in `internal/plugin/host`'s `_test.go` files, not in the production `abi`
package.

The fixture imports no host module functions — `internal/plugin/host`'s
`NewRuntime` (Task 2) registers no host functions, so this guest must
instantiate against WASI alone. Host function calls are exercised starting
Task 3.
