package trustlist

import (
	"errors"
	"io/fs"
)

// lockCreateIsContended 判断一次 O_CREATE|O_EXCL 的失败是不是「另一个写者此刻
// 正持有锁」——那是唯一值得等待的情况——而不是等多久都不会好的故障。
//
// 本包**采取等待策略**：一次刷新完全可以等一会儿，而把争用误判为致命会让一次
// 本来没问题的刷新随机失败。所以「文件已存在」这个常规答案计入争用。
//
// 注意 internal/taskledger 里那份形状几乎一样的判据**语义相反**：它不等待
// （2 次尝试 + mtime 陈旧判定），所以刻意把 ErrExist 排除在争用之外，否则被
// 杀死的进程留下的锁永远无法回收。照抄任何一份之前先读它的等待策略。
func lockCreateIsContended(err error) bool {
	if errors.Is(err, fs.ErrExist) {
		return true
	}
	return lockCreateIsContendedOnThisPlatform(err)
}
