//! ABI 的机械部分：宿主与 guest 之间怎么交换字节。
//!
//! 这一层与插件做什么无关，写你自己的插件时几乎可以原样照抄；真正要改的是
//! `tools.rs`。四个导出（外加线性内存）是宿主的硬要求，缺一个就拒绝实例化：
//!
//! | 导出 | 签名 | 谁调用 |
//! |---|---|---|
//! | `plugin_alloc` | `(i32) -> i32` | 宿主：写入参前、host 函数写返回体前 |
//! | `plugin_free` | `(i32, i32)` | 宿主：调用结束后归还入参内存 |
//! | `plugin_invoke` | `(i32, i32, i32) -> i64` | 宿主：唯一入口（见 lib.rs） |
//! | `_initialize` | `()` | 宿主：实例化时（WASI reactor，没有 `_start`） |
//! | `memory` | — | 由 `wasm32-wasip1` 目标自动导出 |

use std::alloc::{alloc, dealloc, Layout};

/// plugin_alloc 是宿主预留 guest 内存的唯一方式：既用于它即将写入的请求体，
/// 也用于 host 函数交还的响应体。
///
/// 返回 0 表示分配不了。宿主会把它当成错误报告出来，而不是拿 0 当地址用。
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

/// plugin_free 归还 plugin_alloc 预留的内存。宿主为它写过的每一个请求体都会
/// 调用一次，**包括那次调用失败的情况**。
#[no_mangle]
pub extern "C" fn plugin_free(ptr: i32, size: i32) {
    if ptr <= 0 || size <= 0 {
        return;
    }
    if let Ok(layout) = Layout::from_size_align(size as usize, 1) {
        unsafe { dealloc(ptr as *mut u8, layout) }
    }
}

/// write_out 把 `body` 复制进新分配的 guest 内存，并按 ABI 打包返回值：
/// 高 32 位是指针，低 32 位是长度。宿主读完这块区域后会调 plugin_free 归还它。
///
/// 分配失败时返回 0（等价于 ptr=0,len=0），宿主把它读作空响应体。
pub fn write_out(body: &[u8]) -> i64 {
    let ptr = plugin_alloc(body.len() as i32);
    if ptr == 0 {
        return 0;
    }
    unsafe { std::ptr::copy_nonoverlapping(body.as_ptr(), ptr as *mut u8, body.len()) };
    ((ptr as i64) << 32) | (body.len() as i64)
}

/// read_in 借用宿主写在 (ptr, len) 处的请求体。
///
/// 空请求体是合法的：宿主对空入参**不调用** plugin_alloc，直接以 ptr=0、len=0
/// 进入 plugin_invoke。所以这里返回空切片，而不是 trap。
///
/// # Safety
/// 返回的切片只在本次 `plugin_invoke` 调用期间有效——那段内存归宿主管，调用
/// 返回后它就会被 plugin_free 收走。不要把它存到全局里。
pub unsafe fn read_in<'a>(ptr: i32, len: i32) -> &'a [u8] {
    if ptr <= 0 || len <= 0 {
        return &[];
    }
    std::slice::from_raw_parts(ptr as *const u8, len as usize)
}

/// take_host_body 把一个 host 函数返回的打包结果读成 owned 字节，并**归还那块
/// 内存**。
///
/// 归还这件事容易漏，而漏了就是每次调用泄漏一块 guest 内存：host 函数是通过
/// **本 guest 的 plugin_alloc** 分配这块区域的，所以它的所有权在返回的一刻就
/// 转给了 guest。
///
/// 唯一的例外是「把 host 的返回值原样当作 plugin_invoke 的返回值抛回去」——
/// 那种情况下宿主会把它当作本次调用的响应体来读和释放，guest 不能再释放一次。
/// 本示例不用那种写法，因为它只在 guest 完全不解析结果时才成立。
///
/// # Safety
/// `packed` 必须是某个 legion host 函数刚刚返回的值，且尚未被释放过。
#[allow(dead_code)] // 只有开启了带返回值的能力特性时才会被用到
pub unsafe fn take_host_body(packed: i64) -> Vec<u8> {
    let ptr = (packed >> 32) as i32;
    let len = (packed & 0xFFFF_FFFF) as i32;
    if ptr <= 0 || len <= 0 {
        return Vec::new();
    }
    let owned = std::slice::from_raw_parts(ptr as *const u8, len as usize).to_vec();
    plugin_free(ptr, len);
    owned
}
