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

`testdata/hello` 是一个完整的最小插件；`guest_test.go` 会把它**构建出来**（不提交产物：3 MB，而且会在 SDK 改动时立刻过期）并用真实 wazero 宿主跑通六件事：自述来自注册表、闭环带 log 回调、缺参数是失败结果而非 trap、未授权即链接失败、GC 守卫确实在按住缓冲区、以及 200 次调用后没有泄漏。
