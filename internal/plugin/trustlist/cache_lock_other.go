//go:build !windows

package trustlist

// lockCreateIsContendedOnThisPlatform 在非 Windows 上恒为 false。
//
// POSIX 的 O_CREAT|O_EXCL 只用 EEXIST 回答「文件已经在那儿」，而一个名字在被
// 删除的那一刻就解除了，没有 Windows 那种「名字正在被删除」的中间态。EACCES
// 说的就是权限被拒：把它折进争用，等于让一个目录权限配错的部署白等满整个等待
// 时限，然后报出一个误导性的「锁竞争」而不是「你没有写权限」。
func lockCreateIsContendedOnThisPlatform(error) bool { return false }
