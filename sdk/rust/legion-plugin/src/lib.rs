//! `legion-plugin`：写 Legion Agent 的 WASM 插件用的 guest SDK（ABI v1）。
//!
//! # 一个完整的插件
//!
//! ```ignore
//! use legion_plugin::{declare_plugin, log_info, ToolCall, ToolResult};
//!
//! declare_plugin!(
//!     name = "legion-hello",
//!     version = "0.1.0",
//!     tools = [("hello_echo", hello_echo)]
//! );
//!
//! fn hello_echo(call: &ToolCall) -> ToolResult {
//!     match call.argument("name") {
//!         Some(name) => {
//!             log_info(&format!("hello_echo called with name={name}"));
//!             ToolResult::ok(format!("hello, {name}!"))
//!         }
//!         None => ToolResult::fail("missing required argument: name"),
//!     }
//! }
//! ```
//!
//! 这就是全部。SDK 负责四个导出、内存管理、指针打包、op 分发、JSON，以及
//! **op 0 的自述**——`provides` 由 `tools` 列表推导，所以「guest 说自己提供什么」
//! 与「实际注册了什么」不可能对不上（那个对不上会让激活期的交叉校验拒绝挂载，
//! 是手写 ABI 时最容易犯的错之一）。
//!
//! # 能力
//!
//! 默认只有 `log`。其余六个 host 函数在 [`host`] 里，用 feature 关着：
//!
//! | 能力 | feature | 函数 |
//! |---|---|---|
//! | `log` | 默认开 | [`log_info`] |
//! | `config` | `config-capability` | [`host::config`] |
//! | `kv` | `kv-capability` | [`host::kv_read`] / [`host::kv_write`] |
//! | `http` | `http-capability` | [`host::http`] |
//! | `fs` | `fs-capability` | [`host::read`] |
//! | `tool` | `tool-capability` | [`host::invoke_tool`] |
//!
//! 打开一个 feature 必须同时做另外两件事：`plugin.json` 的 `capabilities` 加上
//! 同名能力，部署侧 `agent plugins grant --capabilities` 里也要有它。少任何一
//! 步，模块都会在**实例化**时失败——能力是链接期事实，不是运行期开关。
//!
//! # 边界
//!
//! - 工具失败要返回 [`ToolResult::fail`]，**不要 panic**：panic 会 trap 整个
//!   模块，代价是实例状态、同实例的在途调用，并计入插件健康度。
//! - 初始化工作放 `_initialize`（`declare_plugin!` 生成的是空实现；要做事就自
//!   己写一个同名函数并去掉宏里的那份——见 README）。放到首次调用里做，会算进
//!   那次工具调用的超时预算。

pub mod abi;
pub mod host;
pub mod json;
pub mod tool;

pub use host::log_info;
pub use tool::{ToolCall, ToolHandler, ToolResult};

/// manifest_json 渲染 op 0 的自述。
///
/// 由 [`declare_plugin!`] 调用，作者不需要碰。`provides` 来自宏收到的工具列表，
/// 这正是它存在的理由：自述与注册表同源。
pub fn manifest_json(name: &str, version: &str, provides: &[&str]) -> String {
    let mut list = String::new();
    for (i, tool) in provides.iter().enumerate() {
        if i > 0 {
            list.push(',');
        }
        list.push('"');
        list.push_str(&json::escape(tool));
        list.push('"');
    }
    format!(
        "{{\"name\":\"{}\",\"version\":\"{}\",\"provides\":[{}]}}",
        json::escape(name),
        json::escape(version),
        list
    )
}

/// declare_plugin 生成一个插件的全部 ABI 面：四个导出与 op 分发。
///
/// ```ignore
/// declare_plugin!(
///     name = "legion-hello",
///     version = "0.1.0",
///     tools = [("hello_echo", hello_echo), ("hello_upper", hello_upper)]
/// );
/// ```
///
/// 工具名在两处出现（这里与 `plugin.json` 的 `tools`），比手写 ABI 少一处——
/// op 0 的 `provides` 不再是第三份需要人工同步的清单。
///
/// 未知 op **不会 trap**：返回一个可读的小 JSON 错误体，这样一个更新过的宿主问
/// 起这个 ABI 版本没有的东西时，得到的是答案而不是一个死掉的模块。
#[macro_export]
macro_rules! declare_plugin {
    (name = $name:expr, version = $version:expr, tools = [$(($tool:expr, $handler:expr)),* $(,)?]) => {
        /// 宿主用 `WithStartFunctions("_initialize")` 实例化：guest 是 WASI
        /// reactor（没有 `_start`）。
        #[no_mangle]
        pub extern "C" fn _initialize() {}

        #[no_mangle]
        pub extern "C" fn plugin_alloc(size: i32) -> i32 {
            $crate::abi::alloc(size)
        }

        #[no_mangle]
        pub extern "C" fn plugin_free(ptr: i32, size: i32) {
            $crate::abi::free(ptr, size)
        }

        #[no_mangle]
        pub extern "C" fn plugin_invoke(op: i32, ptr: i32, len: i32) -> i64 {
            match op {
                // abi.OpManifest
                0 => {
                    let provides: &[&str] = &[$($tool),*];
                    $crate::abi::write_out($crate::manifest_json($name, $version, provides).as_bytes())
                }
                // abi.OpCallTool
                1 => {
                    // SAFETY: 这段内存归宿主所有，只在本次调用期间有效；下面的
                    // 分发不会把它存到调用之外。
                    let body = unsafe { $crate::abi::read_in(ptr, len) };
                    let result = match $crate::ToolCall::parse(body) {
                        Ok(call) => match call.tool.as_str() {
                            $(
                                $tool => {
                                    let handler: $crate::ToolHandler = $handler;
                                    handler(&call)
                                }
                            )*
                            // 宿主只会派发它注册过的工具名，但契约之外的输入也
                            // 要有一个说得出口的答案，而不是让匹配失败去 panic。
                            other => $crate::ToolResult::fail(
                                ::std::format!("unknown tool: {}", other)),
                        },
                        Err(err) => $crate::ToolResult::fail(
                            ::std::format!("bad tool call: {}", err)),
                    };
                    $crate::abi::write_out(result.to_json().as_bytes())
                }
                _ => $crate::abi::write_out(br#"{"error":"unsupported op"}"#),
            }
        }
    };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_lists_every_tool_it_was_given() {
        let json = manifest_json("legion-hello", "0.1.0", &["a", "b"]);
        assert_eq!(
            json,
            r#"{"name":"legion-hello","version":"0.1.0","provides":["a","b"]}"#
        );
    }

    #[test]
    fn manifest_of_a_single_tool_has_no_trailing_comma() {
        let json = manifest_json("p", "1", &["only"]);
        assert!(json.contains(r#""provides":["only"]"#), "got {json}");
    }
}
