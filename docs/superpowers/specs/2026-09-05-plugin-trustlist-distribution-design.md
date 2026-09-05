# 插件信任清单的联网分发（S1）

> 日期：2026-09-05 ｜ 状态：设计已定稿，待实施
> 上游决策：`docs/superpowers/plans/2026-09-04-open-items-handoff.md` §三 D-2

## 一、这份 spec 解决什么

目标生态是「人人都可以发布插件」：开发者本地生成密钥对，把**公钥**交给我申请登记，我把它写进一份官方信任清单；开发者用自己的私钥签自己的包，用户机器凭那份清单判断「这个包确实来自这个已登记的开发者」。

这套模型有四条已拍板的前提，实施时不要重新讨论：

| # | 决策 | 含义 |
|---|------|------|
| 1 | **开发者自签，我只登记公钥** | 私钥永不离开开发者机器；我不接触插件二进制，也不承担代签风险。我保证的是「来自这个已登记的人」，不是「这个包没问题」 |
| 2 | **内嵌 root 公钥 + 联网拉签名过的清单** | 加开发者、撤销钥匙都不用发新版 |
| 3 | **未签名可装但要用户显式确认；已撤销硬拒** | 属于 S2 的范围，S1 只负责把判定所需的事实供上去 |
| 4 | **清单放 GitHub 仓库文件，root 私钥只在本机** | 零服务器；root 私钥永不上网、永不进仓库、永不进 CI |

**S1 的边界**：本 spec 只做「把一份可信、够新、防回滚的清单送到用户机器上，并解析成内存里的信任集」。谁在什么时候拿它去判一个插件，是 S2 的事。

### 现状与真空

信任链本身已经是完整的，不要重做：

```
plugin.sig  ──签的是──▶  plugin.json 的原始字节
                              │
                              └── 里面的 sha256 钉住 plugin.wasm
                                        │
                                        └── LoadPackage 逐字节比对（internal/plugin/manifest/assemble.go:110）
```

`internal/plugin/sign` 已有 Ed25519 密钥环、撤销、`ParseKeyring`/`ParseSignature`/`Verify`/`Sign`/`GenerateKey` 与全套 `Marshal*`；`agent plugins keygen`/`sign` 可用；loader 有 `SignaturePolicy` 并在 reload 时比对。

真空只有一处：**keyring 今天是 `plugins.keyring` 指向的一个本地文件路径，运维手工放一份**。没有任何机制让它到达用户机器、保持更新，或让一次撤销传播出去。

另外记下一条容易记反的事实：`plugins.require_signature` 的**未声明值是「要求签名」**（`internal/config/config.go` 的 `SignatureRequired`），不是「关闭」；而强制签名时 `keyring` 必须能读能解析，否则 serve 拒绝启动（`cli.resolvePluginKeyring`）。

---

## 二、文档格式

### 2.1 发布物

仓库里发布两个文件（路径见 §6 配置）：

```
trustlist.json
trustlist.sig
```

`trustlist.json` 的形状：

```json
{
  "serial": 7,
  "issued_at": "2026-09-05T02:00:00Z",
  "expires_at": "2026-10-05T02:00:00Z",
  "keyring": {
    "keys": [
      { "id": "dev-abc", "algorithm": "ed25519", "public_key": "<base64 32 字节>" }
    ],
    "revoked": [
      { "key_id": "dev-old", "revoked_at": "2026-08-29T10:00:00Z", "reason": "私钥泄漏" }
    ]
  },
  "publishers": [
    { "key_id": "dev-abc", "display_name": "张三", "contact": "zhangsan@example.com" }
  ]
}
```

`trustlist.sig` 就是 `sign.MarshalSignature` 产出的那种文档，签的是 **`trustlist.json` 的原始字节**：

```json
{ "key_id": "root-2026", "algorithm": "ed25519", "signature": "<base64 64 字节>" }
```

### 2.2 为什么是「信封套 keyring」，而不是给 keyring 加字段

