# 插件 Guest SDK 实施计划（G2）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让写一个插件不再需要手写 ABI——分配器、指针打包、host 函数声明、JSON 全部由 SDK 承担，作者只写工具本身。

**Architecture:** 两个 SDK，同一套形状。`sdk/rust/legion-plugin`（Rust crate）与 `pkg/legionplugin`（标准 Go，`GOOS=wasip1` + `//go:wasmexport`）各自导出 ABI 要求的四个函数并做分发，把「注册一个工具」变成一次函数调用。能力用 feature（Rust）/ build tag（Go）关着，保持「只 import 用得上的」这条硬规则。`plugin_example/guest` 迁到 Rust crate 上，作为 SDK 的第一个消费者。

**Tech Stack:** Rust（`wasm32-wasip1`，无第三方依赖）；Go 1.26.0（`GOOS=wasip1 GOARCH=wasm -buildmode=c-shared`，标准库）；wazero 侧不改。

**上游依据:** 设计文档 §6.3（ABI v1）、§6.10（guest SDK）；路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G2。

## 已验证的前置事实（2026-08-28 实跑）

Go guest 这条路能走通，且**不需要**任何第三方工具链：

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o spike.wasm .
```

产出 1.87 MB 的模块，导出恰好是 `_initialize` / `plugin_alloc` / `plugin_free` / `plugin_invoke` 加线性内存，`host.NewInstance` 能实例化，op 0 能答出自述。**唯一的坑**：Go 有 GC，`plugin_alloc` 返回的缓冲区必须被 SDK 自己持有引用直到 `plugin_free`，否则宿主写入的是一块可能已被回收的内存。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常。
- 公开标识符必须有文档注释（Go doc 风格 / Rust `///`），且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**（不是包子集）。
- SDK 本身**不引入任何第三方依赖**：Rust crate 零 dependencies，Go SDK 只用标准库。示例仍要能 `cargo build --offline`。
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，输出留在报告里，然后还原并 `git status` 核对。
- `plugin_example` 的四个测试是本期的回归网：任何 task 结束时 `go test ./plugin_example/...` 必须绿。
- 提交只 stage 本 task 自己的文件（显式路径），**永不 `git add -A`**。

---

### Task 1: Rust crate `legion-plugin`

**Files:**
- Create: `sdk/rust/legion-plugin/Cargo.toml`
- Create: `sdk/rust/legion-plugin/src/lib.rs`（导出面 + 文档）
- Create: `sdk/rust/legion-plugin/src/abi.rs`（alloc/free/打包解包）
- Create: `sdk/rust/legion-plugin/src/host.rs`（七个 host 函数，六个 feature 关着）
- Create: `sdk/rust/legion-plugin/src/tool.rs`（工具注册与分发、ToolResult 构造）
- Create: `sdk/rust/legion-plugin/README.md`

**Interfaces:**
- Produces（作者写插件时只碰这四样）：
  - `legion_plugin::declare_plugin!(name = "…", version = "…", tools = [ (name, handler) , … ])`
  - `type ToolHandler = fn(&ToolCall) -> ToolResult`
  - `struct ToolCall { pub call_id: String, pub tool: String, pub arguments: BTreeMap<String, String> }`
  - `ToolResult::ok(output: impl Into<String>) / ToolResult::fail(message: impl Into<String>)`
  - `legion_plugin::log_info(&str)`（以及 feature 后面的 `config()` / `kv_read` / `kv_write` / `http` / `read_file` / `call_tool`）

宏 `declare_plugin!` 负责生成四个 `#[no_mangle]` 导出与 op 分发，包括 op 0 的自述（`provides` 直接由 `tools` 列表推导——这消灭了 `plugin_example` 里「三处联动」中最容易忘的一处）。

**JSON**：crate 零依赖，所以自带一个**只够用**的 JSON：解析 `{"call_id":…,"tool":…,"arguments":{扁平字符串映射}}`（宿主发的就是这一种形状，`arguments` 是 `map[string]string`），序列化 `ToolResult` 四个字段。它是 crate 的实现细节，不导出——作者要更复杂的 JSON 就在自己的 crate 里加 serde。

