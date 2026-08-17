// Test fixture guest for internal/plugin/host.
//
// Implements the plugin ABI (plugin_alloc/plugin_free/plugin_invoke, see
// internal/plugin/abi) with a fixed op table, plus a WASI reactor
// `_initialize` export. Ops 0 and 1 are the real ABI ops (abi.OpManifest,
// abi.OpCallTool); ops 90-99 are test-only and exist solely to exercise the
// host wrapper (echo round trip, host-observable instrumentation, a
// deliberately slow plugin_free, an out-of-range result pointer, the
// memory-cap trap, the context-cancellation trap). See ../README.md for the
// full op table and the build command.
//
// Deliberately imports nothing from a "legion" host module: Task 2's host
// wrapper does not register any host functions (that is Task 3), so this
// fixture must instantiate against WASI alone.

use std::alloc::{alloc, dealloc, Layout};
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};

/// Set by `_initialize`. Reported by op 91 so the host can prove it
/// instantiated this module with WithStartFunctions("_initialize"): a reactor
/// module exports no `_start`, so the default start-function configuration
/// leaves this false.
static INITIALIZED: AtomicBool = AtomicBool::new(false);

/// Counts every plugin_alloc entry, including the guest's own allocations for
/// response bodies (see `write_out`). Reported by op 91 so the host can prove
/// whether Invoke called plugin_alloc for a given input.
static ALLOC_CALLS: AtomicU32 = AtomicU32::new(0);

/// Counts every plugin_free entry, including calls the guest refuses (see
/// plugin_free's pointer check). Reported by op 91 so the host can prove it
/// released a region it asked the guest to allocate.
static FREE_CALLS: AtomicU32 = AtomicU32::new(0);

/// Armed by op 92; makes the next plugin_free call spin (see
/// SLOW_FREE_ITERS) and then disarms itself.
static SLOW_FREE_ARMED: AtomicBool = AtomicBool::new(false);

/// Iteration count for the armed slow plugin_free. Wall-clock spinning, not
/// sleeping: wazero's default ModuleConfig installs a fake nanosleep that
/// returns immediately and a fake nanotime that advances 1ms per reading, so
/// a guest cannot sleep or measure real time. The host calibrates the count.
static SLOW_FREE_ITERS: AtomicU64 = AtomicU64::new(0);

/// Counts plugin_free calls that actually spun. Reported by op 91 so a host
/// test that depends on the slow free happening can prove it happened
/// instead of passing vacuously.
static SLOW_FREE_CALLS: AtomicU32 = AtomicU32::new(0);

/// WASI reactor initialization entry point. Instantiating this module with
/// wazero's default start functions (`["_start"]`) does not call it.
#[no_mangle]
pub extern "C" fn _initialize() {
    INITIALIZED.store(true, Ordering::SeqCst);
}

#[no_mangle]
pub extern "C" fn plugin_alloc(size: i32) -> i32 {
    ALLOC_CALLS.fetch_add(1, Ordering::SeqCst);
    if size <= 0 {
        return 0;
    }
    unsafe { alloc(Layout::from_size_align(size as usize, 1).unwrap()) as i32 }
}

#[no_mangle]
pub extern "C" fn plugin_free(ptr: i32, size: i32) {
    FREE_CALLS.fetch_add(1, Ordering::SeqCst);
    if SLOW_FREE_ARMED.swap(false, Ordering::SeqCst) {
        let iters = SLOW_FREE_ITERS.load(Ordering::SeqCst);
        let mut acc: u64 = 0;
        let mut i: u64 = 0;
        while i < iters {
            acc = std::hint::black_box(acc.wrapping_add(i));
            i += 1;
        }
        std::hint::black_box(acc);
        SLOW_FREE_CALLS.fetch_add(1, Ordering::SeqCst);
    }
    // A non-positive pointer is never something plugin_alloc returned (op 93
    // hands the host an out-of-range one on purpose), so refuse it instead of
    // handing garbage to dealloc.
    if ptr <= 0 || size <= 0 {
        return;
    }
    unsafe { dealloc(ptr as *mut u8, Layout::from_size_align(size as usize, 1).unwrap()) }
}

fn write_out(b: &[u8]) -> i64 {
    let ptr = plugin_alloc(b.len() as i32);
    unsafe { std::ptr::copy_nonoverlapping(b.as_ptr(), ptr as *mut u8, b.len()) };
    ((ptr as i64) << 32) | (b.len() as i64)
}

