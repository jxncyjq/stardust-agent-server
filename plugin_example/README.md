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
guest/                Rust guest 源码（无第三方依赖，可 --offline 构建）
  Cargo.toml          crate-type=cdylib；每个宿主能力一个 feature，默认全关
  src/lib.rs          自述 MANIFEST、plugin_invoke 入口、op 分发
  src/tools.rs        工具实现与按名分发 —— 写自己的插件主要改这里
  src/host.rs         七个 host 函数的声明与安全包装（六个用 feature 关着）
  src/abi.rs          alloc / free / 打包解包 —— 基本可原样照抄
  src/json.rs         EXAMPLE-ONLY 的手搓 JSON，真插件换成 serde_json
package/              插件包目录：plugin.json + plugin.wasm（均已提交）
scripts/build.sh      构建 wasm 并按其摘要重新渲染 plugin.json
scripts/publish.sh    用你自己的私钥签名，打成 dist/legion-hello.tar.gz
example_test.go       用真实 wazero 宿主跑这个包（`go test ./plugin_example/...`）
```

分成五个文件不是为了好看：一个插件里**要改的**（`tools.rs`）、**按需接的**
（`host.rs`）、**照抄的**（`abi.rs`）和**该换掉的**（`json.rs`）是四类东西，
混在一个 `lib.rs` 里看不出哪块该动。

## 完整结构与刻意留空的槽位

这个示例只开了 `log` 一个能力，但**七个 host 函数的通路都已经写好**，其余六个
用 Cargo feature 关着（`src/host.rs`），每个槽位的注释里写明了请求 / 响应形状
与打开它的代价：

| 能力 | feature | 通路（src/host.rs） | 打开后还要做什么 |
|---|---|---|---|
| `log` | 默认开 | `log_info` | — |
| `config` | `config-capability` | `config()` | plugin.json 的 `capabilities` 加 `config` |
| `kv` | `kv-capability` | `kv_read` / `kv_write` | 同上加 `kv` |
| `http` | `http-capability` | `http()` | 加 `http`，**并声明 `network.allowed_hosts`** |
| `fs` | `fs-capability` | `read()` | 加 `fs`，**并声明 `filesystem.allowed_paths`** |
| `tool` | `tool-capability` | `invoke_tool()` | 加 `tool`；依赖别家工具还要写 `requires` |

**为什么默认关着**：能力是**链接期**事实。宿主只为已授权的能力注册 host 函数，
所以 import 一个部署没授权的函数，模块会在**实例化**时失败——guest 根本没机会
调用它，也不会收到一个 `DENIED` 去处理。于是「import 什么」必须与
`plugin.json` 的 `capabilities` 严格对齐，feature 让这件事变成一次显式动作：

```bash
# 三处联动，缺一处就装不上：
# 1) 构建时打开 feature
cargo build --release --target wasm32-wasip1 --features kv-capability
# 2) scripts/build.sh 里内联的 plugin.json：capabilities 加上 "kv"
# 3) 部署侧：agent plugins grant <name> --capabilities log,kv
```

`tools.rs` 的 `dispatch` 里还留了**第二个工具的位置**：一段完整注释掉的
`fetch_title` 实现，演示怎么用 `http` 通路、以及怎么区分正常响应与错误信封
（`{"code":"DENIED"|…}`）——把 `DENIED` 当成空 body 继续跑，就是在替越权的调用
打掩护。

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
agent plugins grant legion-hello --capabilities log --config agent.json
agent serve --config agent.json
```

`install` **只登记不授权**（写出的 entry 是 `enabled:false` 且没有 `grant` 段），
`grant` 才是授权。`--capabilities` 必须**恰好**是 `plugin.json` 声明的那一套。

本地 `http://` 源站调试时，配置里还要 `"allow_insecure_sources": true`——它只
放开 scheme，digest 与验签一条不减。

## 4. 确认闭环

```bash
curl -s http://127.0.0.1:8080/v1/plugins
```

```json
{"plugins":[{"name":"legion-hello","version":"0.1.0","state":"loaded",
  "tools":["hello_echo"],"declared_capabilities":["log"],
  "declared_unresolved":false,"granted_capabilities":["log"]}]}
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

四个测试用**真实的 wazero 宿主**跑这个包，钉住四件事：

| 测试 | 钉住什么 |
|---|---|
| `TestExamplePackageDigestMatchesTheModule` | 提交的 `plugin.json` 与 `plugin.wasm` 是配套的（忘了重跑 build.sh 就会红） |
| `TestExampleGuestSelfDescriptionCoversItsDeclaredTools` | op 0 的 `provides` 覆盖 `plugin.json` 声明的每个工具——不覆盖，激活期交叉校验会拒绝挂载 |
| `TestExampleToolCallReturnsAResultAndLogsThroughTheHost` | 闭环本身：宿主发调用 → guest 经 `log` 回调宿主 → 返回 `ToolResult` |
| `TestExampleRefusesToLinkWithoutItsCapability` | 能力是**链接期**事实：不授 `log`，模块根本链接不上，不是调用时返回 DENIED |

## 从这份示例改成你自己的插件

1. **写工具**：在 `guest/src/tools.rs` 里加实现，并在 `dispatch` 的 match 里加
   分支。加一个工具是**三处联动**：`tools.rs` 的分支、`lib.rs` 里 `MANIFEST` 的
   `provides`、`scripts/build.sh` 内联 `plugin.json` 的 `tools`。少改第二处，
   激活期的交叉校验会拒绝挂载；少改第三处，宿主根本不会注册这个工具。
2. **接能力**：需要 `log` 以外的能力时，打开 `guest/Cargo.toml` 对应的 feature，
   同时把能力名写进 `plugin.json` 的 `capabilities`，部署侧 `grant` 也要带上。
   `capabilities` **只写真正 import 了的**：多写一个是白拿权限，少写一个则模块
   直接链接失败。
3. **换掉 JSON**：`guest/src/json.rs` 整个是 `EXAMPLE-ONLY`，真插件请用
   `serde_json` 解析成结构体，解析失败**返回失败的 ToolResult**，不要 panic——
   panic 会带走整个 wasm 模块，而失败结果只是模型能读懂的一个答案。
4. **别忘了白名单**：用 `http` / `fs` 时要在 `plugin.json` 里同时声明
   `network.allowed_hosts` / `filesystem.allowed_paths`，否则 `grant` 会直接拒绝
   授这两个能力。宿主按它们逐次校验，HTTP 的每一跳重定向都要重新过白名单。
5. **重新打包**：改完跑 `scripts/build.sh`（重新渲染 `sha256`）再
   `scripts/publish.sh`（重新签名，签名覆盖的是 `plugin.json` 的原始字节，改完
   不重签一定验不过）。
