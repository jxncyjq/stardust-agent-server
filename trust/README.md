# 官方插件信任清单的发布目录

这里**目前是空的**，只有一份 `.gitattributes`。这是有意的：

`sign.ParseKeyring` 拒绝空的 `keys`，所以「一份不登记任何人的初始清单」在当前设计下是非法的
——而在第一位真实开发者出现之前，也确实没有人可登记。发布一份只含占位条目的清单，等于在
正式信任清单里放一个假发布者，与这套机制存在的理由相反。

Task 9 的真机验证曾在这里发布过 serial 1–4（见
`docs/superpowers/plans/2026-09-05-plugin-trustlist-distribution.md` 末尾的「真机验证记录」），
验证结束后撤下，验证用的开发者私钥已销毁。

## 真要发布时，按这个来

1. 开发者本地 `agent plugins keygen`，只把**公钥条目**交过来（私钥永不离开他的机器）。
2. 把条目加进 `trustlist.json` 的 `keys`，把显示名加进 `publishers`，`serial` **加一**，
   `issued_at` / `expires_at` 顺延。
3. 用 root 私钥签：

   ```
   agent plugins trustlist sign --in trust/trustlist.json \
     --key ~/.legion/root-key.json --out trust/trustlist.sig
   ```

   它会先跑完整套格式校验、再签、再**用内嵌 root 公钥自验**；任何一步不过就不写 `.sig`。
4. 提交推送。

撤销一把钥匙同理：加进 `revoked` 段，**不要**从 `keys` 里删——`sign` 包保留公钥，正是为了让
拒绝理由能说出「这把钥匙曾被信任，现在不是了」，而不是退化成「未知钥匙」。

## 三条真机验证撞出来的坑

- **`serial` 必须严格递增。** 忘记进 serial 是这条流程里最容易犯、后果又最隐蔽的错：用户机器会
  拒收，而报错说的是 serial 回退，看着像被攻击。
- **字节必须逐字节稳定。** `trustlist.sig` 签的是 `trustlist.json` 的确切字节。`.gitattributes` 里的
  `-text` 就是为此——行尾一旦被转换，症状是所有用户机器同时报「清单不可信」，而排查方向会被指向
  发布侧或中间人。
- **GitHub raw 有 CDN 缓存，且边缘节点会不一致。** 实测推送到各节点翻页要 225–280 秒，期间不同
  用户可能拿到不同版本。紧急撤销的到达时间要把这一段算进去。
