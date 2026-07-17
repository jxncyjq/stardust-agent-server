
### 0. Fail-Loud 铁律：禁止兜底 / fallback，错误必报必记（最高优先级，覆盖下方一切便利写法）

**出错就响亮地错，绝不悄悄兜。**

- **禁止任何「逻辑兜底」/ fallback**：不得用以下手段掩盖非预期状态——
  - 返回 zero value 假装正常（`return 0, nil` / `return "", nil` / `return nil, nil` / 返回空 slice、空 map、空 struct 冒充有效结果）；
  - 丢弃错误（`_ = f()` / `v, _ := f()` 在契约未声明可忽略时丢掉 error）；
  - 静默跳过（`if err != nil { continue }` / `if err != nil { return }` 不记不报就继续或退出）；
  - comma-ok 取到 false 却猜一个值接着跑（`v, ok := m[k]; if !ok { v = 某默认 }` 用于本不该缺失的键）；
  - `recover()` 吞掉 panic 后假装无事继续（recover 仅允许用于在边界把 panic 转成已记录的 error 并终止该工作单元，不得「恢复后接着跑业务」）；
  - `default:` / `else` 分支吞掉非预期枚举值 / 状态。

  约定字段缺失、上游报错、反序列化 / 解码失败、依赖（DB/Redis/NATS/etcd/RPC 对端 / 引擎层调用）不可用等「本不该发生」的情况，一律 fail-fast：
  - 业务 / 领域层：**返回 error**，并用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装，保留错误链与定位上下文；领域不变量违反返回领域错误（如 `ddd.ErrXxx`），不得静默修正聚合状态；
  - 启动期 / 不可恢复的不变量违反 / 编程错误：按 Go 惯例 `log.Fatal`（启动装配）或 `panic`（不变量断言、绝不该到达的分支）；
  - 绝不「凑个值接着跑」。**权威逻辑尤其严**：服务端是唯一真相源（ADR-0002 P1），结算 / 消耗 / 判定环节兜底 = 把错误数值写进权威状态，比崩溃更糟。

- **错误点必须可定位、必须记录**：
  - 传播时用 `%w` 包装注明「发生了什么、哪个输入 / 标识、为什么」，使错误链自带上下文；
  - 在错误被**处理 / 吞咽 / 终止**的边界（RPC/Gateway handler、NATS 消费者、goroutine 顶层、System tick 边界、main 装配），必须用项目 logger 结构化记录：不可恢复用 **Error** 级，异常但可恢复用 **Warn** 级，带足够字段（实体 ID / 玩家 ID / 节点 ID / RowID / 操作名等）。用既有日志设施（zap / slog 结构化，见 `server_eng/logs`、`server_eng/observe`、ADR-0007），**不要** `fmt.Println` / 裸 `log.Printf` 充当错误日志；
  - **严禁静默吞错**：既不许 fallback，也不许 `_ = err`、不许裸 `return err` 不带上下文、不许默默 return / 默默 `continue` / 默默忽略。`defer x.Close()` 等若其 error 有意义，至少 Warn 记录，不得无声丢弃。

- **唯一豁免——契约显式声明的「可选」**：仅当某字段 / 能力 / 配置在契约里被明确定义为 optional（如可选配置槽、feature 开关、proto3 标量缺省、文档写明「缺省即关闭」、map 查找在语义上允许缺键）时，「缺省 / 不存在」才是合法状态，按契约处理即可——这不算兜底。
  - 判别：**可选 = 契约允许它不存在；兜底 = 出错了却假装没事。**
  - 两者存疑时一律按 fail-loud 处理。
  - comma-ok（`v, ok := m[k]`）本身中性：契约允许缺键时是正当可选；用于「本应存在却缺失」时则属兜底，须 fail-loud。

> 与既有 ADR 一致：ADR-0001 配置缺键 / 类型错「大声报错，绝不静默零值」；ADR-0002 P1 服务端权威拒绝越权而非容忍；ADR-0005 帧守卫超界即断。本铁律是这些立场的通用化。

---

## 其余编码规范

- 公开 API 必须有文档注释（Go doc 风格：以标识符名开头）。
- 游戏数值必须**数据驱动**（`config/` 外部配置，经 ADR-0001 管线读，权威端用 Go 读同一份源），严禁硬编码平衡数字。
- **业务 / 引擎边界**：业务组件（Position/Health 等）与 proto 消息定义在 `server/`；`server_eng/` 保持业务无关（守 ADR-0004）。业务经引擎暴露的接口 / 泛型自由函数消费 ECS，不反向污染引擎。
- **DDD 建模**：AggregateRoot 守不变量、Repository 抽象持久化、DomainEvent 解耦；限界上下文映射 ADR-0002 服务划分（Scene/War Director/Account/...）。
- 确定性：核心玩法 ECS 逻辑禁随机 / 时钟（保权威可复现，反作弊 / 压测前提，ADR-0004）；I/O 层超时判断除外。
- 服务间仅经预定义接口（Go interface ≡ 拆分后 gRPC，P4 接口先行）+ NATS 异步事件；禁共享数据库表（ADR-0002 P2）。
- 优先依赖注入而非全局单例，便于测试。
- 每个新系统 / 重要决策需在 `docs/architecture/` 有对应 ADR；提交引用相关 story ID 或设计文档。

## 测试

- `go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空，方算完成。
- 每个系统覆盖其逻辑 / 边界的单元测试；错误路径（fail-loud 分支）须有测试断言「确实返回 error / 确实记录」，不得只测 happy path。
- 权威结算逻辑须覆盖非预期输入（缺配置 / 越权 / 数值越界）确实 fail-loud。

## 引擎 / 依赖版本

- 用第三方库前确认版本与 `go.mod` 一致；post-cutoff API 先查 `docs/engine-reference/` 或 WebSearch，勿臆测签名。
