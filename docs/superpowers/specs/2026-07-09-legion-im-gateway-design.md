---
id: "spec-im-gateway-001"
title: "设计: Legion IM 网关（多通道接入框架 + Telegram 样板）"
aliases: ["IM gateway spec", "channel gateway", "多通道接入设计"]
type: "spec"
category: "backend/library/gateway"
tags: ["spec", "legion-agent", "gateway", "im", "telegram", "multi-channel"]
version: "1.0.0"
created: "2026-07-09"
updated: "2026-07-09"
author: "jxncyjq"
status: "review"
parent: null
children: []
related_docs:
  - id: "analysis-hermes-gateway-004"
    relation: "implements"
    path: "../../../../../docs/design/analysis/hermes/04-gateway-cli-deployment.md"
---

# 设计: Legion IM 网关（多通道接入框架 + Telegram 样板）

<!-- @section: overview -->
## 概述

为 Legion 补齐即时通讯（IM）多通道接入能力，参考 hermes-agent 的网关架构（见 [[analysis-hermes-gateway-004|hermes 网关/CLI/部署分析]] §二消息网关）。

本设计交付：**可扩展适配器框架** + **一个真实样板平台（Telegram，长轮询入站）**，以独立进程 `legion-gateway` 落地，经现有 HTTP API + SSE 与核心通信，**核心零改动**。后续平台照框架模板添加。

**范围边界**：框架 + Telegram 样板。不含其它平台（Discord/WhatsApp 等按框架后续加）、不含 Web 仪表盘、不含 ACP 编辑器集成。
<!-- @end-section -->

<!-- @section: decisions -->
## 关键决策（已确认）

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 范围 | 框架 + Telegram 样板 | 单 spec 可落、立即可用、可扩展 |
| Telegram 入站 | **长轮询**（getUpdates 循环，出站拉取） | 无需公网 URL/TLS/端口暴露，防火墙友好，自托管最简；仿 hermes 自驱动异步适配器 |
| 进程形态 | **独立 gateway 进程/二进制** | 与核心松耦合，隔离、可独立扩展；仿 hermes docker-compose gateway 服务 |
| 完成信号 | **SSE 订阅** `/v1/events` | 复用现有事件流，`task_completed` 推送即时，低轮询负载 |
| 会话映射 | **每 chat 一会话**（chat_id → AgentSession），id 哈希脱敏 | 对话连续、复用现有会话历史；PII 边界仿 hermes `_hash_id` |
<!-- @end-section -->

<!-- @section: architecture -->
## 架构与进程拓扑

`legion-gateway` 纯出站，经现有 HTTP API + SSE 调核心：

```
legion-gateway 进程
  ┌─────────────────────────────────────────────────┐
  │ Telegram getUpdates 轮询循环 ──inbound──▶ 网关调度   │
  │      ▲                                    │POST /v1/tasks (Bearer)
  │      │ sendMessage 出站回投                 ▼
  │      └──────── deliver ◀── SSE task_completed ◀── legion serve(核心)
  └─────────────────────────────────────────────────┘
```

**全链路（自驱动异步）**：
1. 适配器 `Start(ctx)` 跑 `getUpdates` 长轮询（出站拉取，offset 游标去重）
2. 收到消息 → chat_id 确保有 Legion 会话 → `POST /v1/tasks{input, session_id}` → task_id
3. 网关内存记 `task_id → 投递目标(telegram:chat_id)`
4. 网关常驻订阅 SSE `/v1/events`；`task_completed{TaskID,Message}` → 查映射 → `adapter.Send` → Telegram `sendMessage`

**松耦合**：gateway 只走公开 HTTP API + SSE，核心不感知 IM 存在。
<!-- @end-section -->

<!-- @section: framework -->
## ChannelAdapter 框架（`internal/gateway`）

平台无关抽象 + 工厂注册，仿 hermes `BasePlatformAdapter`/`PlatformRegistry`/`DeliveryTarget`。

```go
// 平台无关入站消息
type InboundMessage struct {
    Platform   string    // "telegram"
    ChatID     string    // 会话 id（决定 Legion 会话映射）
    UserID     string    // 发送者 id（脱敏后入会话）
    Text       string
    Images     []string  // data-URI，可选（vision）
    ReceivedAt time.Time
}

// 出站投递目标，语法仿 hermes： "telegram:<chatID>[:<thread>]"
type DeliveryTarget struct {
    Platform string
    ChatID   string
    Thread   string // 可选
}

// 一个平台集成。自驱动：Start 跑自己的入站循环直到 ctx 取消，
// 把 InboundMessage 推给 sink；Send 出站回投。
type ChannelAdapter interface {
    Platform() string
    Start(ctx context.Context, inbound chan<- InboundMessage) error
    Send(ctx context.Context, target DeliveryTarget, text string) error
    Close() error
}

// 平台注册项（工厂 + 元数据），仿 hermes PlatformEntry
type PlatformEntry struct {
    Platform         string
    Factory          func(cfg PlatformConfig) (ChannelAdapter, error)
    MaxMessageLength int   // 超长回复按此切分（Telegram 4096）
    PIISafe          bool
}

// 平台注册表：name → entry，工厂模式（加平台不改核心逻辑）
type PlatformRegistry struct { /* Register / Get / Build(name, cfg) */ }

// 投递路由：按 platform 选 adapter，按 MaxMessageLength 切分长文
type DeliveryRouter struct { /* adapters; Route(ctx, target, text) error */ }
```