- [ ] **Step 1: 建骨架并写第一个失败测试**

Rust 侧的测试用 `cargo test` 跑**宿主无关**的部分（JSON 与 ToolResult），ABI 与分发由 Task 2 的真宿主测试覆盖——一个在 x86 上跑的单测证明不了 wasm 导出。

`sdk/rust/legion-plugin/Cargo.toml`：

```toml
[package]
name = "legion-plugin"
version = "0.1.0"
edition = "2021"
publish = false
description = "Guest SDK for Legion Agent WASM plugins (ABI v1)"

[lib]
crate-type = ["rlib"]

[features]
default = []
config-capability = []
kv-capability = []
http-capability = []
fs-capability = []
tool-capability = []
```

`src/tool.rs` 里先写测试：

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_tool_call_with_arguments() {
        let call = ToolCall::parse(br#"{"call_id":"c1","tool":"hello","arguments":{"name":"legion"}}"#)
            .expect("a well-formed call must parse");
        assert_eq!(call.call_id, "c1");
        assert_eq!(call.tool, "hello");
        assert_eq!(call.arguments.get("name").map(String::as_str), Some("legion"));
    }

    #[test]
    fn a_missing_tool_field_is_an_error_not_an_empty_name() {
        // 空工具名会被分发当成"未知工具"，那是一句误导人的错误信息。
        assert!(ToolCall::parse(br#"{"call_id":"c1","arguments":{}}"#).is_err());
    }

    #[test]
    fn escapes_quotes_and_backslashes_in_results() {
        let json = ToolResult::ok(r#"say "hi"\ok"#).to_json();
        assert!(json.contains(r#"say \"hi\"\\ok"#), "got {json}");
    }

    #[test]
    fn a_failed_result_carries_the_message_and_success_false() {
        let json = ToolResult::fail("missing required argument: name").to_json();
        assert!(json.contains(r#""success":false"#), "got {json}");
        assert!(json.contains("missing required argument: name"), "got {json}");
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd sdk/rust/legion-plugin && cargo test --offline`
Expected: FAIL，`cannot find type ToolCall`

- [ ] **Step 3: 实现 crate**

`src/abi.rs`（照 `plugin_example/guest/src/abi.rs` 现有实现搬，它已经是对的，加上 `take_host_body`）；`src/tool.rs` 实现 `ToolCall::parse` / `ToolResult`；`src/host.rs` 七个 host 函数与安全包装（feature 门）；`src/lib.rs` 定义 `declare_plugin!`：

```rust
#[macro_export]
macro_rules! declare_plugin {
    (name = $name:expr, version = $version:expr, tools = [$(($tool:expr, $handler:expr)),* $(,)?]) => {
        #[no_mangle]
        pub extern "C" fn _initialize() {}

        #[no_mangle]
        pub extern "C" fn plugin_alloc(size: i32) -> i32 { $crate::abi::alloc(size) }

        #[no_mangle]
        pub extern "C" fn plugin_free(ptr: i32, size: i32) { $crate::abi::free(ptr, size) }

        #[no_mangle]
        pub extern "C" fn plugin_invoke(op: i32, ptr: i32, len: i32) -> i64 {
            match op {
                0 => {
                    // provides 由工具列表推导：自述与注册表不可能对不上。
                    let provides: &[&str] = &[$($tool),*];
                    $crate::abi::write_out($crate::manifest_json($name, $version, provides).as_bytes())
                }
                1 => {
                    let body = unsafe { $crate::abi::read_in(ptr, len) };
                    let result = match $crate::tool::ToolCall::parse(body) {
                        Ok(call) => match call.tool.as_str() {
                            $($tool => { let h: $crate::tool::ToolHandler = $handler; h(&call) })*
                            other => $crate::tool::ToolResult::fail(&format!("unknown tool: {other}")),
                        },
                        Err(e) => $crate::tool::ToolResult::fail(&format!("bad tool call: {e}")),
                    };
                    $crate::abi::write_out(result.to_json().as_bytes())
                }
                _ => $crate::abi::write_out(br#"{"error":"unsupported op"}"#),
            }
        }
    };
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd sdk/rust/legion-plugin && cargo test --offline`
Expected: PASS（4 个）

- [ ] **Step 5: 六个 feature 各自编译干净**

Run（在 crate 目录）：

```bash
for f in config-capability kv-capability http-capability fs-capability tool-capability; do
  cargo build --offline --target wasm32-wasip1 --features "$f" || exit 1
done
cargo build --offline --target wasm32-wasip1
```

Expected: 全部成功，**零 warning**（未被使用的 feature 包装函数加 `#[allow(dead_code)]` 并注明原因，`plugin_example` 已有先例）。

- [ ] **Step 6: 提交**

```bash
git add sdk/rust/legion-plugin
git commit -m "feat(sdk): Rust guest SDK legion-plugin（ABI v1）"
```

---

### Task 2: `plugin_example/guest` 迁到 crate 上

**Files:**
- Modify: `plugin_example/guest/Cargo.toml`（依赖 `legion-plugin`，删掉自己的 feature 定义改为透传）
- Rewrite: `plugin_example/guest/src/lib.rs`（只剩 `declare_plugin!` 与工具实现）
- Delete: `plugin_example/guest/src/abi.rs`、`src/host.rs`、`src/json.rs`、`src/tools.rs`
- Modify: `plugin_example/README.md`（目录说明改成 SDK 版）
- Regenerate: `plugin_example/package/plugin.json` + `plugin.wasm`（跑 `scripts/build.sh`）

**Interfaces:**
- Consumes: Task 1 的 `declare_plugin!` / `ToolCall` / `ToolResult` / `log_info`

这一步同时是 Task 1 的**验收**：示例现有的四个 Go 测试（清单与 wasm 配套、op 0 自述覆盖声明工具、闭环带 log 回调、未授权即链接失败）一个都不许改，全绿才算 SDK 是对的。

- [ ] **Step 1: 改 Cargo.toml**

```toml
[dependencies]
legion-plugin = { path = "../../sdk/rust/legion-plugin" }

[features]
default = []
kv-capability = ["legion-plugin/kv-capability"]
http-capability = ["legion-plugin/http-capability"]
# …其余四个同形
```

- [ ] **Step 2: 重写 lib.rs**

```rust
//! legion-hello：用 SDK 写的最小插件。
//!
//! 与 SDK 之前的手写版相比，消失的是四个文件（abi/host/json/tools）与
//! 「三处联动」里的一处：`provides` 现在由 `declare_plugin!` 从工具列表推导。

use legion_plugin::{declare_plugin, log_info, ToolCall, ToolResult};

declare_plugin!(
    name = "legion-hello",
    version = "0.1.0",
    tools = [("hello_echo", hello_echo)]
);

/// hello_echo 读入参 `name`，经 `log` 能力回调宿主写一行日志，再把问候语
/// 作为结果返回。缺 `name` 时返回**失败结果**并点名缺了哪个参数——不要 panic，
/// panic 会带走整个 wasm 模块。
fn hello_echo(call: &ToolCall) -> ToolResult {
    match call.arguments.get("name") {
        Some(name) if !name.is_empty() => {
            log_info(&format!("hello_echo called with name={name}"));
            ToolResult::ok(format!("hello, {name}!"))
        }
        _ => ToolResult::fail("missing required argument: name"),
    }
}
```

- [ ] **Step 3: 重建包并跑既有测试**

```bash
bash plugin_example/scripts/build.sh
go test ./plugin_example/...
```

Expected: 四个测试全绿。digest 会变（wasm 变了），`build.sh` 已经负责重新渲染 `plugin.json`。

- [ ] **Step 4: 变异验证**

把 `declare_plugin!` 里的工具名改成 `"hello_echo_typo"`，重建，确认
`TestExampleGuestSelfDescriptionCoversItsDeclaredTools` FAIL（自述与清单对不上），
贴输出后还原重建。

- [ ] **Step 5: 更新 README 并提交**

README 的目录树与「改成你自己的插件」两节改成 SDK 版：加工具从「三处联动」降为**两处**（`declare_plugin!` 的列表 + `plugin.json` 的 `tools`）。

```bash
git add plugin_example
git commit -m "refactor(plugin_example): 迁到 legion-plugin SDK 上"
```

---

### Task 3: Go guest SDK `pkg/legionplugin`

**Files:**
- Create: `pkg/legionplugin/plugin.go`（注册表、op 分发、自述）
- Create: `pkg/legionplugin/abi_wasip1.go`（`//go:build wasip1`：四个导出与内存管理）
- Create: `pkg/legionplugin/host_wasip1.go`（`//go:build wasip1`：host 函数声明与包装）
- Create: `pkg/legionplugin/tool.go`（`ToolCall` / `ToolResult`，与平台无关，可在本机单测）
- Create: `pkg/legionplugin/tool_test.go`
- Create: `pkg/legionplugin/testdata/hello/main.go`（示例 guest，被 Task 3 的集成测试构建）
- Create: `pkg/legionplugin/guest_test.go`（构建 + 真宿主跑通）
- Create: `pkg/legionplugin/README.md`

**Interfaces:**
- Produces:
  - `func legionplugin.Register(name string, handler Handler)`
  - `type Handler func(ToolCall) ToolResult`
  - `func legionplugin.Serve(name, version string)`（在 `main()` 里调一次，装配自述并接管分发）
  - `func legionplugin.LogInfo(msg string)` 等能力包装

**GC 是这里唯一的真问题**：`plugin_alloc` 返回的缓冲区必须被 SDK 持有到 `plugin_free`，否则 Go 的 GC 会在宿主写入前回收它。SDK 用一个 `map[uintptr][]byte` 持有，`plugin_free` 删除条目。**这一条要有测试**（见 Step 3 的 `TestGoGuestSurvivesAGarbageCollection`）。

- [ ] **Step 1: 写平台无关部分的失败测试**

`pkg/legionplugin/tool_test.go`：

```go
func TestToolCallParsesArguments(t *testing.T) {
	call, err := parseToolCall([]byte(`{"call_id":"c1","tool":"hello","arguments":{"name":"legion"}}`))
	if err != nil {
		t.Fatalf("parseToolCall: %v", err)
	}
	if call.Tool != "hello" || call.Arguments["name"] != "legion" {
		t.Errorf("parsed %+v, want tool=hello name=legion", call)
	}
}

func TestToolCallWithoutAToolNameIsAnError(t *testing.T) {
	if _, err := parseToolCall([]byte(`{"call_id":"c1","arguments":{}}`)); err == nil {
		t.Fatal("parseToolCall with no tool = nil error, want a refusal: an empty tool name " +
			"would be dispatched as 'unknown tool', which blames the caller for the SDK's silence")
	}
}

func TestFailResultCarriesTheMessage(t *testing.T) {
	body, err := Fail("missing required argument: name").encode("c1")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"success":false`) ||
		!strings.Contains(string(body), "missing required argument: name") {
		t.Errorf("encoded %s, want success:false and the message", body)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./pkg/legionplugin/ -run TestTool -v`
Expected: FAIL，`undefined: parseToolCall`

- [ ] **Step 3: 实现 SDK**

平台无关部分（`tool.go`、`plugin.go` 的注册表与分发）用 `encoding/json`——Go 侧有标准库，没有零依赖的理由去手搓。

`abi_wasip1.go` 带 `//go:build wasip1`，四个导出用 `//go:wasmexport`；分配表：

```go
// live keeps every buffer plugin_alloc handed out alive until plugin_free
// releases it. Without it Go's GC is free to collect a buffer whose only
// reference is the integer address the host holds, and the host would then
// write into memory that has been recycled — the one failure mode a Go guest
// has and a Rust guest does not.
var live = map[uintptr][]byte{}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./pkg/legionplugin/ -run TestTool -v`
Expected: PASS（3 个）

- [ ] **Step 5: 示例 guest + 真宿主集成测试**

`testdata/hello/main.go`：

```go
package main

import "github.com/stardust/legion-agent/pkg/legionplugin"

func main() {
	legionplugin.Register("hello_echo", func(call legionplugin.ToolCall) legionplugin.ToolResult {
		name := call.Arguments["name"]
		if name == "" {
			return legionplugin.Fail("missing required argument: name")
		}
		legionplugin.LogInfo("hello_echo called with name=" + name)
		return legionplugin.OK("hello, " + name + "!")
	})
	legionplugin.Serve("legion-hello-go", "0.1.0")
}
```

`guest_test.go` 构建它并通过 `internal/plugin/host` 跑：

```go
// buildGuest compiles testdata/hello for wasip1. It builds rather than commits
// the artifact: a Go guest is ~1.9 MB, and a committed one would go stale the
// moment the SDK changed — which is exactly when this test needs to be honest.
func buildGuest(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hello.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./testdata/hello")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wasip1 guest: %v\n%s", err, output)
	}
	wasm, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read built guest: %v", err)
	}
	return wasm
}

func TestGoGuestAnswersManifestAndToolCall(t *testing.T) {
	// 与 plugin_example 的闭环测试同形：op 0 的 provides 覆盖注册的工具，
	// op 1 返回成功的 ToolResult，并且经 log 能力回调了宿主。
}

func TestGoGuestSurvivesAGarbageCollection(t *testing.T) {
	// 分配、runtime.GC()、再让宿主写入并读回，证明 live 表确实按住了缓冲区。
	// 没有这条，GC 相关的 bug 只会在高负载的生产里偶发。
}
```

两个测试的完整实现照 `plugin_example/example_test.go` 的骨架写（`host.NewRuntime` → `Compile` → `CheckImports` → `BuildHostModule` → `NewInstance` → `Invoke`），断言同形。

- [ ] **Step 6: 跑通并全量**

Run: `go test ./pkg/legionplugin/ -v` → PASS
Run: `go test ./...` → 全绿

- [ ] **Step 7: 变异验证**

把 `live` 表的 `delete` 改成清空整张表（`live = map[uintptr][]byte{}`），确认
`TestGoGuestSurvivesAGarbageCollection` 或闭环测试 FAIL；还原。

- [ ] **Step 8: 提交**

```bash
git add pkg/legionplugin
git commit -m "feat(sdk): Go guest SDK pkg/legionplugin（wasip1 + //go:wasmexport）"
```

---

### Task 4: 文档

**Files:**
- Modify: docs 仓 `agents/reference/reference-legion-agent-plugins-001.md`（§3 增「用 SDK 写」，手写 ABI 降为附录性质的「底层合同」）
- Modify: docs 仓 `design/architecture/legion-plugin-system.md`（§9 路线表 G2 标记已交付）
- Modify: `plugin_example/README.md`（若 Task 2 未覆盖到的部分）

- [ ] **Step 1: 手册**

§3 开头加一句「**先看这个**：两个 SDK 各有一节，手写 ABI 只在你要写第三种语言时才需要」，随后给 Rust 与 Go 各一段最小可编译示例（就是 Task 2、Task 3 的那两个），并写明能力 feature/build tag 与 `plugin.json` 的 `capabilities` 必须一致。

- [ ] **Step 2: 路线表 + 提交（docs 仓单独分支与 PR）**

---

## 自检

**范围覆盖**：roadmap 的 G2 三件事——`pkg/legionplugin`（Task 3）、`sdk/rust` crate（Task 1）、`plugin_example` 迁移（Task 2）——各有任务；文档在 Task 4。

**类型一致性**：Rust 侧 `ToolCall{call_id,tool,arguments}` / `ToolResult::ok|fail` 在 Task 1 定义、Task 2 使用；Go 侧 `ToolCall`/`ToolResult`/`Register`/`Serve`/`OK`/`Fail` 在 Task 3 内自洽，命名与 Rust 侧同义（Go 用大写导出名，Rust 用 snake_case，各随语言习惯）。

**已知留白**：Task 3 Step 5 两个集成测试只给了骨架与断言意图，实现照 `plugin_example/example_test.go` 抄——那份是已验证可用的模板，重写一遍反而会漂移。这是**指向现有代码**，不是 placeholder。

**风险**：Go guest 1.9 MB。这是标准 Go 的事实（设计文档 §6.1 已记录，并写明体积敏感就用 Rust）。SDK 不试图解决它，README 里写明两种 SDK 的取舍即可。
