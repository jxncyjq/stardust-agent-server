//go:build !windows

package trustlist

// renameIsContendedOnThisPlatform 在非 Windows 上恒为 false。
//
// POSIX 的 rename(2) 不看目标名有没有打开的句柄：目标被原子地替换，还开着旧文件的
// 读者继续读它自己那一份，直到关掉——没有可等的争用。EACCES 在这里说的就是这个进程
// 不许在这个目录里改名，把它折进重试等于让一个权限配错的部署白等满整个等待时限，
// 然后报出一个误导性的「读者不放手」而不是「你没有写权限」。
func renameIsContendedOnThisPlatform(error) bool { return false }