**单元职责/依赖**（各自可独立测试）：
- `ChannelAdapter` — 唯一平台耦合点；每平台一实现，仅依赖该平台 SDK/HTTP。
- `PlatformRegistry` — 纯查表工厂，无 I/O。加平台 = `Register(entry)`。
- `DeliveryRouter` — 纯路由 + 切分，依赖 adapter 集。
- `InboundMessage`/`DeliveryTarget` — 纯数据契约。
<!-- @end-section -->

<!-- @section: session-binding -->
## 会话绑定（两层映射，PII 边界）

```
① 绑定 store（网关私有，持久化 SQLite）
   platformKey "telegram:<chatID>"  ──▶  legionSessionID + rawChatID（出站用）

② 投递 map（网关内存，短生命）
   task_id  ──▶  DeliveryTarget{telegram, rawChatID}
```

**入站处理 `InboundMessage → task`**：
```
1. key = "telegram:" + chatID
2. binder.Resolve(key):
     命中 → 复用 legionSessionID
     未命中 → POST /v1/sessions{
                company_id, agent_id,                  // 网关配置的默认 IM agent
                project: "telegram",
                title:   "telegram:" + hashID(chatID)  // 脱敏，Legion 不见原始 id
              } → sessionID → binder.Bind(key, sessionID, rawChatID)
3. POST /v1/tasks{ input: text, session_id, company_id, agent_id, images } → task_id
4. delivery.Track(task_id → DeliveryTarget{telegram, rawChatID})
```

**PII 边界（仿 hermes `_hash_id`）**：
- Legion 侧（session/turns/task）**只存 `hashID(chatID)`**（sha256 截断），永不见原始 id
- **原始 id 只留网关绑定 store**，仅用于出站 `sendMessage`
- 同一 chat 多条消息 → 同一 `legionSessionID` → 复用现有 `AgentSession`/`ConversationTurn` → 多轮对话记忆自动生效

```go
type SessionBinder interface {
    Resolve(ctx context.Context, platformKey string) (sessionID, rawChatID string, ok bool, err error)
    Bind(ctx context.Context, platformKey, sessionID, rawChatID string) error
}
```
- 样板实现：网关自带 SQLite（`modernc.org/sqlite`）表 `channel_bindings(platform_key PK, session_id, raw_chat_id, created_at)`
- 持久化 → 网关重启后会话连续性不丢
<!-- @end-section -->

<!-- @section: delivery -->
## 出站投递 + 编排

```
核心 /v1/events (Bearer) ──SSE 流──▶ 事件过滤 (只要 task_completed)
    │ 断线→指数退避重连              │ {TaskID, Message}
    ▼                                ▼
                         delivery.Take(TaskID) ──▶ target?
                              命中│              未命中→跳过(非本网关任务，合法)
                                  ▼
                    DeliveryRouter.Route(target, Message)
                         │ 按 MaxMessageLength(4096) 切分，优先换行处断
                         ▼
                    adapter.Send(target, chunk) ──▶ Telegram sendMessage
```

**`GatewayRunner`（进程编排，仿 hermes GatewayRunner）**：
```go
type GatewayRunner struct {
    adapters []ChannelAdapter
    router   *DeliveryRouter
    binder   SessionBinder
    delivery *DeliveryTracker   // task_id → target 内存映射
    core     CoreClient
    cfg      GatewayConfig
}
func (g *GatewayRunner) Run(ctx context.Context) error // 起 adapter.Start + 入站消费 + SSE 投递环
```
三条循环并发（errgroup 式），一路致命错误 → 整体停 + 记录。

**`CoreClient`（隔离核心 HTTP 细节，可 fake）**：
```go
type CoreClient interface {
    EnsureSession(ctx context.Context, req SessionReq) (sessionID string, err error)
    SubmitTask(ctx context.Context, req TaskReq) (taskID string, err error)
    StreamEvents(ctx context.Context) (<-chan CompletedTask, error) // 封装 SSE 解析+重连
}
```

**长回复切分**：`Route` 按平台 `MaxMessageLength` 切块，优先换行/句边界断，避免截断多字节字符。
<!-- @end-section -->

