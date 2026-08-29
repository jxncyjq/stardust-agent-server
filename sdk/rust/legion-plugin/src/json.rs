//! 够用就好的 JSON：解析宿主发来的工具调用，序列化插件答回去的结果。
//!
//! 这不是一个通用 JSON 库，也不打算变成一个。它只处理 ABI 契约里实际出现的两
//! 种文档：
//!
//!   - `{"call_id":"…","tool":"…","arguments":{"k":"v",…}}` —— 宿主发来的调用，
//!     `arguments` 在宿主侧的类型是 `map[string]string`（见
//!     `internal/plugin/host.guestToolCall`），所以每个值一定是带引号的字符串；
//!   - `{"call_id":"","success":bool,"output":"…","error":"…"}` —— 答回去的
//!     `domain.ToolResult`。
//!
//! 之所以不用 serde：SDK 是每个插件都要带上的东西，而插件包必须能离线构建。
//! 作者需要复杂 JSON 时在自己的 crate 里加 serde 即可，SDK 不挡路。

use std::collections::BTreeMap;

/// escape 转义 JSON 字符串里必须转义的字符。
///
/// 控制字符走 `\u00XX`，因为一个未转义的换行会让宿主的严格解码失败——而那次
/// 失败会被算成插件的 ABI 故障，代价是插件的健康度，不只是这一次调用。
pub fn escape(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    for ch in value.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out
}

/// 解析失败的原因。`Display` 出来的文本会进 `ToolResult::fail` 的 message，所以
/// 每一条都要说清是哪里不对，而不是「invalid json」。
#[derive(Debug, PartialEq, Eq)]
pub enum ParseError {
    /// 整个文档不是一个 JSON 对象。
    NotAnObject,
    /// 某个字符串字面量没有结束引号，或转义序列不完整。
    UnterminatedString,
    /// 契约要求的字段缺失或为空。
    MissingField(&'static str),
}

impl std::fmt::Display for ParseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ParseError::NotAnObject => write!(f, "body is not a JSON object"),
            ParseError::UnterminatedString => write!(f, "unterminated JSON string"),
            ParseError::MissingField(name) => write!(f, "missing required field {name:?}"),
        }
    }
}

/// 一个扁平 JSON 对象里的顶层字符串字段，以及（若有）`arguments` 子对象。
///
/// 返回 `BTreeMap` 而不是 `HashMap`：顺序确定，测试断言与日志才可复现。
pub struct Object {
    pub strings: BTreeMap<String, String>,
    pub arguments: BTreeMap<String, String>,
    /// bools 是顶层的布尔字段。
    ///
    /// 它存在只因为宿主真的会发一个：观察文档里的 `success`。其余非字符串标量
    /// 仍然跳过——见 [`parse`] 的说明。
    pub bools: BTreeMap<String, bool>,
}

/// parse 读一个工具调用文档。
///
/// 它认得的结构是刻意受限的：顶层的字符串字段，加一个名为 `arguments` 的、值
/// 全为字符串的子对象。遇到别的类型（数字、数组、嵌套对象）会跳过而不是报错
/// ——宿主不会发这些，而一个因为多了一个字段就整体失败的解析器，会把宿主日后
/// 增加的可选字段变成插件的故障。
pub fn parse(body: &[u8]) -> Result<Object, ParseError> {
    let text = std::str::from_utf8(body).map_err(|_| ParseError::NotAnObject)?;
    let bytes = text.as_bytes();
    let mut i = skip_ws(bytes, 0);
    if i >= bytes.len() || bytes[i] != b'{' {
        return Err(ParseError::NotAnObject);
    }
    i += 1;

    let mut object = Object {
        strings: BTreeMap::new(),
        arguments: BTreeMap::new(),
        bools: BTreeMap::new(),
    };
    loop {
        i = skip_ws(bytes, i);
        if i >= bytes.len() {
            return Err(ParseError::UnterminatedString);
        }
        if bytes[i] == b'}' {
            return Ok(object);
        }
        if bytes[i] == b',' {
            i += 1;
            continue;
        }
        if bytes[i] != b'"' {
            return Err(ParseError::NotAnObject);
        }
        let (key, next) = read_string(bytes, i)?;
        i = skip_ws(bytes, next);
        if i >= bytes.len() || bytes[i] != b':' {
            return Err(ParseError::NotAnObject);
        }
        i = skip_ws(bytes, i + 1);
        if i >= bytes.len() {
            return Err(ParseError::UnterminatedString);
        }
        match bytes[i] {
            b'"' => {
                let (value, next) = read_string(bytes, i)?;
                object.strings.insert(key, value);
                i = next;
            }
            b'{' if key == "arguments" => {
                let (args, next) = read_flat_object(bytes, i)?;
                object.arguments = args;
                i = next;
            }
            b't' | b'f' => match read_bool(bytes, i) {
                Some((value, next)) => {
                    object.bools.insert(key, value);
                    i = next;
                }
                // 不是 true/false 的其它 t/f 开头 token 交给通用跳过，理由与
                // 下面那条一样：宿主日后加的字段不该把插件变成故障。
                None => i = skip_value(bytes, i)?,
            },
            _ => i = skip_value(bytes, i)?,
        }
    }
}