`keyring` 字段以 `json.RawMessage` 保存，**验签通过后把那段原始字节原样喂给 `sign.ParseKeyring`**，中间不做一次 re-marshal。re-marshal 会让「验签验的字节」与「解析出信任集的字节」成为两份东西，字段顺序或数字格式的任何差异都会让二者悄悄分家——这正是签名要消灭的那类不确定性。

考虑过的替代方案是直接给 `sign` 包的 `rawKeyring` 增加 `serial` / `publishers` 字段。**否掉**，两个理由：

1. `ParseKeyring` 用了 `DisallowUnknownFields`，而部署方手写的本地 keyring 不会有 `serial`——加了字段以后要么逼所有本地文件跟着改，要么把字段做成可选从而丧失「缺 serial 就是格式错」的判据。
2. `sign` 包的职责是密码学原语与信任集本身，分发元数据（第几版、什么时候过期、谁是发布者）不属于它。混进去以后，一个只想验签的调用方也得懂 serial 语义。

信封分层的结果是：**`internal/plugin/sign` 一行不改，本地手工 keyring 的既有路径一行不改。**

### 2.3 `publishers`

只服务 S2 的界面显示（「此插件来自 张三」）。S1 对它的唯一要求是**完整性**：

- 每个条目的 `key_id` 必须出现在 `keyring.keys` 里。悬空的显示名意味着这份清单是手拼的、或指向了一把已经删掉的钥匙——**整份拒绝**，不是丢弃那一条。
- `display_name` 非空。
- 同一个 `key_id` 不得出现两次（否则「这个包属于谁」没有确定答案）。
- 已撤销的 key 允许保留 publisher 条目：S2 要能说出「张三的这把钥匙已被撤销」，而不是「未知钥匙」。

`contact` 可以为空。

### 2.4 解析规则

沿用 `sign` 包已经定下的严格性，逐条照抄它的理由：

- `json.Decoder.DisallowUnknownFields()`，任意嵌套层级——写错的字段名必须解析失败，不能悄悄不生效。
- 单个 JSON 文档之后不允许有非空白内容。
- `serial` 必须 ≥ 1。0 与负数是错误，不是「未设置」。
- `issued_at` / `expires_at` 必须是 RFC 3339，且 `expires_at` 必须晚于 `issued_at`。
- `keyring` 字段必须存在且能通过 `sign.ParseKeyring`——注意 `ParseKeyring` 自己会拒绝空的 `keys`、拒绝「每把钥匙都被撤销」，那两条错误原样往上冒。

---

## 三、root 公钥：内嵌一份单 key 的 keyring 文档

用 `//go:embed` 嵌入 `internal/plugin/trustlist/root_keys.json`，内容就是一份普通的 keyring 文档：

```json
{ "keys": [ { "id": "root-2026", "algorithm": "ed25519", "public_key": "<base64>" } ] }
```

用 keyring 文档格式而不是一个裸 base64 常量，为两件事：

1. **复用 `ParseKeyring` 的全部校验**（长度、base64、算法名、重复 id）。内嵌数据也会被写坏，而写坏内嵌 root 公钥的后果是所有用户机器同时拒绝所有清单。
2. **root 轮换**。将来换 root 私钥时，可以同时内嵌新旧两把公钥，让新旧两份清单在过渡期都验得过；换完再删掉旧的。裸常量做不到这一点，只能靠一次性的硬切。

包初始化时解析一次，解析失败 **panic**——这是编译期就该正确的数据，运行期降级没有任何安全含义上的意义。

root 私钥**只存在于我本机**，不进仓库、不进 CI secrets、不上任何联网主机。§7 的入册流程是本地跑一条命令重签清单再提交。

---

## 四、防回滚：`serial` 单调

对这套机制最便宜的攻击不是伪造清单（伪造要 root 私钥），而是**重放一份旧清单**：把用户挡在某个撤销发生之前的版本上，让刚泄漏的钥匙继续有效。旧清单的签名是完全合法的。

所以客户端持久记录**见过的最大 serial**，收到的清单 `serial` 小于它就**拒绝**，且不给任何回落余地——不是「过期回落」，是直接不接受这份文档。

