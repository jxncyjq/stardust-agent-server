//! 七个 host 函数在这里声明，但**默认只 import 一个**（`log`）。
//!
//! # 为什么其余六个用 feature 关着，而不是直接写在这里
//!
//! 能力不是运行期开关，是**链接期事实**：宿主只为已授权的能力注册对应的 host
//! 函数，所以 import 一个部署没有授权的函数，模块会在**实例化**时就失败——
//! guest 根本没有机会去调用它，也不会收到一个 `DENIED` 让它处理。
//!
//! 于是「import 什么」必须与 `plugin.json` 的 `capabilities` 严格对齐，而对齐
//! 这件事在源码里最容易忘。这里用 Cargo feature 把每个能力单独关着，打开一个
//! feature 就是一次**三处联动**的显式动作：
//!
//! 1. `Cargo.toml` 打开 feature（构建时 `--features kv-capability`）；
//! 2. `package/plugin.json` 的 `capabilities` 加上同名能力（`scripts/build.sh`
//!    里内联的那份）；
//! 3. 部署侧 `agent plugins grant --capabilities <完整集合>` 里也要有它。
//!
//! 少做第 2 步 → 部署授不出这个能力 → 实例化失败；少做第 3 步 → grant 被拒
//! （`--capabilities` 必须恰好等于声明集合）。
//!
//! # 各能力的请求 / 响应形状
//!
//! | 能力 | 函数 | 请求 | 响应 |
//! |---|---|---|---|
//! | `log` | `log(level, ptr, len)` | 纯文本 | 无返回值 |
//! | `config` | `config_get()` | — | `plugins.json` 里该 entry 的 `config` 原文 |
//! | `kv` | `kv_get(kp, kl)` | 键的原始字节 | `{"found":bool,"value":string}` |
//! | `kv` | `kv_put(kp, kl, vp, vl)` | 键与值的原始字节 | `{"ok":bool}` |
//! | `http` | `http_request(ptr, len)` | `{"method","url","headers"?,"body"?}` | `{"status","headers"?,"body","truncated"?}` |
//! | `fs` | `read_file(ptr, len)` | `{"path"}` | `{"path","content","truncated"?}` |
//! | `tool` | `call_tool(ptr, len)` | `{"call_id"?,"tool","arguments"?}` | `domain.ToolResult` |
//!
//! 失败一律是错误信封 `{"code":…,"message":…}`，`code` 取 `DENIED`（越过了
//! 授权边界，比如主机不在 `allowed_hosts`）、`INVALID_REQUEST`、`HOST_ERROR`。
//! HTTP 响应体与文件内容各截断到 1 MiB，截断时 `truncated:true` 会明说。

// ---------------------------------------------------------------------------
// log —— 本示例默认开启的唯一能力
// ---------------------------------------------------------------------------

#[link(wasm_import_module = "legion")]
extern "C" {
    fn log(level: i32, ptr: i32, len: i32);
}

/// 宿主日志级别：0=debug 1=info 2=warn 3=error。传一个未知级别不会被四舍五入
/// 成 info，宿主会按 error 记下来并标注「未知级别」，好让写错的调用方现形。
const LEVEL_INFO: i32 = 1;

/// log_info 往宿主日志写一行。
///
/// 这是「能力真的到达了 guest」的可见证据：没有 `log` 授权，这个模块连链接都
/// 过不去。
pub fn log_info(message: &str) {
    let bytes = message.as_bytes();
    unsafe { log(LEVEL_INFO, bytes.as_ptr() as i32, bytes.len() as i32) }
}

// ---------------------------------------------------------------------------
// config —— 留空的槽位：打开 feature "config-capability" 才 import
// ---------------------------------------------------------------------------

// 脚手架：打开 feature 只是把这条通路接上，真正用到它的工具由插件作者在
// tools.rs 里写。在那之前 dead_code 是预期的，不是问题。
#[cfg(feature = "config-capability")]
#[allow(dead_code)]
#[link(wasm_import_module = "legion")]
extern "C" {
    fn config_get() -> i64;
}

/// config 返回部署在 `plugins.json` 该 entry 的 `config` 字段里写的**原文
/// JSON**，逐字节交给插件，宿主不解释它的结构——schema 由插件自己定义。
///
/// 典型用法：在 `_initialize` 里读一次并缓存，而不是每次工具调用都读。
#[cfg(feature = "config-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn config() -> Vec<u8> {
    unsafe { crate::abi::take_host_body(config_get()) }
}

// ---------------------------------------------------------------------------
// kv —— 留空的槽位：feature "kv-capability"
// ---------------------------------------------------------------------------

// 脚手架：打开 feature 只是把这条通路接上，真正用到它的工具由插件作者在
// tools.rs 里写。在那之前 dead_code 是预期的，不是问题。
#[cfg(feature = "kv-capability")]
#[allow(dead_code)]
#[link(wasm_import_module = "legion")]
extern "C" {
    fn kv_get(kp: i32, kl: i32) -> i64;
    fn kv_put(kp: i32, kl: i32, vp: i32, vl: i32) -> i64;
}

/// kv_read 读一个键，返回 `{"found":bool,"value":string}` 的原始 JSON。
///
/// 键会被宿主按插件命名空间限定后再落到存储上，所以拼出别的插件的键名也读不到
/// 它们的数据。
#[cfg(feature = "kv-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn kv_read(key: &str) -> Vec<u8> {
    let k = key.as_bytes();
    unsafe { crate::abi::take_host_body(kv_get(k.as_ptr() as i32, k.len() as i32)) }
}