#[no_mangle]
pub extern "C" fn plugin_invoke(op: i32, ptr: i32, size: i32) -> i64 {
    let input: Vec<u8> = if size > 0 {
        unsafe { std::slice::from_raw_parts(ptr as *const u8, size as usize).to_vec() }
    } else {
        Vec::new()
    };

    match op {
        // abi.OpManifest: input ignored, return the plugin's self-description.
        0 => write_out(
            br#"{"name":"legion-test-plugin","version":"0.1.0","provides":["echo_tool"]}"#,
        ),
        // abi.OpCallTool: input is a JSON tool call {"tool":...,"args":...};
        // echo args back under "result".
        1 => {
            let v: serde_json::Value = match serde_json::from_slice(&input) {
                Ok(v) => v,
                Err(_) => return write_out(br#"{"error":"bad json"}"#),
            };
            let args = v.get("args").cloned().unwrap_or(serde_json::Value::Null);
            let out = serde_json::json!({ "result": args });
            write_out(out.to_string().as_bytes())
        }
        // test-only: echo/JSON round trip.
        90 => {
            let v: serde_json::Value = match serde_json::from_slice(&input) {
                Ok(v) => v,
                Err(_) => return write_out(br#"{"error":"bad json"}"#),
            };
            let name = v.get("name").and_then(|x| x.as_str()).unwrap_or("");
            let n = v.get("n").and_then(|x| x.as_i64()).unwrap_or(0);
            let out = serde_json::json!({"greeting": format!("hello {}", name), "doubled": n * 2});
            write_out(out.to_string().as_bytes())
        }
        // test-only: report this instance's host-observable instrumentation,
        // plus the (ptr, len) pair this very call was handed, so the host can
        // check what it passed to plugin_invoke. The counters are snapshotted
        // into the body BEFORE write_out runs, so neither the allocation nor
        // the free for this very response is counted.
        91 => {
            let body = format!(
                concat!(
                    r#"{{"initialized":{},"alloc_calls":{},"free_calls":{},"#,
                    r#""slow_free_calls":{},"in_ptr":{},"in_len":{}}}"#
                ),
                INITIALIZED.load(Ordering::SeqCst),
                ALLOC_CALLS.load(Ordering::SeqCst),
                FREE_CALLS.load(Ordering::SeqCst),
                SLOW_FREE_CALLS.load(Ordering::SeqCst),
                ptr,
                size
            );
            write_out(body.as_bytes())
        }
        // test-only: arm the next plugin_free call to spin for
        // input.spin_iters iterations, so the host can widen the window
        // between a successful plugin_invoke and its deferred input free.
        92 => {
            let v: serde_json::Value = match serde_json::from_slice(&input) {
                Ok(v) => v,
                Err(_) => return write_out(br#"{"error":"bad json"}"#),
            };
            let iters = match v.get("spin_iters").and_then(|x| x.as_u64()) {
                Some(n) => n,
                None => return write_out(br#"{"error":"missing spin_iters"}"#),
            };
            SLOW_FREE_ITERS.store(iters, Ordering::SeqCst);
            SLOW_FREE_ARMED.store(true, Ordering::SeqCst);
            write_out(format!(r#"{{"armed":true,"spin_iters":{}}}"#, iters).as_bytes())
        }
        // test-only: return a result pointer that is far outside linear
        // memory, so the host's result read fails. Nothing is allocated, and
        // plugin_free refuses the pointer (see above), so the host's cleanup
        // of this bogus region is a no-op rather than a corrupt dealloc.
        93 => 0xFFFF_0000_0000_0010u64 as i64,
        // test-only: empty result. Returns PackResult(0, 0) directly (no
        // allocation, no write_out) so the host exercises Invoke's
        // outLen == 0 -> (nil, nil) branch, which the ABI declares a
        // contract-legal "no return body" outcome rather than an error.
        94 => 0i64,
        // test-only: memory bomb. Keeps allocating until the host's page
        // limit traps the guest; must never return normally under a cap.
        98 => {
            let mut held: Vec<Vec<u8>> = Vec::new();
            for _ in 0..4096 {
                held.push(std::hint::black_box(vec![7u8; 1 << 20])); // 1MiB each
            }
            write_out(format!("{{\"allocated_mib\":{}}}", held.len()).as_bytes())
        }
        // test-only: pure-compute infinite loop, interruptible only via
        // context cancellation on the host side. black_box prevents LLVM
        // from proving the loop has no observable effect and deleting it.
        99 => {
            let mut x: u64 = 0;
            loop {
                x = std::hint::black_box(x.wrapping_add(1));
            }
        }
        // Any other op: never trap, return a small readable JSON error body.
        // Op 97 is reserved for the host's unknown-op test and is
        // deliberately not implemented here.
        _ => write_out(br#"{"error":"unsupported op"}"#),
    }
}
