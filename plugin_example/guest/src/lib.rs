// legion-hello: the smallest plugin that closes the whole loop.
//
// It answers both ABI ops for real:
//
//   op 0 (abi.OpManifest)  self-describes, so activation's cross-check can
//                          confirm the guest really implements the tool the
//                          deployment claims it does;
//   op 1 (abi.OpCallTool)  reads the caller's argument, calls back into the
//                          host through the `log` capability, and answers with
//                          a domain.ToolResult.
//
// Deliberately dependency-free so it builds offline (`cargo build --offline`).
// That is why the JSON handling below is hand-rolled: a real plugin should use
// serde_json and decode into a struct. Every place that simplification matters
// is marked EXAMPLE-ONLY.
//
// Build: see ../../scripts/build.sh (cargo build --release --target wasm32-wasip1).

use std::alloc::{alloc, dealloc, Layout};

// The host module every capability's function is imported from.
//
// `log` is the ONLY import here, and that is a hard requirement rather than a
// stylistic choice: the host registers a capability's host functions only when
// that capability is granted, so importing a function the deployment did not
// grant makes this module fail to INSTANTIATE. Never import what the manifest
// does not ask for.
#[link(wasm_import_module = "legion")]
extern "C" {
    fn log(level: i32, ptr: i32, len: i32);
}

const LOG_INFO: i32 = 1;

/// The self-description op 0 returns.
///
/// `provides` must cover every tool the deployment claims this plugin
/// contributes, or activation refuses to mount it. Keep it in sync with
/// `package/plugin.json`'s `tools[].name`.
const MANIFEST: &[u8] = br#"{"name":"legion-hello","version":"0.1.0","provides":["hello_echo"]}"#;

/// plugin_alloc is how the host reserves guest memory: for the request body it
/// is about to write, and for the response body a host function hands back.
/// Returning 0 means "cannot allocate", which the host reports rather than
/// dereferences.
#[no_mangle]
pub extern "C" fn plugin_alloc(size: i32) -> i32 {
    if size <= 0 {
        return 0;
    }
    match Layout::from_size_align(size as usize, 1) {
        Ok(layout) => unsafe { alloc(layout) as i32 },
        Err(_) => 0,
    }
}

/// plugin_free returns what plugin_alloc reserved. The host calls it for every
/// request body it wrote, including when the call it wrote it for failed.
#[no_mangle]
pub extern "C" fn plugin_free(ptr: i32, size: i32) {
    if ptr <= 0 || size <= 0 {
        return;
    }
    if let Ok(layout) = Layout::from_size_align(size as usize, 1) {
        unsafe { dealloc(ptr as *mut u8, layout) }
    }
}

/// The host instantiates with WithStartFunctions("_initialize"): this guest is
/// a WASI *reactor* (no `_start`). Real setup work belongs here — anything done
/// at first call instead would run inside a tool's timeout budget.
#[no_mangle]
pub extern "C" fn _initialize() {}

/// write_out copies `b` into freshly allocated guest memory and packs the
/// result the way the ABI requires: the high 32 bits are the pointer, the low
/// 32 bits are the length. The host reads that region and frees it.
fn write_out(b: &[u8]) -> i64 {
    let ptr = plugin_alloc(b.len() as i32);
    if ptr == 0 {
        return 0;
    }
    unsafe { std::ptr::copy_nonoverlapping(b.as_ptr(), ptr as *mut u8, b.len()) };
    ((ptr as i64) << 32) | (b.len() as i64)
}

/// read_in borrows the request body the host wrote at (ptr, len). An empty body
/// is a legal thing for the host to send (it passes ptr=0, len=0 and never
/// calls plugin_alloc), so this returns an empty slice rather than trapping.
///
/// # Safety
/// The caller must only use the returned slice while the host still owns the
/// region — i.e. within this plugin_invoke call.
unsafe fn read_in<'a>(ptr: i32, len: i32) -> &'a [u8] {
    if ptr <= 0 || len <= 0 {
        return &[];
    }
    std::slice::from_raw_parts(ptr as *const u8, len as usize)
}

