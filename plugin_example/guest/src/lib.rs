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

use legion_plugin::{
    declare_plugin, log_info, ToolCall, ToolDecision, ToolDecisionRequest, ToolObservation, ToolResult,
};

declare_plugin!(
    name = "legion-hello",
    version = "0.1.0",
    tools = [("hello_echo", hello_echo)],
    // observe 是一个**扩展点**，不是能力：宿主在每次工具调用答完之后回调这里。
    // 它同样是三处联动——这一行、`plugin.json` 的 `extensions`、部署侧的
    // `agent plugins grant --extensions observe`。少了最后一步，宿主根本不会
    // 注册观察者（未授权 = 不存在的注册，不是运行期检查）；少了中间那步，激活
    // 期的交叉校验会拒绝这次授权。
    observe = log_observation,
    // decide 是第二个扩展点，也是第一个**答案有后果**的：宿主在派发任何工具
    // 之前问这里，回答只能让结果更严（放行不是授权——宿主自己的权限与策略在
    // 插件被问到之前就已经放行了）。答不出来（超时/trap/坏文档）= 拒绝，并计入
    // 本插件健康度。
    decide = decide_call
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

/// log_observation 是观察者：宿主每完成一次工具调用（**任意工具**，不只是本
/// 插件自己的）就调它一次，把调用与结果一起给过来。
///
/// 它什么也不返回，也改不了任何东西——调用方拿到结果那一刻就已经定了，宿主会
/// 丢弃这里的应答。这就是这个 seam 敢交给不受信代码的原因。
///
/// 它必须**快**：这段代码跑在别人的工具调用里，宿主给每次通知 200ms 上限，超
/// 时按故障计入本插件的健康度。要做贵的活儿，就把需要的东西记下来，留到自己
/// 的下一次工具调用里做。
fn log_observation(observation: &ToolObservation) {
    log_info(&format!(
        "observed tool={} success={}",
        observation.tool, observation.success
    ));
}

/// decide_call 是决策者：宿主在派发**任意**工具之前问它一次。
///
/// 这里只拦一个虚构的工具名，好让示例的测试能把放行与拒绝两条路都跑到。真正的
/// 守门插件还会看参数（路径、主机、金额……）。
///
/// 三条不能忘的事：①**放行不是授权**，只是「我不反对」；②它必须快——上限是
/// `min(工具超时/4, 200ms)`，而工具还没开始跑；③**答不出来就是拒绝**，宿主
/// fail-closed，并把这次失败计进本插件的健康度（连续失败足够多次会被卸载）。
fn decide_call(request: &ToolDecisionRequest) -> ToolDecision {
    if request.tool == "forbidden_tool" {
        return ToolDecision::deny("forbidden_tool is refused by legion-hello");
    }
    if request.tool == "reviewed_tool" {
        // ask 不是拒绝：宿主会挂起这一轮、开一张点名本插件与这条理由的审批票，
        // 人批了再继续。没有审批通道的部署里它按拒绝处理。
        return ToolDecision::ask("reviewed_tool is looked at by a human first");
    }
    ToolDecision::allow()
}
