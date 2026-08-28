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
guest/            Rust guest 源码（无第三方依赖，可 --offline 构建）
package/          插件包目录：plugin.json + plugin.wasm（均已提交）
scripts/build.sh  构建 wasm 并按其摘要重新渲染 plugin.json
scripts/publish.sh 用你自己的私钥签名，打成 dist/legion-hello.tar.gz
example_test.go   用真实 wazero 宿主跑这个包的测试（`go test ./plugin_example/...`）
```

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

1. 改 `guest/src/lib.rs` 的 `MANIFEST`（`name` / `provides`）与 `handle_call`。
2. 改 `scripts/build.sh` 里内联的 `plugin.json`：`name`、`tools`、以及
   `capabilities`——**只写真正用得上的**。多写一个而 guest 不 import，只是白拿
   权限；guest import 了却没写，模块直接链接失败。
3. 换掉两个 `EXAMPLE-ONLY` 的手写 JSON 函数：真插件请用 `serde_json` 解析成
   结构体，解析失败**返回失败的 ToolResult**，不要 panic——panic 会带走整个
   wasm 模块，而失败结果只是模型能读懂的一个答案。
4. 需要 `http` / `fs` 能力时，记得在 `plugin.json` 里同时声明
   `network.allowed_hosts` / `filesystem.allowed_paths`：宿主按它们逐次校验，
   HTTP 的每一跳重定向都要重新过白名单。
