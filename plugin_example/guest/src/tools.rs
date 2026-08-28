//! 工具实现与分发：写自己的插件时，**主要改的就是这个文件**。
//!
//! 加一个工具是三处联动，缺一处就不会生效：
//!
//! 1. 本文件：写实现，并在 `dispatch` 的 match 里加一条分支；
//! 2. `lib.rs` 的 `MANIFEST`（op 0 的自述）：`provides` 加上工具名——不加则激活
//!    期的交叉校验发现「部署声称的工具 guest 不提供」，直接拒绝挂载；
//! 3. `scripts/build.sh` 里内联的 `plugin.json`：`tools` 加一条，含 `group` 与
//!    正数的 `timeout_ms`（两者都是必填，缺了清单解析就失败）。

use crate::host;
use crate::json;

/// ok 与 fail 生成宿主要读的 `domain.ToolResult`。
///
/// `call_id` 一律留空：相关性 id 由宿主持有，它会覆盖 guest 返回的值，所以在
/// guest 里编一个只会误导读日志的人。
fn ok(output: &str) -> Vec<u8> {
    format!(
        "{{\"call_id\":\"\",\"success\":true,\"output\":\"{}\",\"error\":\"\"}}",
        json::escape(output)
    )
    .into_bytes()
}

/// fail 是「这次工具调用没做成」的正确表达方式。
///
/// **不要用 panic 表达失败**：panic 会 trap 整个 wasm 模块，代价是实例里的状态
/// 和同一实例上所有在途调用；而失败结果只是模型能读懂、能据此改口再试的一个
/// 答案。
fn fail(message: &str) -> Vec<u8> {
    format!(
        "{{\"call_id\":\"\",\"success\":false,\"output\":\"\",\"error\":\"{}\"}}",
        json::escape(message)
    )
    .into_bytes()
}

/// dispatch 是 op 1 的入口：按 `tool` 字段选实现。
///
/// 宿主发来的请求是 `{"call_id":…,"tool":…,"arguments":{…}}`。宿主只会派发它
/// 自己注册过的工具名，但这里仍然要有 `_` 分支：契约之外的输入要有一个说得出
/// 口的答案，而不是让匹配失败去 panic。
pub fn dispatch(request: &[u8]) -> Vec<u8> {
    let tool = json::extract_string_field(request, "tool").unwrap_or_default();
    match tool.as_str() {
        "hello_echo" => hello_echo(request),

        // ── 第二个工具的位置 ────────────────────────────────────────────
        // 需要出站 HTTP 的工具长这样（打开 feature "http-capability"、并在
        // plugin.json 里同时声明 capabilities:["http"] 与
        // network.allowed_hosts 之后即可启用）：
        //
        // #[cfg(feature = "http-capability")]
        // "fetch_title" => {
        //     let url = match json::extract_string_field(request, "url") {
        //         Some(u) if !u.is_empty() => u,
        //         _ => return fail("missing required argument: url"),
        //     };
        //     let response = host::http(&format!(
        //         "{{\"method\":\"GET\",\"url\":\"{}\"}}", json::escape(&url)
        //     ));
        //     // 响应要么是 {"status":…,"body":…}，要么是错误信封
        //     // {"code":"DENIED"|"INVALID_REQUEST"|"HOST_ERROR","message":…}，
        //     // 两者都要判：把 DENIED 当成空 body 继续跑，就是在替越权的调用
        //     // 打掩护。
        //     match json::extract_string_field(&response, "code") {
        //         Some(code) => fail(&format!("http_request refused: {}", code)),
        //         None => ok(&String::from_utf8_lossy(&response)),
        //     }
        // }
        // ────────────────────────────────────────────────────────────────
        _ => fail(&format!("unknown tool: {}", tool)),
    }
}

/// hello_echo 是本示例唯一真正实现的工具：读入参 `name`，经 `log` 能力回调宿主
/// 写一行日志，再把问候语作为结果返回。
///
/// 它同时演示了参数缺失的处理：缺 `name` 返回失败结果并**点名**缺了哪个参数。
/// 悄悄问候一个空名字，会变成一个永远「成功」、却从不告诉任何人自己什么也没做
/// 的插件。
fn hello_echo(request: &[u8]) -> Vec<u8> {
    match json::extract_string_field(request, "name") {
        Some(name) if !name.is_empty() => {
            host::log_info(&format!("hello_echo called with name={}", name));
            ok(&format!("hello, {}!", name))
        }
        _ => fail("missing required argument: name"),
    }
}
