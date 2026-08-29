# plugin_example — 最小可跑通的插件

`legion-hello`：一个 WASM 插件的最小闭环示例。它贡献一个工具 `hello_echo`，
把入参里的 `name` 读出来、经 `log` 能力回调宿主写一行日志、再把
`domain.ToolResult` 还给宿主。

**这一份是跑过的**：`build → sign → package → install → grant → serve`，终点是
`GET /v1/plugins` 返回 `state:"loaded"`，`tools` 里有 `hello_echo`。

完整规范（ABI、`plugin.json` 全字段、发布与部署、状态机、排错）见文档仓
`agents/reference/reference-legion-agent-plugins-001.md`。本目录只讲**照着做**。

## 目录

```
guest/                Rust guest 源码，依赖 sdk/rust/legion-plugin
  Cargo.toml          crate-type=cdylib；能力 feature 透传给 SDK
  src/lib.rs          declare_plugin! + 工具实现 —— 整个插件就这一个文件
package/              插件包目录：plugin.json + plugin.wasm（均已提交）
scripts/build.sh      构建 wasm 并按其摘要重新渲染 plugin.json
scripts/publish.sh    用你自己的私钥签名，打成 dist/legion-hello.tar.gz
example_test.go       用真实 wazero 宿主跑这个包（`go test ./plugin_example/...`）
```

**一个文件**：ABI 的四个导出、op 分发、内存管理、指针打包与 JSON 全在
`sdk/rust/legion-plugin` 里，示例只剩 `declare_plugin!` 与一个工具函数。想看
底层合同长什么样，读 SDK 的 `src/abi.rs`；写插件不需要。

## 完整结构与刻意留空的槽位

这个示例只开了 `log` 一个能力，但**七个 host 函数的通路 SDK 里都已经写好**，
其余六个用 Cargo feature 关着（`sdk/rust/legion-plugin/src/host.rs`），每个槽位
的注释里写明了请求 / 响应形状与打开它的代价：

| 能力 | feature | 通路（SDK 的 `host` 模块） | 打开后还要做什么 |
|---|---|---|---|
| `log` | 默认开 | `log_info` | — |
| `config` | `config-capability` | `host::config()` | plugin.json 的 `capabilities` 加 `config` |
| `kv` | `kv-capability` | `host::kv_read` / `host::kv_write` | 同上加 `kv` |
| `http` | `http-capability` | `host::http()` | 加 `http`，**并声明 `network.allowed_hosts`** |
| `fs` | `fs-capability` | `host::read()` | 加 `fs`，**并声明 `filesystem.allowed_paths`** |
| `tool` | `tool-capability` | `host::invoke_tool()` | 加 `tool`；依赖别家工具还要写 `requires` |

**为什么默认关着**：能力是**链接期**事实。宿主只为已授权的能力注册 host 函数，
所以 import 一个部署没授权的函数，模块会在**实例化**时失败——guest 根本没机会
调用它，也不会收到一个 `DENIED` 去处理。于是「import 什么」必须与
`plugin.json` 的 `capabilities` 严格对齐，feature 让这件事变成一次显式动作：

```bash
# 三处联动，缺一处就装不上：
# 1) 构建时打开 feature（示例 crate 的 feature 透传给 SDK）
cargo build --release --target wasm32-wasip1 --features kv-capability
# 2) scripts/build.sh 里内联的 plugin.json：capabilities 加上 "kv"
# 3) 部署侧：agent plugins grant <name> --capabilities log,kv
```

加第二个工具是两处：`declare_plugin!` 的 `tools = [...]` 里加一项、
`scripts/build.sh` 内联的 `plugin.json` 的 `tools` 里加一条。**op 0 的
`provides` 不用管**——SDK 从 `tools` 列表推导，这正是它消灭掉的第三处联动。

用 `http` 通路的工具长这样（打开 `http-capability`、并在 plugin.json 里同时
声明 `capabilities:["log","http"]` 与 `network.allowed_hosts` 之后）：

