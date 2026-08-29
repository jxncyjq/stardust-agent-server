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

/// ToolObservation 是宿主在一次工具调用**答完之后**告诉插件的那件事：调用本身
/// 与它的结果，一起给。
///
/// 只有被授予 `observe` 扩展点的插件会收到它，而且 `tool` 是 agent 跑过的任意
/// 工具，不限于本插件自己的——这正是这个 seam 的用途。
///
/// 它是单向的：观察者没有返回值，宿主会丢弃 op 2 的应答，所以插件在这里改不了
/// 任何东西。调用方拿到结果那一刻就已经定了。
pub struct ToolObservation {
    pub call_id: String,
    pub tool: String,
    pub arguments: BTreeMap<String, String>,
    pub success: bool,
    pub output: String,
    pub error: String,
}

impl ToolObservation {
    /// parse 读一次 `abi::OP_OBSERVE_TOOL_RESULT` 的请求体。
    ///
    /// 缺 `tool` 或缺 `success` 都是错误，不是默认值：一个不知道自己在看哪个
    /// 工具、或者猜一个 `success=false` 的观察者，会把「宿主发来的文档变了」
    /// 记成「那个工具失败了」——那是最难查的一类脏数据。
    pub fn parse(body: &[u8]) -> Result<ToolObservation, json::ParseError> {
        let object = json::parse(body)?;
        let tool = object
            .strings
            .get("tool")
            .filter(|value| !value.is_empty())
            .ok_or(json::ParseError::MissingField("tool"))?
            .clone();
        let success = *object
            .bools
            .get("success")
            .ok_or(json::ParseError::MissingField("success"))?;
        Ok(ToolObservation {
            call_id: object.strings.get("call_id").cloned().unwrap_or_default(),
            tool,
            arguments: object.arguments,
            success,
            output: object.strings.get("output").cloned().unwrap_or_default(),
            error: object.strings.get("error").cloned().unwrap_or_default(),
        })
    }
}

/// Observer 是观察者的实现：拿到一次已完成调用，**什么也不返回**。
///
/// 没有返回值是契约本身，不是省略。另外它必须**快**：它跑在别人的工具调用里，
/// 宿主给每次通知 200ms 的上限，超时会记进本插件的健康度，连续失败足够多次会
/// 被卸载。贵的活儿留到自己的下一次调用里做。
pub type Observer = fn(&ToolObservation);

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

#[cfg(test)]
mod observation_tests {
    use super::*;

    #[test]
    fn parses_an_observation_with_its_result() {
        let observation = ToolObservation::parse(
            br#"{"call_id":"c1","tool":"write_file","arguments":{"path":"/tmp/x"},"success":true,"output":"wrote 3 bytes","error":""}"#,
        )
        .expect("a well-formed observation must parse");
        assert_eq!(observation.call_id, "c1");
        assert_eq!(observation.tool, "write_file");
        assert_eq!(observation.arguments.get("path").map(String::as_str), Some("/tmp/x"));
        assert!(observation.success);
        assert_eq!(observation.output, "wrote 3 bytes");
    }

    #[test]
    fn parses_a_failed_observation() {
        let observation =
            ToolObservation::parse(br#"{"tool":"read_file","success":false,"error":"no such file"}"#)
                .expect("parse");
        assert!(!observation.success, "success=false must not read as true");
        assert_eq!(observation.error, "no such file");
    }

    #[test]
    fn a_missing_success_field_is_an_error_not_a_guess() {
        assert_eq!(
            ToolObservation::parse(br#"{"tool":"read_file"}"#).err(),
            Some(json::ParseError::MissingField("success"))
        );
    }

    #[test]
    fn a_missing_tool_field_is_an_error() {
        assert_eq!(
            ToolObservation::parse(br#"{"success":true}"#).err(),
            Some(json::ParseError::MissingField("tool"))
        );
    }
}
