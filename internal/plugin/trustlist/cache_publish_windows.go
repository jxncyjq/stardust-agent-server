//go:build windows

package trustlist

import (
	"errors"
	"syscall"
)

// renameIsContendedOnThisPlatform 判断一次 rename 的失败是不是「目标那个名字此刻
// 正被谁开着」——那是唯一值得等待的情况——而不是等多久都不会好的故障。
//
// Windows 上覆盖式改名要对目标名取 DELETE 权限，而那道检查不看已有句柄的共享模式，
// 于是任何一个打开的句柄（哪怕只读、哪怕带 FILE_SHARE_DELETE）都会让 rename 失败
// 于 ERROR_ACCESS_DENIED（errno 5）。
//
// 它与 lockCreateIsContendedOnThisPlatform 折叠的是同一个 errno，但**不是同一个
// 现象**：那边是一个正在被删除、名字尚未释放的哨兵文件（delete-pending），这边是
// 一个正在被读的目标名。Windows 把两者都答成 errno 5，所以两处都只能靠这一个数认
// 出争用。
//
// 折叠 errno 5 的代价与那边相同：一个真正不许写的位置现在要等满整个等待时限才被
// 报出来。所以超时的错误里必须带上最后那次 rename 的错误（见 publishRename）。
func renameIsContendedOnThisPlatform(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED
}
