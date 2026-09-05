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
