package trustlist

import (
	"fmt"
	"os"
	"time"
)

// publishRename 把临时文件改名成它要发布的那个名字，目标名正被谁开着时等待重试。
//
// Windows 上 rename 覆盖一个已存在的名字，要求那个名字**此刻没有任何打开的句柄**：
// 覆盖式改名要对目标取 DELETE 权限，而这道检查不看已有句柄的共享模式，所以一个纯
// 读的句柄就足以让 rename 失败于 ERROR_ACCESS_DENIED（errno 5）。而 read 不取目录
// 锁（理由见 cache.read），「发布的时候有人正开着同一个名字」因此是这个包的常规
// 状态，不是异常。
//
// 这是实测出来的，不是推测：4 个 goroutine 用 os.ReadFile 反复读同一个名字、另一
// 侧反复发布同名文件，2000 次 rename 里 1988 次拿到 errno 5；同样的负载撤掉读者之
// 后，400 次并发发布零失败。让读端带 FILE_SHARE_DELETE 打开**解决不了**它——探针
// 实测，带 FILE_SHARE_DELETE 的读句柄照样让 rename 失败，因为拦下它的不是共享模式
// 那道检查。
//
// 所以这里等，与 lock 对争用的取法同一个策略（见 lock）：一次刷新完全可以等一会
// 儿，而把争用报成失败会让一次本来没问题的刷新随机失败。
//
// 非 Windows 上 renameIsContendedOnThisPlatform 恒为 false，这个循环只跑一趟。
func publishRename(from, to string) error {
	return publishRenameWithin(from, to, publishWait, publishPoll)
}

// publishRenameWithin 是 publishRename 的实现，只把等待时限与重试间隔挪成参数。
// 两者都必须是正数：0 的间隔会让这个循环烧掉一整个核。
//
// 拆出这一层唯一的理由是可测：「等满时限之后必须报出来，而且要带上最后那次 rename
// 的错误」是这段代码里唯一一条防止争用重试变成无限挂起的规则，而一个真等
// publishWait 的用例没人会跑，于是那条规则就没人守着。用例调用本函数并传一个极短
// 的时限；生产路径走 publishRename，不走这里。
//
// 不选「把 publishWait 改成包级 var + 测试钩子」那条路：包级可变量会把「哪些用例
// 不能并行」变成一条没人写下来的纪律。
//
// 等满时限之后报错，且错误里带上最后那次 rename 的错误：折叠 errno 5 的代价是一个
// 真正不许写的位置也要等满时限（见 renameIsContendedOnThisPlatform），所以它必须
// 以权限问题的面目出现，而不是一个幽灵读者。
func publishRenameWithin(from, to string, wait, poll time.Duration) error {
	if wait <= 0 || poll <= 0 {
		return fmt.Errorf("publish rename %s: wait is %s and poll is %s; both must be positive",
			to, wait, poll)
	}
	deadline := time.Now().Add(wait)
	for {
		err := os.Rename(from, to)
		if err == nil {
			return nil
		}
		if !renameIsContendedOnThisPlatform(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (retried for %s: on this platform that error is also what an open "+
				"handle on the destination looks like, so a reader that never lets go and a destination "+
				"this process may not write are told apart only by how long it kept failing)", err, wait)
		}
		time.Sleep(poll)
	}
}
