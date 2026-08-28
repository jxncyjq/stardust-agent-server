//! legion-hello：一个把闭环走完的最小插件。
//!
//! # 文件分工
//!
//! | 文件 | 内容 | 写自己的插件时 |
//! |---|---|---|
//! | `lib.rs`（本文件） | 自述、`plugin_invoke` 入口、op 分发 | 改 `MANIFEST` |
//! | `tools.rs` | 工具实现与按名分发 | **主要改这里** |
//! | `host.rs` | 七个 host 函数的声明与安全包装 | 按需打开 feature |
//! | `abi.rs` | alloc/free/打包解包 | 基本可以原样照抄 |
//! | `json.rs` | EXAMPLE-ONLY 的手搓 JSON | 换成 serde_json |
//!
//! # 两个 op
//!
//! | op | 常量 | 入参 | 返回 |
//! |---|---|---|---|
//! | 0 | `abi.OpManifest` | 忽略 | 自述 `{"name","version","provides"}` |
//! | 1 | `abi.OpCallTool` | `{"call_id","tool","arguments"}` | `domain.ToolResult` |
//!
//! 其它 op 一律返回一个可读的小 JSON 错误体，**不要 trap**：一个更新过的宿主
//! 问起这个 ABI 版本没有的东西时，它该得到一个答案，而不是一个死掉的模块。
//!
//! # 本示例刻意留空的部分
//!
//! 只开了 `log` 一个能力。其余六个 host 函数在 `host.rs` 里都写好了声明与包装，
//! 用 Cargo feature 关着，注释里写明了打开它需要联动改哪几处。留空不是省事：
//! **import 一个部署没有授权的 host 函数，模块会在实例化时就失败**，所以「只
//! import 用得上的」是一条硬规则，不是风格偏好。

mod abi;
mod host;
mod json;
mod tools;

// plugin_alloc / plugin_free 是宿主要求的导出，实现在 abi.rs（`#[no_mangle]`
// 的导出不受模块层级影响，wasm 里就是顶层的两个导出函数）。这里重新 pub use
// 一次，是为了让读这个文件的人看到「四个导出」是齐的。
pub use abi::{plugin_alloc, plugin_free};

/// op 0 返回的自述。
///
/// `provides` 必须覆盖部署声称这个插件贡献的每一个工具，否则激活期的交叉校验
/// 拒绝挂载。它与 `package/plugin.json` 的 `tools[].name`、以及 `tools.rs` 的
/// `dispatch` 分支是**同一份清单的三个副本**，加工具时三处都要改。
const MANIFEST: &[u8] = br#"{"name":"legion-hello","version":"0.1.0","provides":["hello_echo"]}"#;

/// 宿主用 `WithStartFunctions("_initialize")` 实例化：这个 guest 是 WASI
/// **reactor**（没有 `_start`）。
///
/// 真正的初始化工作放这里——放到「第一次调用时再做」会算进那次工具调用的超时
/// 预算里。本示例没有要初始化的东西，所以是空的；读配置（`host::config()`）、
/// 建缓存这类事就该写在这里。
#[no_mangle]
pub extern "C" fn _initialize() {}

/// plugin_invoke 是 guest 唯一的入口。
///
/// 返回值按 ABI 打包：高 32 位是指针，低 32 位是长度（见 `abi::write_out`）。
#[no_mangle]
pub extern "C" fn plugin_invoke(op: i32, ptr: i32, len: i32) -> i64 {
    match op {
        // abi.OpManifest
        0 => abi::write_out(MANIFEST),
        // abi.OpCallTool
        1 => {
            // SAFETY: 这段内存归宿主所有，只在本次调用期间有效；dispatch 不会
            // 把这个切片存到调用之外。
            let request = unsafe { abi::read_in(ptr, len) };
            abi::write_out(&tools::dispatch(request))
        }
        _ => abi::write_out(br#"{"error":"unsupported op"}"#),
    }
}