- `serial` 相等：接受，但视为无变化（不重写缓存）。相等且内容不同，说明发布侧改了内容却没进 serial，属于发布流程事故；此时**拒绝并报 error**，因为无法判断哪一份才是当前的。

「见过的最大 serial」不单独存一个文件——它就是缓存里那份 `trustlist.json` 的 `serial`（见 §5.2）。这带来一个必须写清楚的代价：**缓存损坏时 serial 保护随之失效**，攻击者可以在那之后喂一份旧清单。可以接受，因为损坏不会波及 `revoked-ever.json`（§5.1，独立文件、只增不减），而回滚攻击的目标——让撤销失效——正是被那个累积集挡住的。真正丢掉的只是「登记状态回到旧版本」，代价远小于为一个可能与清单不一致的额外文件多引一处失配来源。

---

## 五、缓存、过期与撤销

### 5.1 撤销永不遗忘

`expires_at` 过期**不作废**清单：断网时若清单作废，所有插件立刻失信，可用性代价大于收益。但过期还能用，就意味着**断网可以绕过撤销**——把机器断网，撤销永远到不了。

解法：撤销条目**见过一次就永久生效**。

维护一个本地累积集 `revoked-ever.json`。每次成功接受一份清单，把它 `keyring.revoked` 里的全部条目并入这个集合，只增不减。装配最终信任集时，这个累积集与当前清单的 revoked 取**并集**。

于是：

- 清单过期、断网、甚至此后永远拉不到——已经见过的撤销依然生效。
- 某个 key_id 从后续清单的 `revoked` 里消失（发布侧误删、或被诱导删除）——它在本机仍然是撤销状态。
- **撤销单调，登记不单调**：一把钥匙可以从清单里被移除从而不再被信任（然后再加回来），但一旦被撤销过就永远撤销。

累积集里的条目保留 `revoked_at` 与 `reason`——`sign.Revocation` 用它们生成人能读的拒绝理由，丢掉就退化成「未知钥匙」。

装配方式：把当前清单的 `keyring` 原始字节与累积集合并成一份新的 keyring 文档，再走一次 `sign.ParseKeyring`。**不要**在 `sign.Keyring` 外面自己维护第二套撤销判断——那就是两个地方各写一遍同一条规则，迟早分家。

（注意 `ParseKeyring` 会拒绝「每把钥匙都被撤销」的信任集。合并后触发这一条是完全可能的真实情况；照常往上报，状态归为 `unavailable`。）

### 5.2 缓存内容

缓存目录下三个文件：

```
trustlist.json      最近一次被接受的清单原始字节
trustlist.sig       它的签名
revoked-ever.json   撤销累积集（形如 { "revoked": [ ...与 keyring 的 revoked 同形... ] }）
```

存**原始字节**而不是解析后的结构：读回来时走的是与网络路径完全相同的解析+验签代码，缓存无法成为一条绕过校验的旁路。「见过的最大 serial」就是这份 `trustlist.json` 里的 `serial`，不单独存（理由与代价见 §4 末段）。

### 5.3 状态

`trustlist` 对外报三种状态：

| 状态 | 含义 |
|------|------|
| `fresh` | 手上这份清单验签通过且未过 `expires_at` |
| `stale` | 验签通过但已过 `expires_at`（多半是长期断网）。信任集照常可用，撤销照常生效 |
| `unavailable` | 从未成功取得过任何清单，或缓存损坏且这次也没拉到 |

`unavailable` 时**不返回任何信任集**。S2 收到它必须把所有插件按「未登记」处理——绝不能按「已登记」放行。这一条写在这里，是因为反过来做的诱惑很实在（「拉不到就先放行吧」），而那等于给了攻击者一个把清单打掉就全线放行的开关。

### 5.4 损坏与部分写入