/// kv_write 写一个键，返回 `{"ok":bool}` 的原始 JSON。
#[cfg(feature = "kv-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn kv_write(key: &str, value: &str) -> Vec<u8> {
    let k = key.as_bytes();
    let v = value.as_bytes();
    unsafe {
        crate::abi::take_host_body(kv_put(
            k.as_ptr() as i32,
            k.len() as i32,
            v.as_ptr() as i32,
            v.len() as i32,
        ))
    }
}

// ---------------------------------------------------------------------------
// http —— 留空的槽位：feature "http-capability"
// ---------------------------------------------------------------------------

// 脚手架：打开 feature 只是把这条通路接上，真正用到它的工具由插件作者在
// tools.rs 里写。在那之前 dead_code 是预期的，不是问题。
#[cfg(feature = "http-capability")]
#[allow(dead_code)]
#[link(wasm_import_module = "legion")]
extern "C" {
    fn http_request(ptr: i32, len: i32) -> i64;
}

/// http 发一次出站请求。`request` 是 `{"method","url","headers"?,"body"?}`。
///
/// 光有 `http` 能力还不够：URL 的主机必须在授权的 `allowed_hosts` 里，scheme
/// 必须是 http/https，**并且每一跳重定向都会重新校验**——跳出白名单同样被拒。
/// 所以 `plugin.json` 里除了 `capabilities` 还要写 `network.allowed_hosts`，
/// 否则 grant 会直接拒绝授 `http`（授一个够不到任何主机的白名单没有意义）。
#[cfg(feature = "http-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn http(request: &str) -> Vec<u8> {
    let b = request.as_bytes();
    unsafe { crate::abi::take_host_body(http_request(b.as_ptr() as i32, b.len() as i32)) }
}

// ---------------------------------------------------------------------------
// fs —— 留空的槽位：feature "fs-capability"
// ---------------------------------------------------------------------------

// 脚手架：打开 feature 只是把这条通路接上，真正用到它的工具由插件作者在
// tools.rs 里写。在那之前 dead_code 是预期的，不是问题。
#[cfg(feature = "fs-capability")]
#[allow(dead_code)]
#[link(wasm_import_module = "legion")]
extern "C" {
    fn read_file(ptr: i32, len: i32) -> i64;
}

/// read 读一个文件，`request` 是 `{"path":"…"}`。
///
/// 与 http 同理：路径必须落在授权的 `allowed_paths` 内，且 `plugin.json` 要声明
/// `filesystem.allowed_paths`。内容超过 1 MiB 会被截断，响应里的 `truncated`
/// 会明说——不要把截断的内容当完整文件解析。
#[cfg(feature = "fs-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn read(request: &str) -> Vec<u8> {
    let b = request.as_bytes();
    unsafe { crate::abi::take_host_body(read_file(b.as_ptr() as i32, b.len() as i32)) }
}

// ---------------------------------------------------------------------------
// tool —— 留空的槽位：feature "tool-capability"
// ---------------------------------------------------------------------------

// 脚手架：打开 feature 只是把这条通路接上，真正用到它的工具由插件作者在
// tools.rs 里写。在那之前 dead_code 是预期的，不是问题。
#[cfg(feature = "tool-capability")]
#[allow(dead_code)]
#[link(wasm_import_module = "legion")]
extern "C" {
    fn call_tool(ptr: i32, len: i32) -> i64;
}

/// invoke_tool 调用**别的插件或宿主**贡献的工具，`request` 是
/// `{"call_id"?,"tool","arguments"?}`，返回 `domain.ToolResult`。
///
/// 两条硬边界：单链嵌套深度上限是 3；而且这些调用与模型共用同一份 per-task
/// 工具预算——插件替模型多调一次，模型自己就少一次。
///
/// 依赖别的插件的工具时，`plugin.json` 里要用 `requires` 把它们列出来。它与
/// `capabilities` 不同类：能力缺失是加载期拒载，`requires` 未满足只是可恢复的
/// `suspended` 状态，提供方回来就恢复。
#[cfg(feature = "tool-capability")]
#[allow(dead_code)] // 同上：等作者写出用它的工具
pub fn invoke_tool(request: &str) -> Vec<u8> {
    let b = request.as_bytes();
    unsafe { crate::abi::take_host_body(call_tool(b.as_ptr() as i32, b.len() as i32)) }
}

/// invoke_service 按**能力名**调用当前提供者，而不是写死某个插件的工具名：
/// `invoke_service("issue-tracker", "search", r#"{"q":"bug"}"#)` 发出的
/// `tool` 是 `service:issue-tracker/search`，宿主解析到此刻提供该服务的插件的
/// 工具。换提供者不用改这里。
///
/// 与 [`invoke_tool`] 的边界完全相同（深度上限 3、与模型共用 per-task 预算），
/// 因为解析之后走的就是同一条路。
///
/// 依赖某个服务时，`plugin.json` 里要用 `requires_services` 列出它——没有提供者
/// 时本插件会 `suspended`（不是卸载），提供者到场即恢复。
///
/// `arguments_json` 传 `"{}"` 表示无参数。它被原样嵌进请求体，所以必须是一个
/// 合法的 JSON 对象；这里不代为拼装，是为了不在 guest 侧多养一个 JSON 编码器。
#[cfg(feature = "tool-capability")]
#[allow(dead_code)]
pub fn invoke_service(service: &str, capability: &str, arguments_json: &str) -> Vec<u8> {
    let request = format!(
        "{{\"tool\":\"service:{}/{}\",\"arguments\":{}}}",
        crate::json::escape(service),
        crate::json::escape(capability),
        arguments_json,
    );
    invoke_tool(&request)
}
