# 插件的分级安装体验（S2）

> 日期：2026-09-06 ｜ 状态：设计已定稿，待实施
> 上游：S1「插件信任清单的联网分发」已合入 master（PR #157 / `9c40d18`），
> 设计见 `2026-09-05-plugin-trustlist-distribution-design.md`

## 一、这份 spec 解决什么

S1 把一份可信的信任清单送到了用户机器上，但**没有任何东西消费它**——插件加载路径至今只认
`plugins.keyring` 那个本地文件，而且只会回答「过 / 不过」。

S2 让它落地：**已登记 → 显示开发者名直接装；未签名 → 要操作者显式认下；已撤销 → 硬拒。**

四条已拍板的前提，实施时不要重新讨论：

| # | 决策 | 含义 |
|---|------|------|
| 1 | **清单拉不到时，按安装期记下的结论挂载** | 断网不该让一个正常部署掉光工具。**撤销仍然硬拒**——撤销累积集是本地文件，永不遗忘，不依赖联网 |
| 2 | **确认绑定到那一份字节的摘要** | 插件升级或任何人换掉 wasm，摘要就变了，要重新认。与 `Entry.Digest`、`plugin.json` 的 `sha256` 是同一套思路 |
| 3 | **确认形式是必须显式传的 flag** | 与本仓既有做法一致（`plugins grant` 也是 flag），脚本里能跑、可审计、不会在 CI 里挂住 |
| 4 | **信任集改成动态读，下一次挂载即生效** | 后台刷新拉到的紧急撤销最长 6 小时到达，无需重启 serve |

### 现状与真空

**信任判定今天是二选一。** `manifest.LoadPackage(dir, keyring)`：`keyring == nil` 完全不验，
非 nil 则必须有有效 `plugin.sig`（`assemble.go` 的 `verifyManifestSignature`）。没有第三种答案的位置。

**信任集今天是冻结的。** `loader.keyring` 是 serve 装配时的 `cfg.Keyring`，`SignaturePolicy`
的注释把理由写死了：`plugins reload` 必须比对它，否则「新清单在旧信任集下生效——一个看起来
生效了、实际没有的签名策略」。它甚至为撤销专门加了 `RevokedIDs` 字段。

**这两件事与 S1 直接撞车**：清单每 6 小时后台刷新，而 loader 看不见。那条撤销通路等于没接上，
而它正是整套机制存在的理由。

**安装是 CLI-only。** `agent plugins install` 是唯一入口；HTTP/GUI 只有
`/v1/plugins/{name}/grant`、`/deny`、`/resolve`，都作用于**已在 `plugins.json` 里**的插件。
所以「显式确认」只有一个落点。

---

## 二、信任判定的形状

`LoadPackage` 改成返回一个**判定**，而不是「过 / 不过」：

```go
// TrustState 是一个插件包在这台机器上的信任判定。
type TrustState int

const (
    // TrustUnsigned：没有 plugin.sig，或签名的 key 不在信任集里。
    TrustUnsigned TrustState = iota
    TrustRegistered
    TrustRevoked
)

// Trust 是 LoadPackage 对一个包的信任判定。它只报告，不裁决。
type Trust struct {
    State     TrustState
    KeyID     sign.KeyID // Registered 与 Revoked 时有值
    Publisher string     // 已登记发布者的显示名，来自清单的 publishers
    Reason    string     // Revoked 时：当初写下的撤销理由
    RevokedAt time.Time  // Revoked 时：当初写下的撤销时间
}
```

判据：

| 状态 | 何时 |
|---|---|
| **Registered** | `plugin.sig` 存在、验签通过、key 在信任集里且未被撤销 |
| **Unsigned** | 没有 `plugin.sig`，**或**签名的 key 不在信任集里 |
| **Revoked** | 签名的 key 在撤销集里（**本地判定，不依赖联网**） |

### 为什么「签了但 key 不在信任集」归到 Unsigned

它对用户的意义与「压根没签」**完全相同**：没有任何已登记的开发者为这份字节背书。多一态只会
让策略表多一行而不多一个决定。`Trust.KeyID` 在这种情况下**留空**——那把 key 这台机器不认识，
把它显示出来只会让操作者以为它有意义。

### 边界：LoadPackage 只报告，不裁决

「未签名要不要放行」是调用方的策略。这与它现有注释里那句
「Deciding when a deployment may pass nil is the caller's policy call, deliberately not
this function's」是同一条边界，只是把判定从布尔升成三态。

**签名验证本身仍然必须做**：一个带着 `plugin.sig` 但签名对不上（被篡改）的包，不是 Unsigned，
是**错误**——`LoadPackage` 照旧返回裹 `ErrUntrustedPackage` 的 error。三态说的是「谁为它背书」，
不是「它有没有被改过」。这两件事不能混：前者是策略，后者是完整性。