```rust
fn fetch_title(call: &ToolCall) -> ToolResult {
    let Some(url) = call.argument("url") else {
        return ToolResult::fail("missing required argument: url");
    };
    let response = legion_plugin::host::http(&format!(
        "{{\"method\":\"GET\",\"url\":\"{}\"}}", url
    ));
    // 响应要么是 {"status":…,"body":…}，要么是错误信封
    // {"code":"DENIED"|"INVALID_REQUEST"|"HOST_ERROR","message":…}。两者都要判：
    // 把 DENIED 当成空 body 继续跑，就是在替越权的调用打掩护。
    ToolResult::ok(String::from_utf8_lossy(&response))
}
```

### 完整的 plugin.json 骨架

JSON 写不了注释，这里给一份把**所有**字段都摆出来的骨架（本示例实际提交的那份
只保留了用得上的部分）：

```jsonc
{
  "name": "legion-hello",          // 必填，与 op 0 自述里的 name 一致
  "version": "0.1.0",              // 必填
  "abi": 1,                        // 必须是 1
  "sha256": "<plugin.wasm 的 64 位十六进制摘要>",   // build.sh 负责填
  "capabilities": ["log"],         // 只写真正 import 了的：log/config/kv/http/fs/tool
  "extensions": ["observe"],       // 可选：本插件实现了哪些宿主扩展点（宿主调插件的方向）
                                   // 授权可以只取子集；授权了而 guest 没实现 = 激活期拒绝
  "limits": {
    "timeout_ms": 5000,            // 单次进插件调用的超时
    "max_memory_pages": 64,        // 不得为 0（一页 64 KiB）
    "max_instances": 2             // 至少 1
  },
  "network":    { "allowed_hosts": [] },   // 用 http 能力时必须非空
  "filesystem": { "allowed_paths": [] },   // 用 fs 能力时必须非空
  "requires":   [],                        // 本插件要 call_tool 调用的别家工具名
  "tools": [                       // 不得为空：插件的存在理由就是贡献工具
    {
      "name": "hello_echo",        // 必填、不得重名，且要出现在 op 0 的 provides 里
      "description": "…",          // 给模型看的
      "group": "example",          // 必填，没有 group 进不了能力目录
      "risk_level": "low",
      "input_schema": { "type": "object" },
      "timeout_ms": 3000,          // 必须 > 0
      "sensitive": false
    }
  ]
}
```

解析器拒绝**未知字段**（任何嵌套层级），所以拼错一个键名不会被忽略，而是当场
报错并点名。

`package/plugin.sig` 与 `dist/` **不入仓**：签名是密钥持有者的表态，附一个谁都
能拿到的示例密钥签名，等于邀请部署去信任一把没人控制的钥匙。

## 1. 构建

```bash
rustup target add wasm32-wasip1     # 只需一次
plugin_example/scripts/build.sh
```

脚本做两件事，第二件才是关键：把 wasm 复制进 `package/`，**并按它的 sha256
重新渲染 `plugin.json`**。`plugin.json` 里的 `sha256` 是 `plugin.wasm` 的摘要，
宿主加载时会比对——改了 wasm 不重跑脚本，加载时就是 `sha256 mismatch`。

不装 Rust 也能用：`package/` 里的 wasm 已经提交，可以直接跳到第 2 步。

## 2. 签名与打包

```bash
agent plugins keygen --key-id my-key --private-key my.key      # 只需一次
plugin_example/scripts/publish.sh my.key
```

产出 `dist/legion-hello.tar.gz`（恰好三个文件、平铺）与它的 **tarball 摘要**。

> 两个 sha256 不是一回事：`plugin.json` 里的是 **wasm** 的，`--digest` 要的是
> **tarball** 的。

把 `my.key` 的公钥（keygen 打印的那段）放进部署的 `keyring.json`。

## 3. 部署侧安装与授权

`agent.json`：

```json
{
  "plugins": {
    "manifest": "plugins.json",
    "root": "plugins",
    "cache": "cache",
    "keyring": "keyring.json",
    "require_signature": true
  }
}
```

```bash
echo '{ "plugins": [] }' > plugins.json

agent plugins install <tarball 的 URL> --digest sha256:<tarball 摘要> --config agent.json
agent plugins grant legion-hello --capabilities log --extensions observe --config agent.json
agent serve --config agent.json
```

`install` **只登记不授权**（写出的 entry 是 `enabled:false` 且没有 `grant` 段），
`grant` 才是授权。`--capabilities` 必须**恰好**是 `plugin.json` 声明的那一套；
`--extensions` 则可以是声明集合的**子集**——不写就是一个都不授，这个示例照样
贡献 `hello_echo`，只是不会被叫去观察别人的工具调用。