<!-- @section: config-deploy -->
## 配置与部署

**`GatewayConfig`（独立 `gateway.json`，密钥走 env）**：
```jsonc
{
  "core":     { "base_url": "http://127.0.0.1:8080", "token_env": "LEGION_ADMIN_TOKEN" },
  "identity": { "agent_id": "im-agent", "company_id": "default" },
  "binding":  { "sqlite_path": "gateway.db" },
  "delivery": { "retries": 3, "backoff_ms": 500 },
  "platforms": {
    "telegram": { "enabled": true, "token_env": "LEGION_TELEGRAM_BOT_TOKEN", "poll_timeout_s": 30 }
  }
}
```
- 密钥只从 env 读（bot_token / admin_token），不落配置文件
- 复用 Legion `config` 风格（JSON + 缺键 fail-loud，ADR-0001）

**包布局**：
```
cmd/gateway/main.go                    # legion-gateway 入口
internal/gateway/
  adapter.go        # ChannelAdapter / InboundMessage / DeliveryTarget
  registry.go       # PlatformRegistry / PlatformEntry
  router.go         # DeliveryRouter（路由+切分）
  binder.go         # SessionBinder + sqlite 实现
  tracker.go        # DeliveryTracker（task_id→target）
  core_client.go    # CoreClient（HTTP + SSE 解析/重连）
  runner.go         # GatewayRunner（编排）
  config.go         # GatewayConfig
  hash.go           # hashID（PII 脱敏，sha256 截断）
  platforms/telegram/adapter.go   # getUpdates 轮询 + sendMessage
```

**部署**（长轮询 → 纯出站）：
- 新二进制 `legion-gateway`，与 `legion serve` 并行；docker-compose 两服务（仿 hermes serve+gateway）
- 网关只需：能连核心（内网）+ 能出站 `api.telegram.org`；**无入站端口**，防火墙友好
<!-- @end-section -->

<!-- @section: error-handling -->
## 错误处理（Fail-Loud 铁律）

- 启动缺 bot_token / core URL / 未知 platform → `log.Fatal`（装配期）
- `Send`/`Start`/SSE 各边界 → slog 结构化记录（Error/Warn），带 platform / chatID(hash) / taskID 字段
- `Take` 未命中 = 非本网关提交的任务（其它 API 客户端也发任务）→ **合法跳过**（契约显式可选，非兜底）
- `Send` 失败 → 有限退避重试 `retries` 次，仍败则丢弃并记 Error（不崩网关）
- SSE 断线 → 指数退避重连；重连间隙可能漏 `task_completed`（核心事件流为 live 推送）→ 该窗口内完成的回复丢失（见 follow-up 对账）
<!-- @end-section -->

<!-- @section: testing -->
## 测试策略（每单元独立 + 错误路径断言）

| 单元 | 测法 |
|------|------|
| PlatformRegistry | register / build / 未知平台报错 |
| DeliveryRouter | fake adapter：切分正确、Send 错误上抛 |
| SessionBinder(sqlite) | miss→bind→hit；重开持久化不丢 |
| 入站流 | fake CoreClient+adapter：EnsureSession/SubmitTask 用**脱敏 id**、Track 记录 |
| 出站环 | fake SSE(channel)：completed→Send(target,text)；Take-miss 跳过 |
| Telegram adapter | httptest 模拟 getUpdates/sendMessage：offset 游标推进、入站解析、出站投递 |
| GatewayRunner | fakes 集成串起 |
| fail-loud | 缺配置 / Send 失败 / 未知平台 各断言 |

验证门（同核心）：`go build ./... && go vet ./... && go test ./... && gofmt -l .` 全绿。
<!-- @end-section -->

<!-- @section: follow-ups -->
## 明确非目标 / Follow-up

- **in-flight 投递持久化**：投递 map 为内存态，网关在任务进行中崩溃 → 该条回复丢失。可把 `task_id→target` 落 SQLite + 重连后对账（`GET /v1/tasks/{id}/result`）修复。
- **其它平台**：Discord/WhatsApp/Slack 等按 `ChannelAdapter` + `PlatformEntry` 模板后续加，各自入站形式（webhook/websocket）由适配器自行封装。
- **富媒体/命令菜单/typing 指示**：MVP 仅纯文本双向；富媒体、斜杠命令菜单、输入中指示为后续。
- **每用户会话 / 群聊分发言者**：当前每 chat 一会话；如需群聊按发言者分上下文，扩展 platformKey 粒度。
- **MoA 质量感知路由接入**：IM 任务可结合 `moa_consult`，非本 spec 范围。
<!-- @end-section -->

## 相关文档

- [[analysis-hermes-gateway-004|hermes 网关/CLI/部署分析]] — 参考来源
- [[adr-delegation-001|ADR: delegate_task 子任务委派]] — 复用的 agent 消息/事件机制近亲