`plugin.json` 里的 `sha256` 与 `plugin.wasm` 的逐字节比对**一行不改**。

---

## 三、信任集从哪来：并集

S1 明确把「本地 keyring 与联网清单如何共存」留给了 S2。

```
最终登记集 = 本地 keyring 的 keys    ∪ 清单的 keys
最终撤销集 = 本地 keyring 的 revoked ∪ 清单的撤销累积集
```

两者回答的是同一个问题（「这个部署信任哪些签名钥匙」），只是来源不同：一个是运维手工放的
（服务内网 / 离线部署），一个是项目方发布的（服务公开生态）。**登记取并集**——两类插件都该能装；
**撤销取并集**——任何一边说撤销了就是撤销了，撤销单调这条不能因为来源多了就打折。

### 三条不可违反的规则

**① 先合并、后判定。** 不得先用本地 keyring 判一次、拿不到再用清单判一次——那样一个被清单撤销的
key 只要还在本地 keyring 里就能过，撤销被绕过。

**② 装配走 `sign.ParseKeyring`，不自建第二套判断。** 拼一份合并后的 keyring 文档再交给它。
这条规矩在 S1 被评审逼着立过一次，理由是同一条规则写两遍迟早分家，而分家的方向一定是某处忘了
查撤销。

**③ 清单 `StatusUnavailable` 时，并集里只剩本地 keyring 那一半，但撤销累积集仍然可用**——它是
本地文件，S1 保证它永不遗忘、不依赖联网。这是前提 1（断网按安装期结论挂载）能成立的基础。

### 两边都没有时

本地 keyring 未配置**且**清单未配置（`plugins.trustlist.url` 为空）→ 没有信任集。此时
**所有包都判为 Unsigned**，而不是「不验签所以都放行」。判定与放行是两件事：`LoadPackage` 照旧
只报告，放行与否由 §四 的策略决定。

### `require_signature` 在新模型下的含义（**这是一处兼容性断点，必须显式处理**）

`plugins.require_signature` 今天是一个指针，未声明值取**「要求签名」**；显式写 `false` 是这个部署
「我不要求签名」的明确声明，也是 `keyring` 可以为 nil 的唯一途径。

如果 S2 不管它，一个写了 `require_signature: false` 的既有部署在升级后会**所有插件突然停止挂载**
——因为它们全是 Unsigned 且从没人写过 `AcceptedUnsigned`。那是把一个安全增强变成一次静默的服务
中断。

**所以 `require_signature` 继续管「Unsigned 要不要拦」**：

| `require_signature` | Unsigned 且无匹配确认 | Revoked |
|---|---|---|
| 未声明 / `true` | **拒绝挂载**（§4.2 的表） | **硬拒** |
| 显式 `false` | **挂载**，但每次记 Warn 说明它未经任何背书 | **仍然硬拒** |

**撤销不受 `require_signature` 影响**，这条不能松。理由：这个开关说的是「我不要求有人背书」，
而撤销说的是「这把钥匙曾被信任、现在明确不信任」——后者是一条已经做出的判断，不是一个尚未提出
的要求。一个声明「我不要求签名」的部署，并没有声明「我愿意运行被吊销的代码」。

（`require_signature: false` 且两边都没有信任集时，没有任何东西是 Revoked——那是这个配置自己选择
的后果，不是本设计的缺口。）

---

## 四、确认记在哪，运行期怎么判

### 4.1 记录

`plugins.json` 的 `Entry` 增加一个字段，与既有的 `Grant` / `GrantStated` 同一手法：

```go
// AcceptedUnsigned 是操作者在安装期认下的那一份未签名字节的摘要，
// 格式 "sha256:" + 64 位十六进制。空字符串表示从未认过。
AcceptedUnsigned string `json:"accepted_unsigned,omitempty"`
```

**绑定到 `plugin.json` 的字节摘要**，不是 `plugin.wasm`。理由：`plugin.json` 里已经用 `sha256`
钉住了 `plugin.wasm`，而 `LoadPackage` 每次都逐字节比对——钉住 manifest 就等于钉住了代码；
而且**签名保护的也正是这段字节**，两条路径盯的是同一个东西。

### 4.2 运行期判定表（`loader.prepare`，**无人在场**）

| 信任状态 | `AcceptedUnsigned` | 结果 |
|---|---|---|
| Registered | — | **挂载**，日志记 `publisher` 与 `key_id` |
| Unsigned | 有，且摘要与本次读到的 `plugin.json` 一致 | **挂载**，日志记「按安装期确认」与那个摘要 |
| Unsigned | 空 | **拒绝挂载**：从未有人认过它 |
| Unsigned | 有，但摘要对不上 | **拒绝挂载**：这个包变了，认过的是另一份字节 |
| **Revoked** | 有也没用 | **硬拒**，日志带 `key_id`、撤销时间与理由 |

