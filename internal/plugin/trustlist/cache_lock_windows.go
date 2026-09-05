//go:build windows

package trustlist

import (
	"errors"
	"syscall"
)

// lockCreateIsContendedOnThisPlatform 补上 Windows 对「另一个写者正持有这把锁」
// 的第二种拼法。
//
// Windows 删除一个文件不一定立刻解除它的名字：只要还有句柄开着，这个名字就处在
// delete-pending 状态，而对 delete-pending 的名字 CreateFile 失败于
// ERROR_ACCESS_DENIED（errno 5），**不是**「已存在」。一个写者释放锁（os.Remove）
// 的过程因此是一个窗口，窗口里另一个写者的 create 会对一个逻辑上只是「被占着」
// 的文件看到 errno 5。
//
// 这是实测出来的，不是推测：internal/plugin/fetch 里同一现象的探针在一条路径上
// 用 12 个 goroutine 反复 create/release，看到 611 次 ERROR_ACCESS_DENIED 对
// 28160 次 ErrExist——约 2% 的争用创建。把它读成致命错误，就是让测试与生产在
// Windows 上随机失败的原因。
//
// 折叠 errno 5 的代价：一把真正打不开的锁（ACL 拒绝、只读位置）现在要等满整个
// 等待时限才被报出来，而不是立刻失败。所以超时的错误信息里必须带上最后看到的
// 那个错误——权限问题仍然会浮出来，带着 errno，而不是被报成一个幽灵持锁者。
func lockCreateIsContendedOnThisPlatform(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED
}