/// read_bool 读 `true` / `false` 字面量，返回它的值与其后的下标。
///
/// 返回 `None`（而不是错误）表示这里不是布尔字面量：调用方会退回通用的跳过路
/// 径，所以一个本 SDK 不认识的 token 仍然不会让整份文档解析失败。
fn read_bool(bytes: &[u8], i: usize) -> Option<(bool, usize)> {
    if bytes[i..].starts_with(b"true") {
        return Some((true, i + 4));
    }
    if bytes[i..].starts_with(b"false") {
        return Some((false, i + 5));
    }
    None
}

fn skip_ws(bytes: &[u8], mut i: usize) -> usize {
    while i < bytes.len() && (bytes[i] as char).is_ascii_whitespace() {
        i += 1;
    }
    i
}

/// read_string 从 `bytes[start]`（必须是 `"`）读一个字符串字面量，返回它的值与
/// 结束引号之后的下标。
fn read_string(bytes: &[u8], start: usize) -> Result<(String, usize), ParseError> {
    let mut i = start + 1;
    let mut out = String::new();
    while i < bytes.len() {
        match bytes[i] {
            b'"' => return Ok((out, i + 1)),
            b'\\' => {
                i += 1;
                if i >= bytes.len() {
                    return Err(ParseError::UnterminatedString);
                }
                match bytes[i] {
                    b'n' => out.push('\n'),
                    b'r' => out.push('\r'),
                    b't' => out.push('\t'),
                    b'u' => {
                        // \uXXXX：只处理 BMP，代理对按字面留下——宿主发来的是
                        // 工具参数，不是任意文本，而错误的重组比留下原样更糟。
                        if i + 4 >= bytes.len() {
                            return Err(ParseError::UnterminatedString);
                        }
                        let hex = std::str::from_utf8(&bytes[i + 1..i + 5])
                            .map_err(|_| ParseError::UnterminatedString)?;
                        let code =
                            u32::from_str_radix(hex, 16).map_err(|_| ParseError::UnterminatedString)?;
                        out.push(char::from_u32(code).unwrap_or('\u{fffd}'));
                        i += 4;
                    }
                    other => out.push(other as char),
                }
                i += 1;
            }
            _ => {
                // UTF-8 多字节序列按字节收集：切片一定落在字符边界上，因为
                // 只有 ASCII 的引号和反斜杠会终止循环。
                let start_ch = i;
                let len = utf8_len(bytes[i]);
                i += len;
                if i > bytes.len() {
                    return Err(ParseError::UnterminatedString);
                }
                out.push_str(
                    std::str::from_utf8(&bytes[start_ch..i]).map_err(|_| ParseError::UnterminatedString)?,
                );
            }
        }
    }
    Err(ParseError::UnterminatedString)
}

fn utf8_len(first: u8) -> usize {
    match first {
        0x00..=0x7F => 1,
        0xC0..=0xDF => 2,
        0xE0..=0xEF => 3,
        _ => 4,
    }
}

/// read_flat_object 读 `arguments` 那种「字符串到字符串」的对象。
fn read_flat_object(bytes: &[u8], start: usize) -> Result<(BTreeMap<String, String>, usize), ParseError> {
    let mut map = BTreeMap::new();
    let mut i = start + 1;
    loop {
        i = skip_ws(bytes, i);
        if i >= bytes.len() {
            return Err(ParseError::UnterminatedString);
        }
        match bytes[i] {
            b'}' => return Ok((map, i + 1)),
            b',' => i += 1,
            b'"' => {
                let (key, next) = read_string(bytes, i)?;
                i = skip_ws(bytes, next);
                if i >= bytes.len() || bytes[i] != b':' {
                    return Err(ParseError::NotAnObject);
                }
                i = skip_ws(bytes, i + 1);
                if i >= bytes.len() {
                    return Err(ParseError::UnterminatedString);
                }
                if bytes[i] == b'"' {
                    let (value, next) = read_string(bytes, i)?;
                    map.insert(key, value);
                    i = next;
                } else {
                    i = skip_value(bytes, i)?;
                }
            }
            _ => return Err(ParseError::NotAnObject),
        }
    }
}

/// skip_value 跳过一个本 SDK 不消费的值（数字、布尔、null、数组、嵌套对象）。
fn skip_value(bytes: &[u8], start: usize) -> Result<usize, ParseError> {
    let mut i = start;
    let mut depth = 0usize;
    while i < bytes.len() {
        match bytes[i] {
            b'{' | b'[' => depth += 1,
            b'}' | b']' => {
                if depth == 0 {
                    return Ok(i);
                }
                depth -= 1;
                if depth == 0 {
                    return Ok(i + 1);
                }
            }
            b',' if depth == 0 => return Ok(i),
            b'"' => {
                let (_, next) = read_string(bytes, i)?;
                i = next;
                continue;
            }
            _ => {}
        }
        i += 1;
    }
    Err(ParseError::UnterminatedString)
}