- 三个文件任一解析失败、验签失败、或 `trustlist.sig` 与 `trustlist.json` 对不上：**当作无缓存**，报 error，不静默重建、不删除（保留现场供排查）。
- 写缓存用「写临时文件 → fsync → rename」，且**先写 `revoked-ever.json`，成功后再写清单**。顺序反了会出现「清单已更新但撤销没记下」的窗口，而这个方向的丢失正是 §5.1 要防的那件事。
- 多进程并发写：复用 `internal/plugin/fetch` 已有的目录锁思路。**Windows 上必须注意 delete-pending 语义**：一个正在被删除的文件名，`CreateFile`/`os.Stat` 返回的是 `ERROR_ACCESS_DENIED`（errno 5），既不是 `ErrExist` 也不是 `ErrNotExist`。`fetch/cache_lock_windows.go` 与 `taskledger/lock_contention.go` 各有一份已经修对的判据，**照抄之前先读被抄那份的锁策略**：fetch 那份会等待所以把 `ErrExist` 折进争用；taskledger 那份不等待，所以刻意把 `ErrExist` 排除在外，否则被杀死的进程留下的锁永远无法回收。本包属于哪一种，实施时按实际的等待策略决定，并在注释里写明选了哪一种、为什么。

---

## 六、取回与配置

### 6.1 为什么不能复用 `fetch.Fetch`

`fetch.Fetch(ctx, client, u, digest, limits)` 要求调用方**预先知道内容的 digest**——它是为「manifest 里钉死了 digest 的插件产物」设计的。信任清单本来就是可变的，没有预知的 digest 可传。

复用它的**形状**，不复用它的函数：

- 响应体上限（读到上限+1 字节即判定超限，而不是读完再判断）
- 显式超时
- 最多 9 跳重定向；**原始 scheme 是 https 时，每一跳目标也必须是 https**
- 非 2xx 一律是错误，且错误里带上状态码（GitHub 对带无效凭据的请求返回 **404 而不是 401**，不把状态码写进错误信息会让鉴权问题伪装成「文件不存在」）

清单 URL **必须是 https**，不受 `plugins.allow_insecure_sources` 影响：那个开关放宽的是插件产物的取回方案，而产物有 digest 兜底；清单没有。

### 6.2 配置

`plugins` 下新增：

```json
"trustlist": {
  "url": "https://raw.githubusercontent.com/jxncyjq/stardust-agent-server/master/trust/trustlist.json",
  "cache": "/var/lib/legion-agent/trustlist",
  "refresh_interval_ms": 21600000
}
```

- **`url` 为空 = 不启用远程清单**。不做布尔开关：沿用 `plugins.keyring` 用空串表达「没配」的既有做法，避开「未声明的布尔值该取哪一边」的歧义。这里那个歧义没有安全上的正确答案——联网能拿到撤销（更安全），不联网不引入新攻击面（也更安全）。
- `trustlist.sig` 的地址由 `url` 推导：同目录、同文件名换后缀 `.sig`。不给它单独一个配置项——两个可以各自配的 URL，就有可以指向两份不匹配文档的配置。
- `cache` 为空但 `url` 非空：配置错误，`Load` 阶段拒绝。清单没有落点就等于每次启动前都有一段完全没有信任集的窗口。
- `refresh_interval_ms` 默认 **21600000（6 小时）**，必须为正。

`url` 指向的 ref 应当是**分支**而不是 commit SHA——与内嵌安装脚本的 `scriptRef` 相反。那边钉 commit 是因为脚本内容必须不可变；这边整份文档由 root 签名保护，内容可变正是它存在的目的，钉死 commit 就等于回到「换清单要发新版」。

### 6.3 何时拉

1. **serve 启动后**在后台拉一次，**不阻塞启动**。启动期网络故障不该让 agent 起不来。
2. 每 `refresh_interval_ms` 一次。
3. `agent plugins trustlist refresh` 手动触发，同步执行并把结果打给操作者看（含状态、serial、下次过期时间）。
4. `agent plugins trustlist show` 打印当前状态、serial、`expires_at`、已登记发布者列表、撤销累积集大小。

按需拉取（挂载插件前若 stale 就先刷新）**不做**：会把一次网络往返塞进插件挂载的关键路径，而 stale 状态本来就是可用的。

### 6.4 上限

- 清单文档：**1 MiB**。远超任何合理的公钥清单规模，又远小于「读进内存会造成麻烦」的量级。
- 签名文档：**4 KiB**。
- 单次取回超时：**30 秒**。

