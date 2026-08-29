# legionplugin

写 Legion Agent WASM 插件的 **Go** guest SDK（ABI v1）。只用标准库。

Rust 版在 `sdk/rust/legion-plugin`，形状相同。

## 用它

```go
package main

import "github.com/stardust/legion-agent/pkg/legionplugin"

func init() {
	legionplugin.Serve("legion-hello-go", "0.1.0", legionplugin.Tool{
		Name: "hello_echo",
		Handler: func(call legionplugin.ToolCall) legionplugin.ToolResult {
			name := call.Argument("name")
			if name == "" {
				return legionplugin.Fail("missing required argument: name")
			}
			legionplugin.LogInfo("hello_echo called with name=" + name)
			return legionplugin.OK("hello, " + name + "!")
		},
	})
}

func main() {}
```

构建：

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

SDK 生成四个导出（`_initialize` / `plugin_alloc` / `plugin_free` / `plugin_invoke`）、op 分发、内存管理、指针打包、JSON，以及 **op 0 的自述**——`provides` 由 `Serve` 注册的工具推导并排序，所以「guest 说自己提供什么」与「实际能分发什么」不会对不上，且自述字节稳定（不稳定会让包的 digest 无故变化，而 digest 正是部署钉住的东西）。

## 为什么注册在 `init`、`main` 是空的

插件是 WASI **reactor**，不是命令行程序：宿主用 `WithStartFunctions("_initialize")` 实例化，然后调用导出函数，**`main` 永远不会被调用**。`_initialize` 跑的就是包初始化，所以注册只能放 `init`；`main` 存在只是因为 `package main` 要求它。

## 能力

`LogInfo` / `LogWarn` / `LogError` / `LogDebug` 无条件可用。其余六个 host 函数在 build tag 后面：`legion_config`、`legion_kv`、`legion_http`、`legion_fs`、`legion_tool`。

打开一个能力要**三处联动**，缺一模块就在实例化时失败（能力是链接期事实，不是运行期开关）：

```bash
# 1) 构建时带 tag
GOOS=wasip1 GOARCH=wasm go build -tags legion_kv -buildmode=c-shared -o plugin.wasm .
# 2) plugin.json 的 capabilities 加 "kv"
# 3) 部署侧 agent plugins grant <name> --capabilities log,kv
```

## 扩展点：观察工具调用

能力是插件调宿主，**扩展点是宿主调插件**。当前只有一个：`observe`——每次工具调用
**答完之后**回调你一次。

```go
func init() {
	legionplugin.Serve("legion-audit", "0.1.0", legionplugin.Tool{ /* … */ })
	legionplugin.Observe(func(o legionplugin.ToolObservation) {
		legionplugin.LogInfo("saw " + o.Tool)
	})
}
```

同样是**三处联动**，但失败点各不相同：

| 位置 | 缺了会怎样 |
|---|---|
| `legionplugin.Observe(...)` | 部署授权了却没实现 → **激活期拒绝**（op 0 的 `extensions` 由本 SDK 从注册推导，宿主拿它交叉校验） |
| `plugin.json` 的 `"extensions": ["observe"]` | `grant --extensions` 拒绝：没声明的授不了 |
| `agent plugins grant <name> --extensions observe` | 宿主不注册观察者，op 2 一次也不到达——**静默且正确**，这就是未授权的含义 |

四条边界，都不是可以商量的：

- **改不了任何东西。** 观察者没有返回值，宿主丢弃 op 2 的应答；调用方的结果在观察者跑之前就定了。
- **只看得到跑起来并答了的调用。** 被权限 / 策略 / 护栏拒掉的调用从不通知（它没发生过），handler 返回 Go error 也不通知（那是宿主或工具的故障）。`success:false` **会**通知：工具跑了，答了「不行」。
- **看到的是任意工具**，不只是本插件的。
- **每次 200ms**，跑在调用方的 goroutine 上；超时或 trap 计入本插件健康度。贵的活儿留到自己的下一次工具调用里做。

### 决策点：在派发前否决一次调用

`decide` 是第二个扩展点，也是第一个**答案有后果**的：

```go
legionplugin.Decide(func(req legionplugin.ToolDecisionRequest) legionplugin.ToolDecision {
	if req.Tool == "write_file" && frozen() {
		return legionplugin.Deny("writes are frozen during the incident")
	}
	return legionplugin.Allow()
})
```

