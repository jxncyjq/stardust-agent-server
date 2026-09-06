package trustlist

import (
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// swapRootKeyring 让一个用例临时把内嵌的 root 信任集换成它自己生成的一把，
// 并在用例结束时还原。
//
// 生产的 root 私钥按设计不在仓库里，所以测试无法用真的 root 签任何东西。
// 换掉信任集是唯一能端到端跑通「签 → 验」的办法，而它只存在于 _test.go 里，
// 生产路径没有任何入口能改动 root。
func swapRootKeyring(t *testing.T, kr *sign.Keyring) {
	t.Helper()
	previous := rootOverride
	rootOverride = kr
	t.Cleanup(func() { rootOverride = previous })
}

// failPublishing 让一个用例把「发布 target 这一步」变成必败：其余文件照常发布，
// 只有 target 那一次改为调用 failure，并把它返回的错误当成这一步的失败。用例结束
// 时还原。
//
// 它存在的唯一理由是 write 的落盘顺序（先累积集、后清单）。那条顺序只有在能把
// 失败点钉在中间某一步时才验得出来：目录权限之类的外部手段分不开「读磁盘上的累积
// 集失败」与「发布清单失败」，于是把顺序反转过来测试照样全绿。failure 拿得到
// *cache，所以用例还能在那一刻对目录动手——比如删掉锁文件，去验 write 怎么报一次
// 失败的解锁。
//
// 生产路径不经过这里：writeFileAtomically 在生产里始终是它自己那个实现，本函数
// 只在 _test.go 里被调用。
func failPublishing(t *testing.T, target string, failure func(c *cache) error) {
	t.Helper()
	previous := writeFileAtomically
	writeFileAtomically = func(c *cache, name string, data []byte) error {
		if name == target {
			return failure(c)
		}
		return previous(c, name, data)
	}
	t.Cleanup(func() { writeFileAtomically = previous })
}
