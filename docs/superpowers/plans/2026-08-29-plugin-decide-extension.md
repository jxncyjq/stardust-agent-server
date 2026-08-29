# G4b 实施计划：决策点（deny）

**日期**：2026-08-29
**上游**：spec `specs/2026-08-29-plugin-extension-points-design.md` §3.2 与 §7（G4b）；G4a（`observe`）已交付并合入 master。
**范围**：插件可以在工具**派发之前**被征询，回答 `allow` / `deny`。`ask` 是 G4c，不在本期。

---

## 一、这一期要立住的四条

1. **只能收紧，永远不能放宽。** 宿主的 enforcer / policy 已经拒了的调用，插件根本不会被问到；插件回 `allow` 只是「我不反对」，不是授权。合成规则是**取最严**，与插件的先后顺序无关。
2. **fail-closed**（spec 决策 B）。超时、trap、答不出可解码的东西 = **拒绝这次调用**，并计入 G1 的连续故障计数——坏插件造成的停摆因此是**有界**的（到阈值自动卸载）。fail-open 会让「安全控制」变成「攻击者把插件搞崩就能关掉的安全控制」。
3. **未授权 = 不存在的注册**（G4a 立的规矩）。没有 `grant.extensions` 里的 `decide`，宿主不注册决策者，op 3 一次也不到达 guest。
4. **决策花的是这次调用的预算**。每次征询的上限是 `min(descriptor.Timeout/4, 200ms)`：决策跑在调用方的 goroutine 上，一个慢决策者就是一次慢工具调用。

## 二、落点

```
Registry.Execute:
  resolve → schema → enforcer.Check → policy.Decide
    → 【新】consultDeciders(ctx, call)      ← 只在前面全放行时才跑
    → guards.Before → handler → guards.After → sanitize → audit → notifyObservers
```

放在 `policy.Decide` **之后**是「只能收紧」的结构性保证：宿主说不行的调用，插件连看都看不到；插件也就没有任何位置可以把它改成行。

## 三、任务拆解

| # | 任务 | 关键测试 |
|---|---|---|
| T1 | `perm.Extensions` 增加 `Decide` | 未知扩展点名仍拒绝；`Names()` 顺序稳定 |
| T2 | `tool.Verdict` / `Decider` / `AddDecider` / `DecideOwned` + `Registry.Execute` 接入 | 任一 deny 即拒；插件 allow 不能推翻宿主 deny；顺序无关；撤销即不再被问；父注册表的决策者对子视图的调用同样生效；预算 = min(timeout/4, 200ms) |
| T3 | `abi.OpDecideToolCall = 3` + `host.pluginDecider` | trap/超时/坏文档三条都 **deny + 计故障**；调用方取消不算插件故障；答案严格解码（未知字段、尾随数据、空体全拒） |
| T4 | `contributeTools` 按授权注册决策者 | 未授权时注册表里根本没有这个 seam；`DisposeOwner` 同时收走决策者与工具 |
| T5 | 两个 SDK + 示例插件 | Go `Decide(fn)` / Rust `decide = fn`；op 0 的 `extensions` 仍从注册推导；示例插件真机跑通 op 3 |
| T6 | GUI 同意流展示扩展点 | 「这个插件将能否决你的工具调用」必须在授权前可见 |
| T7 | 文档：手册 §3.2/§3.4/§4/§7/§9、两份 SDK README、示例 README、路线图 | — |

## 四、明确不做

- `ask`（G4c）：本期的 `Verdict.Decision` 复用既有的 `tool.Decision` 字符串枚举，`ask` 之后加一个取值即可，**不改接口形状**。
- 让插件看到被宿主拒掉的调用（那是「未发生的执行」，与 G4a 观察点的排除项同理）。
- 让插件改写调用参数——那是包装（spec §5 明确不做）。

## 五、验收

`go build/vet/test ./...` 全绿、`gofmt -l` 空、`cargo test` 全绿、`-race ./internal/tool/... ./internal/plugin/...`；每个任务做变异核对。
