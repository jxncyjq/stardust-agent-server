# G4d 实施计划：插件的提示词段

**日期**：2026-08-29
**上游**：spec `specs/2026-08-29-plugin-extension-points-design.md` §3.3 + 决策 C（「进稳定前缀」）；G4a/G4b/G4c 已交付。
**范围**：被授予 `prompt` 扩展点的插件可以往系统提示词里贡献一段文本。

---

## 一、四条约束

1. **不可信文本进系统提示词**，所以必须带**边界标记**：模型要能分辨哪句话来自宿主、哪句来自一个被装上的插件。没有标记，一个插件写「忽略先前的指令」就和宿主的指令等价。
2. **进稳定前缀**（spec 决策 C）：它是**部署级**事实，跨任务不变。代价是插件挂载/卸载各让前缀缓存失效一次——低频操作，但必须写进手册的 token 一节，否则运维会把挂载后那一次缓存未命中当 bug 查。
3. **有上限**：每插件 2 KiB、全部合计 8 KiB，超出**截断并记 Warn**，且截断处在文本里留痕——静默截断会让作者以为整段都进去了。
4. **卸载即撤回**：与工具贡献同一套 ledger owner。

## 二、文本从哪来：一次，在激活期

新增 op 4（`OpPromptSegment`），宿主在**激活时问一次**，把答案缓存成部署级事实。

不在 `BuildContext` 里每次问 guest，理由是三条叠加：每个任务多一次 wasm 调用（延迟）；答案可能每次不同（稳定前缀立刻失效，决策 C 的收益归零）；而 `BuildContext` 在任务的关键路径上，一个慢 guest 会拖慢每一次推理。

激活期拿不到（trap/超时/坏文档）= **拒绝激活**。一个被授予 `prompt` 却给不出段落的插件，是部署以为装上了而实际没装上的状态。

## 三、落点

```
host.Activate → 授权了 prompt ? 问 guest 一次 → prompt.Segments.Add(owner, name, text)
cognitive.BuildContext → add("plugin_prompt", segments.Render())  ← 在 stablePrefixLen 之前
```

`internal/prompt` 是新的小包（host 与 cognitive 都要用它，放任一边都会造成 import 环）。渲染顺序按**插件名排序**，不是注册顺序：同一份部署每次起来必须逐字节一致，否则前缀缓存白做。

## 四、任务拆解

| # | 任务 | 关键测试 |
|---|---|---|
| T1 | `perm.ExtensionPrompt` + `sameExtensions` 逐 seam | 授予未声明的 prompt 被拒 |
| T2 | `internal/prompt.Segments`：Add/撤回/ledger、上限截断+Warn、排序、边界标记 | 超长截断留痕；总量超限后来者被拒并 Warn；顺序与注册顺序无关；空集渲染为空串 |
| T3 | `abi.OpPromptSegment` + 激活期取一次 + 授权闸门 | 未授权则不问 guest；guest 答不出 = 拒绝激活 |
| T4 | `cognitive.Core` 渲染进**稳定前缀** | 段落出现在 `stablePrefixLen` 之前；两次构建逐字节一致 |
| T5 | 两个 SDK + 示例插件 + e2e | Go `Prompt(fn)` / Rust `prompt = fn`；op 0 的 extensions 仍从注册推导 |
| T6 | GUI 同意流对 `prompt` 的措辞 | 勾选前能读到「这段文本会进系统提示词，且不可信」 |
| T7 | 文档：手册 §3.2/§3.4/§4/§7/§9 + token 一节、两份 SDK README、示例、路线图 | — |

## 五、明确不做

- 每次 `BuildContext` 问 guest（见 §二）。
- 让插件写进**稳定前缀之外**的位置（任务框架、对话）——那是宿主的地盘。
- 让插件的段落出现在别的插件的段落里，或互相引用。

## 六、验收

`go build/vet/test ./...` 全绿、`gofmt -l` 空、`cargo test` 全绿、`-race ./internal/cognitive/... ./internal/prompt/... ./internal/plugin/...`；每任务变异核对。