四条边界：

- **只能收紧。** 宿主自己的权限与策略先跑；它们拒掉的调用根本不会问到插件。`Allow()` 是「我不反对」，不是授权。
- **答不出来就是拒绝**（fail-closed）：超时、trap、答出宿主解不了的东西，都会拒掉那次调用并计入本插件健康度。这不是苛刻——fail-open 会让「安全控制」变成「把插件搞崩就能关掉的安全控制」；代价则因为 G1 到阈值自动卸载而**有界**。
- **`ToolDecision` 的零值不是合法回答**：忘了返回会得到一个宿主解不了的文档（于是拒绝），而不是意外的放行。
- **上限 `min(工具超时/4, 200ms)`**，比观察点更紧：工具还没开始跑。

`Ask(reason)` 是第三种答案：不是拒绝，而是**要人批**。宿主在 round 边界挂起任务、开一张点名本插件与这条理由的票，人批了再从检查点继续；部署里没有审批通道时按拒绝处理。它**不看模式**——Auto 模式下同样会停下来等人。

```go
if req.Tool == "deploy" {
	return legionplugin.Ask("deploys are reviewed by a human")
}
```

### 提示词段：往系统提示词里加一段文字

`prompt` 是第三个扩展点，也是唯一一个「不是调用」的：宿主在**激活时问一次**，答案在插件挂着期间进每一次推理。

```go
legionplugin.Prompt(func() string {
	return "When citing a Jira issue, link it as https://jira.example.com/browse/KEY."
})
```

四条边界：

- **只问一次**：这个函数可以读插件的配置，但不要试图每次说不一样的话——这段文字待在提示词的**稳定前缀**里，变一次就让整段前缀缓存失效一次。
- **带围栏**：渲染时被 `--- plugin "<名字>" (untrusted…) ---` 包起来。模型据此分辨哪句话来自宿主、哪句来自被装上的插件。
- **有上限**：单插件 2048 rune、合计 8192 rune；超长截断并留痕。它**每次推理都在**，所以短不是风格偏好。
- **答不出来 = 插件挂不上**：被授予 `prompt` 却给不出段落，会让部署以为装了指令而模型从没看见。空字符串是合法回答（这个部署里没话说）。

## 两条硬规矩

- **工具失败返回 `Fail`，不要 panic。** panic 会 trap 整个模块，代价是实例状态、同实例的在途调用，并计入插件健康度（连续故障到阈值会被自动卸载）。
- **不要把请求体的切片存到调用之外。** SDK 已经复制了一份给你，宿主那块内存在调用返回后就被回收。

## Go 特有的一件事：GC

`plugin_alloc` 返回之后，宿主手上只有一个**整数地址**，Go 的垃圾回收器看不见它。SDK 因此自己按住每个缓冲区直到 `plugin_free`——没有这一步，回收发生在分配与宿主写入之间时，宿主会写进已被复用的内存，而这种损坏只在高负载的生产里出现。

`LiveBuffers()` 报告当前按住的数量，可以用来查泄漏：跨调用只增不减就是有东西没被释放。

## 与 Rust SDK 的取舍

| | Go（本包） | Rust（`sdk/rust/legion-plugin`） |
|---|---|---|
| 产物体积 | ~3.3 MB | ~49 KB |
| 内存下限 | 32 MiB（`max_memory_pages: 512`） | 4 MiB（64 页）够用 |
| 工具链 | 只要 Go 1.24+ | `rustup target add wasm32-wasip1` |
| GC | 有（SDK 内部按住缓冲区） | 无 |

体积或内存敏感、或者一个部署要挂很多插件，用 Rust；团队只有 Go 就用 Go——3.3 MB 是标准 Go 运行时的代价，不是 SDK 的。

## 示例与测试

`testdata/hello` 是一个完整的最小插件；`guest_test.go` 会把它**构建出来**（不提交产物：3 MB，而且会在 SDK 改动时立刻过期）并用真实 wazero 宿主跑通十一件事：自述来自注册表（含 `extensions`）、闭环带 log 回调、缺参数是失败结果而非 trap、未授权即链接失败、GC 守卫确实在按住缓冲区、200 次调用后没有泄漏、op 2 真的到达注册的观察者、未知 op 有答案而不是 trap，决策点放行/拒绝/送审三条路都真的走通，以及提示词段答得出宿主能解码的文档。