**最后一行是要害：撤销压过一切确认。** 真实场景是——包原本已登记，安装时既没有也不需要
`AcceptedUnsigned`；后来那把 key 被撤销 → 落到 Revoked → 硬拒。这条路走得通，正因为撤销累积集
是本地的。

**「摘要对不上」与「从未认过」必须分开报。** 前者的意思是**这个包变了**，后者是**没人认过它**，
操作者要采取的动作完全不同。合并成一句话会让前者——真正值得警惕的那个——被读成后者。

### 4.3 拒绝挂载的粒度

拒绝的是**这一个插件**，不是整个收敛。其余插件照常挂载，serve 照常服务。理由：一个未认过的
插件不该让整台机器停摆；而它自己的工具消失，是操作者能看见、也能理解的后果。

日志必须**每次拒绝都记**（Warn 级），不能只在第一次记——一个默默不挂载了三个月的插件，与没配
过这个插件是同一件事，而只有日志能把两者分开。

---

## 五、信任集动态读，以及 `SignaturePolicy` 怎么办

### 5.1 loader 不再持有冻结的 keyring

`loader.Config.Keyring *sign.Keyring` 换成一个提供者：

```go
// TrustSet 返回此刻的合并信任集（本地 keyring ∪ 清单），以及清单侧的发布者名录。
//
// 每次挂载都调用它，所以后台刷新拉到的撤销在下一次挂载即生效，无需重启 serve。
// 返回 nil keyring 表示这个部署没有任何信任集——那不是「不验签」，见 §三 末段。
type TrustSet func() (*sign.Keyring, map[sign.KeyID]trustlist.Publisher, error)
```

紧急撤销的到达时间因此变成「刷新间隔（默认 6 小时）+ CDN 翻页（S1 实测 225–280 秒）+ 到下一次
挂载」。

### 5.2 `SignaturePolicy` 的语义收窄

它现在守的是**「本地 keyring 配置变了没」**，不再包含清单那一半。

理由写死在这里：清单本来就会变（每 6 小时），把它放进相等性比较，会让 `plugins reload` 在清单
刚好刷新过的时候随机拒绝收敛——一道随机失败的守卫比没有守卫更糟，因为人会学会忽略它。

`SignaturePolicy` 现有注释里那段论证（「新清单在旧信任集下生效」「被撤销的 key 照样验得过，
屏幕上还写着 reload 成功」）**对本地 keyring 仍然成立**，逐字保留；但必须补一句说明它**不再覆盖
清单那一半**，以及为什么不需要覆盖——清单侧是动态读的，根本不存在「在旧信任集下收敛」这回事。

**这是一处注释必须跟着改的地方**：本仓把注释当契约用，而这段注释在改动之后会变成一句只对一半
成立的话。

---

## 六、CLI 与 GUI

### 6.1 `agent plugins install`（唯一的安装入口）

| 信任状态 | 行为 |
|---|---|
| Registered | 照装，输出显示 `来自 <display_name> (<key_id>)` |
| Unsigned，未传 `--accept-unsigned` | **拒装**。错误说清「没有任何已登记的开发者为这份字节背书」，给出要加的 flag，并显示那份摘要 |
| Unsigned，传了 `--accept-unsigned` | 装，把 `plugin.json` 的摘要写进 `AcceptedUnsigned` |
| **Revoked** | **硬拒**，`--accept-unsigned` 也救不了 |

最后一行的理由：flag 的名字里是 `unsigned`，而撤销不是未签名——**它是「曾经被信任、现在明确不
信任」**。让一个 flag 同时能绕过两件不同的事，是把两个决定塞进一个开关。

### 6.2 GUI：只显示，不新增决定点

`PluginView` 增加三个字段：

```go
TrustState     string `json:"trust_state"`               // "registered" | "unsigned" | "revoked"
TrustPublisher string `json:"trust_publisher,omitempty"` // 已登记发布者的显示名
TrustDetail    string `json:"trust_detail,omitempty"`    // revoked 时：时间与理由
```

`/resolve`、`/grant`、`/list` 三条路都填它。**不新增确认按钮**：安装是 CLI-only，确认在那里已经
发生过；GUI 再加一个确认等于同一个决定有两个真相源。

### 6.3 一个已知的、有意接受的代价

`plugins.json` 是可以手工编辑的。运维手写一条未签名插件的 entry、不带 `AcceptedUnsigned`，
然后从 GUI 授权能力——**授权会成功，但 `loader.prepare` 会拒绝挂载**。

方向是 fail-closed，所以接受；代价是错误出现的位置离原因有点远（在挂载日志里，不在授权响应里）。

