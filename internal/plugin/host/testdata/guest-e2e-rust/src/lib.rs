// End-to-end acceptance fixture guest for internal/plugin/host.
//
// It is the only fixture that is BOTH activatable and able to call back into
// the host, which is what the acceptance test needs:
//
//   - ../guest-rust (plugin.wasm) self-describes and can be activated, but
//     imports nothing from the `legion` host module, so nothing it does can
//     observe the context the host handed it;
//   - ../guest-hostcall-rust (hostcall.wasm) calls every host function, but its
//     op 0 is deliberately NOT a manifest (an activation test asserts the
//     cross-check refuses it), so it can never be activated.
//
// This one answers abi::OP_MANIFEST with a real self-description and answers
// abi::OP_CALL_TOOL — the op a contributed tool's handler invokes — by calling
// the `call_tool` host function. That makes the whole chain real: the model
// calls the contributed tool, the host's handler enters this guest, this guest
// calls back out to the host, and the host runs another registered tool. What
// the innermost tool sees is what the dispatch context carried all the way
// through wazero.
//
// The inner call names a CONSTANT tool, never the tool this plugin itself
// contributes. That is a safety property, not a stylistic choice: forwarding
// the host's own request back would make this guest call itself through the
// registry, and the only thing stopping that chain would be the call_tool depth
// cap — the feature under test. A fixture whose only brake is the thing being
// tested has no brake.
//
// See ../README.md for the op table and the build command.

use std::alloc::{alloc, dealloc, Layout};
use std::sync::atomic::{AtomicBool, Ordering};

#[link(wasm_import_module = "legion")]
extern "C" {
    fn call_tool(ptr: i32, len: i32) -> i64;
}

/// The self-description this guest returns for abi::OP_MANIFEST. `provides`
/// must list the tool the host's Spec claims, or activation's cross-check
/// refuses the plugin.
const MANIFEST: &[u8] =
    br#"{"name":"legion-e2e-plugin","version":"0.1.0","provides":["e2e_proxy_tool"]}"#;

/// The request this guest hands to the `call_tool` host function when it is
/// asked to run its tool. The tool name is fixed and is NOT this plugin's own
/// contributed tool — see the module comment.
const INNER_CALL: &[u8] =
    br#"{"call_id":"guest-inner-call","tool":"e2e_inner_tool","arguments":{"probe":"from-guest"}}"#;

/// Set by `_initialize`, so a host that instantiates without
/// WithStartFunctions("_initialize") is detected instead of quietly running an
/// uninitialized guest: op 1 refuses to call out before initialization.
static INITIALIZED: AtomicBool = AtomicBool::new(false);

#[no_mangle]
pub extern "C" fn _initialize() {
    INITIALIZED.store(true, Ordering::SeqCst);
}

#[no_mangle]
pub extern "C" fn plugin_alloc(size: i32) -> i32 {
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
pub extern "C" fn plugin_invoke(op: i32, _ptr: i32, _size: i32) -> i64 {
    match op {
        // abi.OpManifest: the self-description activation cross-checks.
        0 => write_out(MANIFEST),
        // abi.OpCallTool: the op a contributed tool's handler invokes. Ignore
        // the host's request body (this fixture has no JSON parser) and call
        // the host straight back with the constant request above, returning the
        // host's packed (ptr, len) result verbatim — the host wrote that region
        // through this guest's own plugin_alloc, so Instance::Invoke reads and
        // plugin_free's it exactly as it would a body this guest produced.
        //
        // On success the host's answer is a JSON domain.ToolResult, which is
        // exactly what the contributed tool's handler decodes, so the inner
        // tool's result travels back to the model unchanged.
        1 => {
            if !INITIALIZED.load(Ordering::SeqCst) {
                return write_out(br#"{"success":false,"error":"guest was never initialized"}"#);
            }
            unsafe { call_tool(INNER_CALL.as_ptr() as i32, INNER_CALL.len() as i32) }
        }
        // Any other op: never trap, return a small readable JSON error body.
        _ => write_out(br#"{"error":"unsupported op"}"#),
    }
}