本地 `http://` 源站调试时，配置里还要 `"allow_insecure_sources": true`——它只
放开 scheme，digest 与验签一条不减。

## 4. 确认闭环

```bash
curl -s http://127.0.0.1:8080/v1/plugins
```

```json
{"plugins":[{"name":"legion-hello","version":"0.1.0","state":"loaded",
  "tools":["hello_echo"],"declared_capabilities":["log"],
  "declared_unresolved":false,"granted_capabilities":["log"],
  "declared_extensions":["observe"],"granted_extensions":["observe"]}]}
```

`state:"loaded"` + `tools` 里有 `hello_echo` = 模型的工具清单里已经有它了。让
模型调用 `hello_echo`，`agent serve` 的日志里会出现 guest 写的那行
`hello_echo called with name=…`——那是能力真的到达了 guest 的证据。

> `agent plugins status` 是**本进程视图**，在一个独立 CLI 进程里跑会明确报告
> 没有 loader。外部检查用 `GET /v1/plugins` 或 GUI 面板。

## 5. 不装 Rust 也能验证

```bash
go test ./plugin_example/...
```

六个测试用**真实的 wazero 宿主**跑这个包，钉住六件事：

| 测试 | 钉住什么 |
|---|---|
| `TestExamplePackageDigestMatchesTheModule` | 提交的 `plugin.json` 与 `plugin.wasm` 是配套的（忘了重跑 build.sh 就会红） |
| `TestExampleGuestSelfDescriptionCoversItsDeclaredTools` | op 0 的 `provides` 覆盖 `plugin.json` 声明的每个工具——不覆盖，激活期交叉校验会拒绝挂载 |
| `TestExampleToolCallReturnsAResultAndLogsThroughTheHost` | 闭环本身：宿主发调用 → guest 经 `log` 回调宿主 → 返回 `ToolResult` |
| `TestExampleRefusesToLinkWithoutItsCapability` | 能力是**链接期**事实：不授 `log`，模块根本链接不上，不是调用时返回 DENIED |
| `TestExampleObserverIsNotifiedThroughTheHost` | 扩展点闭环：宿主发 op 2 → guest 的观察者跑 → 它经 `log` 写下看到了什么。这个 seam 单向，只能靠它留下的痕迹证明它跑过 |
| `TestExampleObserverDoesNotTrapOnAnUnreadableObservation` | 读不懂的观察文档不能 trap：无人可报，而 trap 会连累那个正等着观察者被通知完的调用方 |

## 从这份示例改成你自己的插件

1. **写工具**：在 `guest/src/lib.rs` 里加一个 `fn(&ToolCall) -> ToolResult`，并
   把它加进 `declare_plugin!` 的 `tools = [...]`。加一个工具是**两处联动**：这里
   与 `scripts/build.sh` 内联 `plugin.json` 的 `tools`（宿主据后者注册）。op 0 的
   `provides` 由 SDK 推导，不用管。
2. **接能力**：需要 `log` 以外的能力时，打开 `guest/Cargo.toml` 对应的 feature
   （它透传给 SDK），同时把能力名写进 `plugin.json` 的 `capabilities`，部署侧
   `grant` 也要带上。`capabilities` **只写真正用到的**：多写一个是白拿权限，少
   写一个则模块直接链接失败。
3. **要复杂 JSON 就自己加 serde**：SDK 只解析宿主实际发来的那一种形状
   （`arguments` 是字符串到字符串）。工具内部要处理嵌套结构时，在**你自己的**
   crate 里加 `serde_json`，解析失败**返回失败的 ToolResult**，不要 panic——
   panic 会带走整个 wasm 模块，而失败结果只是模型能读懂的一个答案。
4. **别忘了白名单**：用 `http` / `fs` 时要在 `plugin.json` 里同时声明
   `network.allowed_hosts` / `filesystem.allowed_paths`，否则 `grant` 会直接拒绝
   授这两个能力。宿主按它们逐次校验，HTTP 的每一跳重定向都要重新过白名单。
5. **重新打包**：改完跑 `scripts/build.sh`（重新渲染 `sha256`）再
   `scripts/publish.sh`（重新签名，签名覆盖的是 `plugin.json` 的原始字节，改完
   不重签一定验不过）。
