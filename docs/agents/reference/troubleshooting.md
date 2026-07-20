# 故障定位

## 一、先看哪里

GUI 上的「任务执行失败，未返回结果」**不包含失败原因**——`/v1/tasks/{id}/result` 在失败时 `result` 为空字段，接口当前不返回错误详情。真因在另外两处：

### 1. 结构化日志（最完整）

```bash
grep -a '"msg":"task run failed"' logs/agent.log | tail -5
```

输出形如：

```json
{"time":"...","level":"ERROR","msg":"task run failed","component":"coordinator",
 "task_id":"gui-task-...","agent_id":"default-agent",
 "error":"run task: generate inference: openai chat endpoint returned 402 ..."}
```

`error` 字段是完整错误链，从最外层动作一直到根因。

> **注意 `agent_id` 字段在成功与失败事件里的含义不同**：
>
> - **成功**事件（`learning_event ... signal=success`）里是**实际执行**任务的 agent，可信；
> - **失败**事件（`task run failed` 日志、`learning_event ... signal=failure`）里是 **coordinator 自身**的 agent ID（固定 `default-agent`），**不是**实际执行者 —— 失败路径记录的是 `c.agent.ID` 而非解析后的 `runnerAgent.ID`。
>
> 所以失败时若看到 `agent_id=default-agent`，**不能**据此判断"sub-agent 没生效"。要确认实际执行者，看 `error` 内容里的路径（例如 `scan skills in "skills/researcher"` 说明走的是 researcher），或查任务记录 `GET /v1/tasks/{id}` 的 `agent_id`。

### 2. 事件流（GUI 可见）

GUI 右侧事件面板中的 `task_failed` 事件，Message 即完整错误。

同时出现的 `learning_event` 携带的是 `reason=task_run_error` 这类**固定枚举值**，供演进管线消费，不承载具体错误——不要在那里找原因。

### 3. 历史版本注意

若日志中**完全找不到**任务失败记录，说明服务端版本早于「恢复可诊断性」的修复。该版本中失败原因在 goroutine 顶层被丢弃，日志、事件、接口三处都不可见，需先升级服务端。

## 二、常见失败原因

### `402 Payment Required: Insufficient Balance`

```
run task: generate inference: openai chat endpoint returned 402 Payment Required:
{"error":{"message":"Insufficient Balance",...}}
```

MaaS 账户余额不足。非代码问题，充值即可。与选择哪个 agent 无关——所有 agent 都会撞上。

### `scan skills in "...": cannot find the path specified`

```
run task: build cognitive context: select task skills:
scan skills in "skills/researcher": ... The system cannot find the path specified.
```

skills 目录不存在。典型表现是**选任何 sub-agent 都失败、default agent 正常**。

处理：建目录（`mkdir -p skills/researcher skills/writer`），或升级到已修复版本。详见 [directory-layout.md](./directory-layout.md)。

### `create maas runner for profile "xxx"`

agent 的 `maas_profile` 在 `maas.profiles` 中不存在，或该 profile 配置有误。核对两处名字是否一致。

### 服务启动即失败，报某个 agent 配置文件路径

agent 配置文件不存在或 JSON 格式错误。注册表在启动时一次性加载，任一文件有问题即拒绝启动，不会带半套配置运行。检查 `agent.json` 的 `agents` 映射中该路径是否正确（相对路径以 `agent.json` 所在目录为基准）。

### `workspace root fallback` WARN

```
configured workspace.root "C:\Users\xxx\.stardust" unusable
(cannot find the file specified), falling back to "C:\Users\xxx\.stardust"
```

配置值与回退值恰好相同，所以看起来像空转。实际含义是**该目录不存在**。`mkdir ~/.stardust` 即可。仅为 WARN，不影响任务执行。

## 三、绕过 GUI 直接验证服务端

判断问题在前端还是服务端，最快的方式是直接打接口：

```bash
# 提交
curl -s -X POST http://127.0.0.1:<port>/v1/tasks \
  -H 'Content-Type: application/json' \
  -d '{"id":"diag-1","input":"ping","agent_id":"researcher","company_id":"default-company"}'

# 查结果
curl -s http://127.0.0.1:<port>/v1/tasks/diag-1/result

# 查任务记录（确认服务端收到的 agent_id）
curl -s http://127.0.0.1:<port>/v1/tasks/diag-1

# 查事件
curl -s "http://127.0.0.1:<port>/v1/runtime-events?limit=10"
```

若直接调接口同样失败，问题在服务端，与 GUI 无关。

## 四、配置改动的生效范围

| 改动 | 需要重启服务端 | 需要重新构建 |
|---|---|---|
| `agent.json` / agent 配置文件 | ✅ | ❌ |
| 建目录（skills 等） | ✅（挂载在启动时决定） | ❌ |
| 服务端 Go 代码 | ✅ | ✅ |
| GUI 前端代码 | ❌ | ✅（前端构建） |

只重启 GUI 不会重新加载服务端的 agent 注册表。