---

## 七、发布侧流程（S1 交付的 CLI 部分）

新增 `agent plugins trustlist sign`，在**我本机**运行：

```
agent plugins trustlist sign \
  --in  trust/trustlist.json \
  --key ~/.legion/root-key.json \
  --out trust/trustlist.sig
```

它做四件事，缺一不可：

1. 解析 `--in`，跑完 §2.4 的全部规则（包括 publisher 完整性）。**格式不对就不签**——签一份验证方会拒绝的文档，会教育所有人「验签是坏的」，比不签更糟。
2. 检查 `serial` 严格大于 `trust/trustlist.json` 在 git HEAD 里的那一版的 serial。忘记进 serial 是这条流程里最容易犯、后果又最隐蔽的错（用户机器会拒收，而报错说的是 serial 回退，看着像被攻击）。
3. 用 `--key` 签，写出 `--out`。
4. **立刻用内嵌的 root 公钥把自己刚写出的东西验一遍**。`sign.ParsePrivateKey` 的注释已经写明：它不检查私钥的两半是否自洽，一份手工编辑过的私钥能愉快地签出谁也验不过的签名。自验是唯一能当场发现这件事的地方。

入册一个新开发者的完整动作：把他的公钥条目加进 `trust/trustlist.json` 的 `keys`、把显示名加进 `publishers`、`serial` 加一、`issued_at`/`expires_at` 顺延，跑上面那条命令，提交推送。撤销一把钥匙同理，加进 `revoked` 即可（**不要**从 `keys` 里删——`sign` 包保留公钥正是为了让拒绝理由能说出「这把钥匙曾被信任，现在不是了」）。

开发者侧的申请与打包流程属于 S3，本 spec 不展开。

---

## 八、包边界与接口

新包 `internal/plugin/trustlist`。它**不知道插件是什么**——不 import `manifest`、不 import `loader`、不 import `host`。它认识的只有 `sign`、`net/http` 和文件系统。

```go
// Publisher 是一个已登记发布者的展示信息，S2 用它回答「这个包是谁的」。
type Publisher struct {
    KeyID       sign.KeyID
    DisplayName string
    Contact     string
}

// Status 是手上这份清单的时效状态，见 §5.3。
type Status int
const (
    StatusUnavailable Status = iota // 从未成功取得，或缓存损坏且这次也没拉到
    StatusStale                     // 验签通过但已过 expires_at
    StatusFresh
)

// Trust 是一次装配的结果：信任集、发布者名录，以及它有多新。
// Status 为 StatusUnavailable 时 Keyring 为 nil、Publishers 为空。
type Trust struct {
    Keyring    *sign.Keyring
    Publishers map[sign.KeyID]Publisher
    Status     Status
    Serial     int64
    IssuedAt   time.Time
    ExpiresAt  time.Time
}

// Store 持有缓存目录与取回配置，是这个包唯一有状态的类型。
type Store struct{ /* ... */ }

func NewStore(cfg Config) (*Store, error)

// Current 返回当前信任状态，只读缓存，绝不发起网络请求。
// 挂载插件这类热路径调用它。
func (s *Store) Current() (Trust, error)

// Refresh 取回一次并在通过全部校验后落盘，然后返回新的状态。
// 网络失败时返回缓存的状态与一个 error——两者都要返回，调用方
// 既需要知道拉取失败了，也需要继续拿着能用的那份干活。
func (s *Store) Refresh(ctx context.Context) (Trust, error)
```

哨兵错误：

```go
// ErrSerialRegressed：收到的清单 serial 不大于本地已见的最大值。
// 是哨兵，因为它与「网络故障」是完全不同的事——它意味着有人在喂旧清单，
// 值得比一次超时高得多的告警级别，而且重试绝不会让它变好。
var ErrSerialRegressed = errors.New("trustlist serial regressed")

// ErrUntrustedList：验签不过、publisher 悬空、格式非法——这份文档不可信。
// 与 ErrSerialRegressed 一样不该重试。与之相对，网络与文件 I/O 错误
// 不裹这个哨兵：重试它们是有意义的。
var ErrUntrustedList = errors.New("trustlist is not trusted")
```

