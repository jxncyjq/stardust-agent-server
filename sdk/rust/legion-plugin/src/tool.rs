//! 工具调用与结果：插件作者唯一需要读懂的两个类型。

use crate::json;
use std::collections::BTreeMap;

/// ToolCall 是宿主交给插件的一次调用。
///
/// `call_id` 只用于把插件自己的日志和宿主的日志对上——**答回去时不要填它**，
/// 相关性 id 归宿主所有，它会覆盖插件返回的值（`ToolResult` 因此也没有这个字段）。
pub struct ToolCall {
    pub call_id: String,
    pub tool: String,
    /// arguments 是字符串到字符串的映射：宿主侧的类型就是 `map[string]string`，
    /// 所以数字、布尔在这里都是它们的字符串写法，由插件自己解析。
    pub arguments: BTreeMap<String, String>,
}

impl ToolCall {
    /// parse 读一次 `abi::OP_CALL_TOOL` 的请求体。
    ///
    /// 缺 `tool` 是错误而不是空名字：一个空工具名会被分发当成「未知工具」，那
    /// 句错误信息会把 SDK 的沉默说成调用方的错。
    pub fn parse(body: &[u8]) -> Result<ToolCall, json::ParseError> {
        let object = json::parse(body)?;
        let tool = object
            .strings
            .get("tool")
            .filter(|value| !value.is_empty())
            .ok_or(json::ParseError::MissingField("tool"))?
            .clone();
        Ok(ToolCall {
            call_id: object.strings.get("call_id").cloned().unwrap_or_default(),
            tool,
            arguments: object.arguments,
        })
    }

    /// argument 取一个必填参数，缺失或为空时返回 `None`。
    ///
    /// 空串与缺失同等对待是刻意的：一个只有空白的参数几乎总是调用方写错了，而
    /// 拿它继续跑出来的结果谁也解释不了。
    pub fn argument(&self, name: &str) -> Option<&str> {
        self.arguments
            .get(name)
            .map(String::as_str)
            .filter(|value| !value.is_empty())
    }
}

/// ToolResult 是插件答给宿主的东西，对应宿主侧的 `domain.ToolResult`。
///
/// **失败要用 `ToolResult::fail`，不要 panic**：panic 会 trap 整个 wasm 模块，
/// 代价是实例里的状态和同一实例上所有在途调用，而且会被记进插件的健康度；失败
/// 结果只是模型能读懂、能据此改口再试的一个答案。
pub struct ToolResult {
    success: bool,
    output: String,
    error: String,
}

impl ToolResult {
    /// ok 构造一个成功结果，output 是给模型看的内容。
    pub fn ok(output: impl Into<String>) -> ToolResult {
        ToolResult {
            success: true,
            output: output.into(),
            error: String::new(),
        }
    }

    /// fail 构造一个失败结果。message 要说清是什么不对——它会原样到达模型。
    pub fn fail(message: impl Into<String>) -> ToolResult {
        ToolResult {
            success: false,
            output: String::new(),
            error: message.into(),
        }
    }

    /// to_json 渲染宿主严格解码的那份文档。
    ///
    /// `call_id` 恒为空串：宿主拥有相关性 id 并会覆盖它（见 ToolCall::call_id）。
    /// 四个字段全部写出而不省略，因为宿主侧的 `domain.ToolResult` 没有
    /// `omitempty`，省略字段只会让两边对同一份文档的读法产生分歧。
    pub fn to_json(&self) -> String {
        format!(
            "{{\"call_id\":\"\",\"success\":{},\"output\":\"{}\",\"error\":\"{}\"}}",
            self.success,
            json::escape(&self.output),
            json::escape(&self.error)
        )
    }
}

/// ToolHandler 是一个工具的实现：拿到调用，给出结果。
pub type ToolHandler = fn(&ToolCall) -> ToolResult;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_tool_call_with_arguments() {
        let call = ToolCall::parse(br#"{"call_id":"c1","tool":"hello","arguments":{"name":"legion"}}"#)
            .expect("a well-formed call must parse");
        assert_eq!(call.call_id, "c1");
        assert_eq!(call.tool, "hello");
        assert_eq!(call.argument("name"), Some("legion"));
    }

    #[test]
    fn a_missing_tool_field_is_an_error_not_an_empty_name() {
        assert_eq!(
            ToolCall::parse(br#"{"call_id":"c1","arguments":{}}"#).err(),
            Some(json::ParseError::MissingField("tool"))
        );
    }

    #[test]
    fn an_empty_argument_reads_as_absent() {
        let call = ToolCall::parse(br#"{"tool":"hello","arguments":{"name":""}}"#).expect("parse");
        assert_eq!(call.argument("name"), None);
    }

    #[test]
    fn ignores_fields_this_sdk_does_not_consume() {
        // 宿主日后加一个可选字段，不该把每个插件都变成故障。
        let call = ToolCall::parse(br#"{"tool":"hello","deadline_ms":3000,"arguments":{"a":"b"}}"#)
            .expect("an unknown scalar field must not fail the parse");
        assert_eq!(call.tool, "hello");
        assert_eq!(call.argument("a"), Some("b"));
    }

    #[test]
    fn escapes_quotes_and_backslashes_in_results() {
        let json = ToolResult::ok("say \"hi\"\\ok").to_json();
        assert!(json.contains("say \\\"hi\\\"\\\\ok"), "got {json}");
    }

    #[test]
    fn escapes_newlines_so_the_host_can_decode_the_body() {
        let json = ToolResult::ok("line one\nline two").to_json();
        assert!(json.contains("line one\\nline two"), "got {json}");
        assert!(!json.contains('\n'), "a raw newline would break the host's strict decode: {json}");
    }

    #[test]
    fn a_failed_result_carries_the_message_and_success_false() {
        let json = ToolResult::fail("missing required argument: name").to_json();
        assert!(json.contains("\"success\":false"), "got {json}");
        assert!(json.contains("missing required argument: name"), "got {json}");
    }
}
