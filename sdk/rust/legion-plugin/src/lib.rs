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
pub use tool::{
    Decider, Observer, ToolCall, ToolDecision, ToolDecisionRequest, ToolHandler, ToolObservation,
    ToolResult,
};

/// manifest_json 渲染 op 0 的自述。
///
/// 由 [`declare_plugin!`] 调用，作者不需要碰。`provides` 来自宏收到的工具列表，
/// 这正是它存在的理由：自述与注册表同源。
pub fn manifest_json(name: &str, version: &str, provides: &[&str], extensions: &[&str]) -> String {
    format!(
        "{{\"name\":\"{}\",\"version\":\"{}\",\"provides\":[{}],\"extensions\":[{}]}}",
        json::escape(name),
        json::escape(version),
        string_list(provides),
        string_list(extensions)
    )
}

/// string_list 把一串名字渲染成 JSON 数组的**内容**（不含方括号）。
fn string_list(items: &[&str]) -> String {
    let mut list = String::new();
    for (i, item) in items.iter().enumerate() {
        if i > 0 {
            list.push(',');
        }
        list.push('"');
        list.push_str(&json::escape(item));
        list.push('"');
    }
    list
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
    // 四条入口，对应两个可选 seam 的四种组合。它们只做一件事：把「有没有」翻译
    // 成 @build 里的重复语法，真正的展开只有一份。
    (name = $name:expr, version = $version:expr,
     tools = [$(($tool:expr, $handler:expr)),* $(,)?] $(,)?) => {
        $crate::declare_plugin!(@build name = $name, version = $version,
            tools = [$(($tool, $handler)),*], observe = [], decide = []);
    };
    (name = $name:expr, version = $version:expr,
     tools = [$(($tool:expr, $handler:expr)),* $(,)?], observe = $observer:expr $(,)?) => {
        $crate::declare_plugin!(@build name = $name, version = $version,
            tools = [$(($tool, $handler)),*], observe = [$observer], decide = []);
    };
    (name = $name:expr, version = $version:expr,
     tools = [$(($tool:expr, $handler:expr)),* $(,)?], decide = $decider:expr $(,)?) => {
        $crate::declare_plugin!(@build name = $name, version = $version,
            tools = [$(($tool, $handler)),*], observe = [], decide = [$decider]);
    };
    (name = $name:expr, version = $version:expr,
     tools = [$(($tool:expr, $handler:expr)),* $(,)?],
     observe = $observer:expr, decide = $decider:expr $(,)?) => {
        $crate::declare_plugin!(@build name = $name, version = $version,
            tools = [$(($tool, $handler)),*], observe = [$observer], decide = [$decider]);
    };
    (@build name = $name:expr, version = $version:expr,
     tools = [$(($tool:expr, $handler:expr)),*],
     observe = [$($observer:expr)?], decide = [$($decider:expr)?]) => {
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
                    // extensions 与 provides 同源：写了 `observe = ...` 才有
                    // "observe"，写了 `decide = ...` 才有 "decide"。宿主会拿部署
                    // 侧的 grant.extensions 与这份自述交叉校验，所以一个「授权了
                    // 但没实现」的组合会在激活期被拒绝，而不是每次调用都白跑。
                    let extensions: &[&str] = &[
                        $($crate::declare_plugin!(@name_of observe, $observer),)?
                        $($crate::declare_plugin!(@name_of decide, $decider),)?
                    ];
                    $crate::abi::write_out(
                        $crate::manifest_json($name, $version, provides, extensions).as_bytes())
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
                // abi.OpObserveToolResult —— 只有声明了 `observe = ...` 的插件
                // 才会生成这条分支；没有它时 op 2 落到下面的「未知 op」，那也
                // 正是宿主对一个没实现这个 seam 的插件该听到的话。
                $(
                    2 => {
                        // SAFETY: 同 op 1——只在本次调用期间借用。
                        let body = unsafe { $crate::abi::read_in(ptr, len) };
                        if let Ok(observation) = $crate::ToolObservation::parse(body) {
                            let observer: $crate::Observer = $observer;
                            observer(&observation);
                        }
                        // 解析失败时无人可报：这个 seam 按构造就是单向的。宿主
                        // 会拿到一个格式正确的应答（它随即丢弃），但**不会**拿到
                        // 一个 trap ——trap 才会伤到那个正等着结果的调用方。
                        $crate::abi::write_out(b"{}")
                    }
                )?
                // abi.OpDecideToolCall —— 同样只在声明了 `decide = ...` 时生成。
                // 与 op 2 相反的是**失败方向**：这里的答案有后果，所以读不懂的
                // 请求答 deny 而不是一个无害的确认。宿主对读不懂的**答案**本就
                // fail-closed，这里答 deny 只是把理由说清楚。
                $(
                    3 => {
                        // SAFETY: 同 op 1——只在本次调用期间借用。
                        let body = unsafe { $crate::abi::read_in(ptr, len) };
                        let decision = match $crate::ToolDecisionRequest::parse(body) {
                            Ok(request) => {
                                let decider: $crate::Decider = $decider;
                                decider(&request)
                            }
                            Err(err) => $crate::ToolDecision::deny(
                                ::std::format!("could not decode the decision request: {}", err)),
                        };
                        $crate::abi::write_out(decision.to_json().as_bytes())
                    }
                )?
                _ => $crate::abi::write_out(br#"{"error":"unsupported op"}"#),
            }
        }
    };
    // @name_of 把「有没有这个 seam」变成 extensions 里的名字：宏的重复语法只在
    // 有对应函数时展开这一项，函数本身被丢弃。
    (@name_of observe, $observer:expr) => { "observe" };
    (@name_of decide, $decider:expr) => { "decide" };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_lists_every_tool_it_was_given() {
        let json = manifest_json("legion-hello", "0.1.0", &["a", "b"], &[]);
        assert_eq!(
            json,
            r#"{"name":"legion-hello","version":"0.1.0","provides":["a","b"],"extensions":[]}"#
        );
    }

    #[test]
    fn manifest_of_a_single_tool_has_no_trailing_comma() {
        let json = manifest_json("p", "1", &["only"], &[]);
        assert!(json.contains(r#""provides":["only"]"#), "got {json}");
    }

    /// 一个注册了观察者的插件必须**说出来**：宿主拿这份自述与部署侧的
    /// grant.extensions 交叉校验，缺了它，一个正确的授权反而会被拒。
    #[test]
    fn manifest_names_the_extensions_it_implements() {
        let json = manifest_json("p", "1", &["only"], &["observe"]);
        assert!(json.contains(r#""extensions":["observe"]"#), "got {json}");
    }
}
