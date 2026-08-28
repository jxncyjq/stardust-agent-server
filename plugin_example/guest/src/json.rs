//! EXAMPLE-ONLY：手搓的 JSON 读写，**不要抄进真插件**。
//!
//! 它存在的唯一理由是让这个示例零依赖、能 `cargo build --offline`。真插件请
//! 用 `serde_json`：
//!
//! ```toml
//! [dependencies]
//! serde = { version = "1", features = ["derive"] }
//! serde_json = "1"
//! ```
//!
//! ```ignore
//! #[derive(serde::Deserialize)]
//! struct ToolCall { call_id: String, tool: String, arguments: HashMap<String, String> }
//! ```
//!
//! 并且**解码失败要返回失败的 ToolResult，不要 panic**——panic 会带走整个 wasm
//! 模块（连同实例里的状态和别的在途调用），而失败结果只是模型能读懂的一个答案。

/// extract_string_field 在一段扁平 JSON 里找 `"<key>":"` 并取到下一个引号。
///
/// 它不是 JSON 解析器：不认嵌套、不认转义、不认重复键。之所以对本示例够用，是
/// 因为宿主发来的 `arguments` 是**字符串到字符串**的 map（见 host.guestToolCall），
/// 所以它要读的值确实都是带引号的字符串。
pub fn extract_string_field(body: &[u8], key: &str) -> Option<String> {
    let text = std::str::from_utf8(body).ok()?;
    let needle = format!("\"{}\":\"", key);
    let start = text.find(&needle)? + needle.len();
    let rest = &text[start..];
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

/// escape 处理两个会破坏手拼 JSON 字符串的字符。用 serde_json 就不需要它。
pub fn escape(value: &str) -> String {
    value.replace('\\', "\\\\").replace('"', "\\\"")
}
