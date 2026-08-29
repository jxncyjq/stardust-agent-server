//! legion-hello：用 SDK 写的最小插件，闭环走完 install → grant → serve →
//! `state:"loaded"`。
//!
//! 与 SDK 之前的手写版相比消失了四个文件（abi/host/json/tools），以及「三处
//! 联动」里的一处：op 0 的 `provides` 现在由 `declare_plugin!` 从工具列表推导，
//! 不再是需要人工同步的第三份清单。
//!
//! 加一个工具因此只剩两处：这里的 `tools = [...]`，与 `scripts/build.sh` 内联
//! 的 `plugin.json` 的 `tools`（后者是部署侧的接受清单，宿主据它注册）。
//!
//! 完整的能力表、边界与取舍见 `sdk/rust/legion-plugin/README.md`。

use legion_plugin::{declare_plugin, log_info, ToolCall, ToolResult};

declare_plugin!(
    name = "legion-hello",
    version = "0.1.0",
    tools = [("hello_echo", hello_echo)]
);

/// hello_echo 读入参 `name`，经 `log` 能力回调宿主写一行日志，再把问候语作为
/// 结果返回。那行日志是「能力真的到达了 guest」的可见证据：没有 `log` 授权，
/// 这个模块连链接都过不去。
///
/// 缺 `name` 返回**失败结果**并点名缺了哪个参数——不要 panic，panic 会 trap
/// 整个模块；也不要悄悄问候一个空名字，那会变成一个永远「成功」、却从不告诉
/// 任何人自己什么也没做的插件。
fn hello_echo(call: &ToolCall) -> ToolResult {
    match call.argument("name") {
        Some(name) => {
            log_info(&format!("hello_echo called with name={name}"));
            ToolResult::ok(format!("hello, {name}!"))
        }
        None => ToolResult::fail("missing required argument: name"),
    }
}