`Refresh` 同时返回 `Trust` 和 `error` 是有意的：调用方两件事都需要知道。这里不用「返回零值 + error」的常规写法，因为零值状态（`StatusUnavailable`）在 S2 里有明确的安全含义（一律按未登记），把一次网络超时渲染成它会造成一次不必要的全线降级。

---

## 九、测试

全部用 `httptest` + 测试内生成的 root 密钥对。**不打真实网络**。

必须存在、且在对应缺陷下必红的用例：

| 用例 | 断言 |
|------|------|
| 篡改 `trustlist.json` 一个字节 | 拒绝，错误裹 `ErrUntrustedList`，缓存未被覆盖 |
| 用非 root 的钥匙签 | 拒绝（key_id 不在内嵌 root keyring 里） |
| `serial` 回退 | 拒绝，错误裹 `ErrSerialRegressed`；缓存与 `revoked-ever` 均未变 |
| `serial` 相等但内容不同 | 拒绝 |
| `publishers` 里有 `keys` 中不存在的 key_id | 整份拒绝（不是丢弃那一条） |
| 响应体超过 1 MiB | 在读完之前就拒绝 |
| 非 200（含 404） | 错误信息里出现状态码 |
| http:// 的 URL | 拒绝，与 `allow_insecure_sources` 无关 |
| https 重定向到 http | 拒绝 |
| 断网（服务器关掉）且有缓存 | 返回缓存的 `Trust` **和** 一个 error |
| 断网且无缓存 | `StatusUnavailable`，`Keyring == nil` |
| **撤销遗忘**：清单 A 撤销 K，清单 B（serial 更大）的 `revoked` 里没有 K | K 仍被拒；`sign.Verify` 报的是撤销而不是「未知钥匙」 |
| 撤销过的 key 从 `keys` 里也消失了 | K 仍被拒，且理由仍是撤销 |
| 过期清单 | `StatusStale`，`Keyring` 可用，撤销照常生效 |
| 缓存文件损坏（截断 / 改一字节 / `.sig` 与 `.json` 不配对） | 当作无缓存，报 error，**文件保留不删** |
| `revoked-ever.json` 损坏 | 报 error，不静默重建 |
| 内嵌 root keyring 写坏 | 包初始化 panic（用一个可注入的解析入口测，不是真的把内嵌文件改坏） |
| `trustlist sign` 拿到格式非法的输入 | 不产出 `.sig` 文件 |
| `trustlist sign` 拿到两半不自洽的私钥 | 自验失败，不产出 `.sig` 文件 |
| 合并后信任集为空（所有 key 都被撤销） | `ParseKeyring` 的既有错误原样上报，状态 `unavailable` |

并发：多个 goroutine 同时 `Refresh` + `Current`，`-race` 必须干净；Windows 上跑 `-count=20`（§5.4 的锁争用窗口是概率性的，单次通过说明不了任何事——上一期在 `fetch` 上正是「重跑 3 次没复现」把一个 2% 概率的真实缺陷放过去的）。

---

## 十、S1 明确不做

- **分级安装体验**（未签名要用户确认、界面显示发布者名、已撤销硬拒）——S2。`manifest.LoadPackage` 今天是二选一（`keyring == nil` 完全不验 / 非 nil 必须有有效签名，`assemble.go:94`），改成能表达第三态是 S2 的核心改动。
- **开发者申请与打包流程文档**、公钥提交格式、审核标准——S3。
- **root 密钥轮换的实际执行**。§3 保证格式上支持（可同时内嵌多把），但轮换流程本身不在本期。
- **清单的传输加密之外的隐私考量**（谁在什么时候拉了清单对 GitHub 可见）。可接受。
- **`plugins.keyring` 本地文件路径的移除**。它继续可用，且与远程清单是两条独立的路：本地 keyring 服务于内网/离线部署，远程清单服务于公开生态。二者如何共存由 S2 决定优先级，S1 不动它。
