// Host-call test fixture guest for internal/plugin/host.
//
// Unlike ../guest-rust (which imports nothing but WASI), this fixture imports
// EVERY host function of the `legion` module, so the host's capability
// whitelist can be exercised from both sides:
//
//   - a grant that is missing a capability must make this module fail to
//     instantiate (link-time absence, not a runtime DENIED), and
//   - with the capability granted, each op below calls the corresponding host
//     function so the host-side argument checks (allowed_hosts /
//     allowed_paths) can be observed.
//
// Every op that has a host return value returns the host's packed (ptr, len)
// result VERBATIM as its own plugin_invoke result. The region was allocated by
// this guest's plugin_alloc (the host writes results back through it), so the
// host's Instance.Invoke reads and then plugin_free's it exactly as it would
// its own guest-produced body.
//
// See ../README.md for the op table and the build command.

use std::alloc::{alloc, dealloc, Layout};
use std::sync::atomic::{AtomicBool, Ordering};

#[link(wasm_import_module = "legion")]
extern "C" {
    fn log(level: i32, ptr: i32, len: i32);
    fn config_get() -> i64;
    fn kv_get(kp: i32, kl: i32) -> i64;
    fn kv_put(kp: i32, kl: i32, vp: i32, vl: i32) -> i64;
    fn http_request(ptr: i32, len: i32) -> i64;
    fn read_file(ptr: i32, len: i32) -> i64;
    fn call_tool(ptr: i32, len: i32) -> i64;
}

/// Armed by op 77; makes the NEXT plugin_alloc call return 0 and then disarms
/// itself. It exists so the host's "cannot hand the result back to the guest"
/// path can be observed: that path must fail loudly, not return an empty body
/// that the guest would read as a successful call with no data.
static FAIL_NEXT_ALLOC: AtomicBool = AtomicBool::new(false);

/// WASI reactor initialization entry point (this fixture keeps no state that
/// needs initializing; the export exists because the host instantiates with
/// WithStartFunctions("_initialize")).
#[no_mangle]
pub extern "C" fn _initialize() {}

#[no_mangle]
pub extern "C" fn plugin_alloc(size: i32) -> i32 {
    if FAIL_NEXT_ALLOC.swap(false, Ordering::SeqCst) {
        return 0;
    }
    if size <= 0 {
        return 0;
    }
    unsafe { alloc(Layout::from_size_align(size as usize, 1).unwrap()) as i32 }
}

#[no_mangle]
pub extern "C" fn plugin_free(ptr: i32, size: i32) {
    if ptr <= 0 || size <= 0 {
        return;
    }
    unsafe { dealloc(ptr as *mut u8, Layout::from_size_align(size as usize, 1).unwrap()) }
}

fn write_out(b: &[u8]) -> i64 {
    let ptr = plugin_alloc(b.len() as i32);
    if ptr == 0 {
        return 0;
    }
    unsafe { std::ptr::copy_nonoverlapping(b.as_ptr(), ptr as *mut u8, b.len()) };
    ((ptr as i64) << 32) | (b.len() as i64)
}

#[no_mangle]
pub extern "C" fn plugin_invoke(op: i32, ptr: i32, size: i32) -> i64 {
    match op {
        // test-only: call log(level=1 /* info */) with the request body as the
        // message. log has no host return value, so report the ABI's
        // contract-legal "no return body" (PackResult(0, 0)).
        70 => {
            unsafe { log(1, ptr, size) };
            0
        }
        // test-only: call config_get() and return its result verbatim.
        71 => unsafe { config_get() },
        // test-only: call kv_get() with the whole request body as the key.
        72 => unsafe { kv_get(ptr, size) },
        // test-only: call kv_put(). The request body is framed "<key>\n<value>"
        // (kv_put takes key and value as two separate pointer pairs, so the
        // split is this fixture's business, not the ABI's).
        73 => {
            let input = if size > 0 {
                unsafe { std::slice::from_raw_parts(ptr as *const u8, size as usize) }
            } else {
                &[][..]
            };
            match input.iter().position(|b| *b == b'\n') {
                Some(i) => unsafe {
                    kv_put(
                        ptr,
                        i as i32,
                        ptr + i as i32 + 1,
                        size - i as i32 - 1,
                    )
                },
                None => write_out(br#"{"error":"missing newline separator"}"#),
            }
        }
        // test-only: call http_request() with the request body verbatim.
        74 => unsafe { http_request(ptr, size) },
        // test-only: call read_file() with the request body verbatim.
        75 => unsafe { read_file(ptr, size) },
        // test-only: call call_tool() with the request body verbatim.
        76 => unsafe { call_tool(ptr, size) },
        // test-only: arm the next plugin_alloc to fail. Returns
        // PackResult(0, 0) directly so this op's own response does not consume
        // the arming (write_out would).
        77 => {
            FAIL_NEXT_ALLOC.store(true, Ordering::SeqCst);
            0
        }
        // Any other op: never trap, return a small readable JSON error body.
        _ => write_out(br#"{"error":"unsupported op"}"#),
    }
}
