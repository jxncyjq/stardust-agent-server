# legion-plugin

写 Legion Agent WASM 插件的 Rust guest SDK（ABI v1）。零依赖，可 `cargo build --offline`。

## 用它

`Cargo.toml`：

```toml
[lib]
crate-type = ["cdylib"]

[dependencies]
legion-plugin = { path = "…/sdk/rust/legion-plugin" }
```

`src/lib.rs`：

```rust
use legion_plugin::{declare_plugin, log_info, ToolCall, ToolResult};

declare_plugin!(
    name = "legion-hello",
    version = "0.1.0",
    tools = [("hello_echo", hello_echo)]
);

fn hello_echo(call: &ToolCall) -> ToolResult {
    match call.argument("name") {
        Some(name) => {
            log_info(&format!("hello_echo called with name={name}"));
            ToolResult::ok(format!("hello, {name}!"))
        }
        None => ToolResult::fail("missing required argument: name"),
    }
}
```

构建：`cargo build --release --target wasm32-wasip1`。

SDK 生成四个导出（`_initialize` / `plugin_alloc` / `plugin_free` / `plugin_invoke`）、op 分发、内存管理、指针打包、JSON，以及 **op 0 的自述**——`provides` 由 `tools` 列表推导，所以「guest 说自己提供什么」和「实际注册了什么」不会对不上。

## 能力

默认只有 `log`。其余六个 host 函数用 feature 关着：

| 能力 | feature | 函数 |
|---|---|---|
| `log` | 默认开 | `log_info` |
| `config` | `config-capability` | `host::config` |
| `kv` | `kv-capability` | `host::kv_read` / `host::kv_write` |
| `http` | `http-capability` | `host::http` |
| `fs` | `fs-capability` | `host::read` |
| `tool` | `tool-capability` | `host::invoke_tool` |

打开一个 feature 要**同时**做另两件事，缺一模块就在实例化时失败（能力是链接期事实，不是运行期开关）：

1. `plugin.json` 的 `capabilities` 加上同名能力（`http`/`fs` 还要声明 `network.allowed_hosts` / `filesystem.allowed_paths`）；
2. 部署侧 `agent plugins grant --capabilities <完整集合>`。

## 扩展点：观察工具调用

能力是插件调宿主，**扩展点是宿主调插件**。当前只有一个：`observe`——每次工具调用
**答完之后**回调你一次。宏里多一行：

```rust
declare_plugin!(
    name = "legion-hello",
    version = "0.1.0",
    tools = [("hello_echo", hello_echo)],
    observe = log_observation
);

fn log_observation(o: &ToolObservation) {
    log_info(&format!("observed tool={} success={}", o.tool, o.success));
}
```

没有这一行，宏**不生成** op 2 那条分支（op 2 落到「未知 op」），op 0 的
`extensions` 也是空的——两者同源，作者不需要维护第二份清单。

同样是三处联动，失败点各不相同：

| 位置 | 缺了会怎样 |
|---|---|
| `observe = <fn>` | 部署授权了却没实现 → **激活期拒绝**（宿主拿 op 0 的 `extensions` 交叉校验） |
| `plugin.json` 的 `"extensions": ["observe"]` | `grant --extensions` 拒绝：没声明的授不了 |
| `agent plugins grant <name> --extensions observe` | 宿主不注册观察者，op 2 一次也不到达——**静默且正确**，这就是未授权的含义 |

四条边界，都不是可以商量的：

- **改不了任何东西**：观察者没有返回值，宿主丢弃 op 2 的应答。
- **只看得到跑起来并答了的调用**：被权限 / 策略 / 护栏拒掉的调用从不通知；`success:false` 会通知（工具跑了，答了「不行」）。
- **看到的是任意工具**，不只是本插件的。
- **每次 200ms**，跑在调用方的线程上；超时或 trap 计入本插件健康度。

### 决策点：在派发前否决一次调用

`decide` 是第二个扩展点，也是第一个**答案有后果**的。宏里再多一行：

```rust
declare_plugin!(
    name = "legion-gatekeeper",
    version = "0.1.0",
    tools = [("status", status)],
    observe = log_observation,   // 可选，可单独出现
    decide = decide_call         // 可选，可单独出现
);

fn decide_call(request: &ToolDecisionRequest) -> ToolDecision {
    if request.tool == "write_file" && frozen() {
        return ToolDecision::deny("writes are frozen during the incident");
    }
    ToolDecision::allow()
}
```

四条边界：

- **只能收紧。** 宿主自己的权限与策略先跑；它们拒掉的调用根本不会问到插件。`allow()` 是「我不反对」，不是授权。
- **答不出来就是拒绝**（fail-closed）：超时、trap、答出宿主解不了的东西都会拒掉那次调用并计入健康度。SDK 连「读不懂这次请求」也答 `deny` 并说明理由——宿主对读不懂的答案本就 fail-closed，说清理由只是让运维看得懂。
- **上限 `min(工具超时/4, 200ms)`**，比观察点更紧：工具还没开始跑。
- 没写 `decide = ...` 时宏**不生成** op 3 那条分支，op 0 的 `extensions` 里也没有它。

`ToolDecision::ask(reason)` 是第三种答案：不是拒绝，而是**要人批**。宿主在 round 边界挂起任务、开一张点名本插件与这条理由的票，人批了再从检查点继续；没有审批通道的部署按拒绝处理。它**不看模式**——Auto 模式下同样会停下来等人。

```rust
if request.tool == "deploy" {
    return ToolDecision::ask("deploys are reviewed by a human");
}
```

### 提示词段：往系统提示词里加一段文字

`prompt` 是第三个扩展点，也是唯一一个「不是调用」的：宿主在**激活时问一次**，答案在插件挂着期间进每一次推理。宏里再多一行（三个 seam 按 `observe` → `decide` → `prompt` 的顺序写）：

```rust
declare_plugin!(
    name = "legion-jira",
    version = "0.1.0",
    tools = [("jira_search", jira_search)],
    prompt = prompt_segment
);

fn prompt_segment() -> String {
    String::from("When citing a Jira issue, link it as https://jira.example.com/browse/KEY.")
}
```

四条边界：**只问一次**（可以读配置，但别每次说不一样的话——它待在稳定前缀里）；**带围栏**（`--- plugin "<名字>" (untrusted…) ---`）；**有上限**（单插件 2048 rune、合计 8192 rune，超长截断留痕）；**答不出来 = 插件挂不上**（空字符串则是合法的「没话说」）。

## 两条硬规矩

- **工具失败返回 `ToolResult::fail`，不要 panic。** panic 会 trap 整个模块，代价是实例状态、同实例的在途调用，而且计入插件健康度（连续故障到阈值会被自动卸载）。
- **初始化放 `_initialize`。** `declare_plugin!` 生成的是空实现；要做初始化就自己写一个 `_initialize` 并把宏那份去掉（把宏展开抄到自己文件里，改那一处）。放到首次调用里做会算进那次工具调用的超时预算。

## 与 Go SDK 的取舍

| | Rust（本 crate） | Go（`pkg/legionplugin`） |
|---|---|---|
| 产物体积 | ~40 KB | ~1.9 MB |
| 工具链 | `rustup target add wasm32-wasip1` | 只要 Go 1.24+ |
| GC | 无 | 有（SDK 内部按住分配的缓冲区） |

体积敏感（或部署要挂很多插件）用 Rust；团队只有 Go 就用 Go，1.9 MB 是标准 Go 运行时的代价，不是 SDK 的。

## 完整示例

`plugin_example/`：一个跑通过 install → grant → serve → `state:"loaded"` 全程的最小插件。
