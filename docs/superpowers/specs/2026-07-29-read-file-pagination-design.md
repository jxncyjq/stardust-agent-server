---
title: read_file 分页读设计（offset/limit + 512KB 读盘上限）
date: 2026-07-29
status: approved
scope: legionAgent internal/tool — read_file 工具
---

# read_file 分页读设计

## 目标

让模型能读完超过单次返回上限的文件。现状是：文件被硬截在前 4000 字，模型看不到后半、又没有任何续读手段，于是**反复整篇重读同一文件**——实测一个任务重读 5 次、跑满 13 轮、消耗 99.1k input token。

## 病理（实测证据）

会话 `session-1785336732831509900` 末任务，日志逐条还原：

```
[8]  模型自述：「文件三次读取都被截断在第9.2节。第10节（理赔指南）始终未展示」
[16] search_content 命中「## 10. 理赔指南」
[18] no matches
[20] ⚠️ 本任务已第 5 次读取此文件（PR #58 的提醒生效，但模型无替代手段）
```

根因是**两层截断中的第二层**：
- `internal/tool/builtin.go` `maxReadFileBytes = 256KB` —— 读盘上限，不是本次病因。
- `internal/runtime/messages.go` `appendToolResults` → `truncateText(content, maxToolResultChars=4000)` —— **纯前截**：保留前 4000 runes，追加 `…[truncated N chars]`，**没有尾部**。

手册 13,551 B ≈ 4,700 runes → 模型永远只能看到前 4000 runes，第 10 节不可达。`read_file` 的入参只有 `path`，没有任何续读手段。

## 硬约束

**任何一次 read_file 的返回必须 ≤ `maxToolResultChars`（4000 runes）**，否则 runtime 会再截一刀，分页形同虚设。这是本设计所有取值的根据。

## 决策（已确认）

1. **分页单位 = 字符（rune）offset**。与 `maxToolResultChars` 同单位，页边界可精确控制；行号方案会因「单行超长」撑破 4000 上限。
2. **页大小默认 3500，模型可传 `limit`，服务端 clamp 到 `[1, 3500]`**。3500 硬低于 4000，永不被二次截断。
3. **截断提示给出可执行的续读参数**，而不只是「还剩多少」。
4. **`maxReadFileBytes`: 256KB → 512KB**。读盘上限翻倍，配合分页让 512KB 以内的文件全部可经 offset 触达。
5. **`search_content` 的 `searchContentMaxFileBytes` 保持 256KB 不变**：它是全目录遍历时的逐文件保护上限，且只需匹配、不需全文进上下文，与单文件按需读的取舍不同。

## 接口

```
read_file(path, offset?, limit?)
```

| 参数 | 类型 | 语义 |
|---|---|---|
| `path` | string（必填） | 现有语义不变（相对 workspace root 或其内的绝对路径） |
| `offset` | int（可选，默认 0） | 起始 rune 偏移 |
| `limit` | int（可选，默认 3500） | 本次最多返回的 runes，clamp 到 `[1, 3500]` |

## 返回

正常返回内容切片 `content[offset : offset+limit]`。**当且仅当还有剩余**时追加一行可执行提示：

```
…[已返回第 3501-7000 字，共 4700 字；继续读用 read_file(path="…", offset=7000)]
```

最后一页不带提示（读完即止，避免模型误以为还有内容而再发一轮）。

## 边界与错误处理（fail-loud）

| 情况 | 行为 |
|---|---|
| `offset < 0` | 返回 error（`read_file offset 不能为负`） |
| `offset >= 文件 rune 总长` | 返回 error（`read_file offset N 超出文件长度 M`）——**不静默返回空内容**，空内容会被模型读成「文件到此为止」 |
| `limit <= 0` | 用默认 3500（契约声明的可选缺省，非兜底） |
| `limit > 3500` | clamp 到 3500；description 写明上限，返回提示里体现实际范围，模型可自行看出 |
| 文件总长 ≤ limit | 全文返回，不加提示 |
| 文件 > `maxReadFileBytes`(512KB) | 保留现有「超上限已截断」标注；分页在已读入的 512KB 内进行 |

参数解析失败（非数字）一律返回 error，不静默当默认值。

## 不改动

- `maxToolResultChars`（4000）不动 —— 分页的目的正是让它不必提高。
- `truncateText` / `appendToolResults` 不动 —— 第二层截断保留为最终防线。
- `search_content` / `list_files` 不动。
- PR #58 的重读提醒保留：分页解决「读不到」，重读提醒解决「无谓重读」，两者互补。

## 测试

- 无 offset/limit 的长文件 → 返回前 3500 runes + 含 `offset=3500` 的续读提示。
- `offset=3500` → 返回第二页，且提示中的下一 offset 正确。
- 读到末页 → 内容完整且**不含**续读提示。
- 短文件（< 3500） → 全文、无提示。
- `offset` 越界 → 返回 error（断言 error 非 nil，不是空内容）。
- `offset < 0` → 返回 error。
- `limit > 3500` → 实际返回 ≤ 3500。
- 多字节内容按 rune 切分，不出现半个汉字。
- 门禁：`go build/vet/test ./...` 全绿、`gofmt -l .` 空。

## 相关

- 病理取证：本会话 `session-1785336732831509900`，runtime.debug 探针 + 分块账。
- memory：[[legion-token-multiround-debug-probe]]（多轮累积成本结构）。
- 相邻已交付：PR #58（重读可见提醒）、P1 非连续重复守卫、P2 对话 compaction。