**不在 `grant` 里加信任校验**，理由是那会把安装期的策略复制到第二个地方——本仓在 S1 反复吃过
「同一条规则写两遍迟早分家」的亏。作为补偿，`/grant` 的响应与 `/resolve` 一样带上 `trust_state`，
让 GUI 至少**能看见**它正在授权一个未认过的包。

---

## 七、错误与日志

沿用 fail-loud 铁律：不许回落零值、不许吞错误、错误点必须可定位。

新增两个哨兵，**分开**是因为调用方的动作不同：

```go
// ErrUnsignedNotAccepted：包未签名，且没有匹配的安装期确认。
// 可恢复——操作者重新 install 并带上 --accept-unsigned 即可。
var ErrUnsignedNotAccepted = errors.New("plugin package is unsigned and was never accepted")

// ErrRevokedPublisher：签名的 key 已被撤销。
// 不可恢复——没有任何 flag 能绕过它，只能换一个由未撤销的钥匙签的包。
var ErrRevokedPublisher = errors.New("plugin package is signed by a revoked key")
```

**日志里不得出现会误导排查方向的措辞。** S1 在这上面栽过一次（本地缓存损坏被记成
「有人给你喂了假清单」的 ERROR，把人支去查发布侧和中间人）。具体到这里：
「未签名」与「签了但 key 不认识」在**判定上**同归 Unsigned，但**日志文本必须分得开**——
后者意味着有人用一把这台机器不认识的钥匙签了它，那是值得看一眼的事。

---

## 八、测试

必须存在、且在对应缺陷下**必红**的用例：

| 用例 | 断言 |
|---|---|
| 已登记的包 | 挂载，`Trust.Publisher` 是清单里的显示名 |
| 未签名 + 无确认 | 拒绝挂载，错误裹 `ErrUnsignedNotAccepted` |
| 未签名 + 确认摘要一致 | 挂载 |
| 未签名 + 确认摘要**不一致** | 拒绝，且错误文本与「从未认过」**可区分** |
| 签了但 key 不在信任集 | 判为 Unsigned，**且日志文本与「没有 plugin.sig」不同** |
| 签了但签名对不上（篡改） | **不是** Unsigned，是裹 `ErrUntrustedPackage` 的 error |
| key 被撤销 + 有确认记录 | **仍然硬拒**，错误裹 `ErrRevokedPublisher` |
| 撤销只存在于清单侧 | 硬拒（证明清单那一半真的接进了判定） |
| 撤销只存在于本地 keyring | 硬拒（证明本地那一半没被清单覆盖掉） |
| 登记只存在于清单侧 / 只存在于本地 | 两种都能挂载（证明取的是并集不是覆盖） |
| **先合并后判定** | 一个「本地 keyring 里有、清单里被撤销」的 key → 硬拒 |
| 清单 `StatusUnavailable` | 已登记的包按安装期结论挂载；**撤销仍然生效** |
| 两边都没有信任集 | 所有包判为 Unsigned（判定），放行与否由 `require_signature` 决定 |
| `require_signature: false` + Unsigned 无确认 | **挂载**，且**每次**记 Warn（既有部署不被这次改动打断） |
| `require_signature: false` + Revoked | **仍然硬拒**——这个开关管不到撤销 |
| **动态读**：挂载后撤销集变大 | 下一次挂载即拒绝，**无需重启** |
| `SignaturePolicy` | 本地 keyring 变了 → reload 拒绝；**清单刷新了 → reload 不受影响** |
| `install` Revoked | `--accept-unsigned` 救不了 |
| `install` Unsigned 写入的摘要 | 与 `loader` 读回来比对的是**同一段字节**（`plugin.json`），端到端一致 |
| 拒绝挂载的粒度 | 其余插件照常挂载，serve 照常服务 |
| 日志 | 每次拒绝都记，不是只记第一次 |

**接线必须单独守。** 本仓在 S1 里六次撞上「接缝在，但没人测那条接缝」。至少这三条接线要有各自
必红的用例：`LoadPackage` 的三态真的被 `loader.prepare` 消费了；`TrustSet` 提供者真的在每次挂载
时被调用（而不是只调一次）；`install` 真的把摘要写进了 `plugins.json`。

---

## 九、S2 明确不做

- **GUI 的安装入口**。安装仍然是 CLI-only。
- **在 `grant` 里做信任校验**（§6.3 的理由）。
- **交互式确认提示**。已拍板用 flag（前提 3）。
- **给「缓存自身不可信」拆单独哨兵**。这是 S1 留给下一棒的账（`ErrUntrustedList` 语义过载），
  与 S2 无关，不要顺手做。
- **`sign.ParseKeyring` 的重复 JSON 键盲区**。S1 已判定那条路被 root 签名保护，留给下一棒。
- **开发者申请流程与文档**。那是 S3。