/// host_log sends one line to the host logger. It is the visible proof that the
/// capability grant reached the guest: with `log` ungranted this module would
/// not have instantiated at all.
fn host_log(message: &str) {
    let bytes = message.as_bytes();
    unsafe { log(LOG_INFO, bytes.as_ptr() as i32, bytes.len() as i32) }
}

/// EXAMPLE-ONLY: pull one string field out of a flat JSON object by scanning
/// for `"<key>":"`.
///
/// This is not a JSON parser and must not be copied into a real plugin — it
/// ignores nesting, escapes and duplicate keys. It is here so this example has
/// no dependencies and builds offline; production plugins should decode with
/// serde_json into a struct and report a decode failure as a FAILED TOOL
/// RESULT (see below), not as a trap.
///
/// It is safe enough for this one job because the host sends `arguments` as a
/// map of STRING values (see host.guestToolCall), so every value it needs to
/// read really is a quoted string.
fn extract_string_field(body: &[u8], key: &str) -> Option<String> {
    let text = std::str::from_utf8(body).ok()?;
    let needle = format!("\"{}\":\"", key);
    let start = text.find(&needle)? + needle.len();
    let rest = &text[start..];
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

/// EXAMPLE-ONLY: escape the two characters that would break a hand-built JSON
/// string. A real plugin serializes with serde_json and needs none of this.
fn escape(value: &str) -> String {
    value.replace('\\', "\\\\").replace('"', "\\\"")
}

/// ok_result and fail_result build the domain.ToolResult the host decodes.
///
/// A tool that could not do its job answers with `success:false` and an
/// `error` — it does NOT trap. A trap aborts the whole wasm module, which costs
/// the guest's state and every in-flight call in it; a failed result is just an
/// answer the model can read and react to.
fn ok_result(output: &str) -> Vec<u8> {
    format!(
        "{{\"call_id\":\"\",\"success\":true,\"output\":\"{}\",\"error\":\"\"}}",
        escape(output)
    )
    .into_bytes()
}

fn fail_result(message: &str) -> Vec<u8> {
    format!(
        "{{\"call_id\":\"\",\"success\":false,\"output\":\"\",\"error\":\"{}\"}}",
        escape(message)
    )
    .into_bytes()
}

/// handle_call is op 1's body: the guest side of one `hello_echo` invocation.
///
/// The host sends `{"call_id":…,"tool":…,"arguments":{…}}` and reads back a
/// domain.ToolResult. `call_id` is answered empty on purpose: the host owns the
/// correlation id and overwrites whatever the guest returns.
fn handle_call(request: &[u8]) -> Vec<u8> {
    let tool = extract_string_field(request, "tool").unwrap_or_default();
    if tool != "hello_echo" {
        return fail_result(&format!("unknown tool: {}", tool));
    }
    match extract_string_field(request, "name") {
        Some(name) if !name.is_empty() => {
            host_log(&format!("hello_echo called with name={}", name));
            ok_result(&format!("hello, {}!", name))
        }
        // A missing argument is the caller's mistake, and saying so is the whole
        // point: silently greeting nobody would be a plugin that always
        // "succeeds" and never tells anyone it did nothing.
        _ => fail_result("missing required argument: name"),
    }
}

/// plugin_invoke is the guest's only entry point. It must never trap: an
/// unknown op gets a small readable body instead, so a host that asks for
/// something this ABI version does not have still gets an answer.
#[no_mangle]
pub extern "C" fn plugin_invoke(op: i32, ptr: i32, len: i32) -> i64 {
    match op {
        // abi.OpManifest
        0 => write_out(MANIFEST),
        // abi.OpCallTool
        1 => {
            let request = unsafe { read_in(ptr, len) };
            write_out(&handle_call(request))
        }
        _ => write_out(br#"{"error":"unsupported op"}"#),
    }
}
