// Test fixture guest for internal/plugin/host.
//
// Implements the plugin ABI (plugin_alloc/plugin_free/plugin_invoke, see
// internal/plugin/abi) with a fixed op table. Ops 0 and 1 are the real ABI
// ops (abi.OpManifest, abi.OpCallTool); ops 90/98/99 are test-only and exist
// solely to exercise the host wrapper (echo round trip, memory-cap trap,
// context-cancellation trap). See ../README.md for the full op table and the
// build command.
//
// Deliberately imports nothing from a "legion" host module: Task 2's host
// wrapper does not register any host functions (that is Task 3), so this
// fixture must instantiate against WASI alone.

use std::alloc::{alloc, dealloc, Layout};

#[no_mangle]
pub extern "C" fn plugin_alloc(size: i32) -> i32 {
    if size <= 0 {
        return 0;
    }
    unsafe { alloc(Layout::from_size_align(size as usize, 1).unwrap()) as i32 }
}

#[no_mangle]
pub extern "C" fn plugin_free(ptr: i32, size: i32) {
    if ptr == 0 || size <= 0 {
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
        _ => write_out(br#"{"error":"unsupported op"}"#),
    }
}
