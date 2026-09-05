package trustlist

import (
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"
)

// TestLockCreateIsContended_ClassifiesTheCreateErrors 钉住 lock 对一次失败的
// create 做的那个判断：「另一个写者正持有它，等」还是「等多久都不会好，报出来」。
//
// 掰错任何一边都有代价，但方向不同：把争用误判为致命，会让一次本来没问题的刷新
// 随机失败；把真正的故障误判为争用，只是白等一个有上限的时限然后照样报出来。所以
// 「文件已存在」这个常规答案计入争用，其余一概不计。
func TestLockCreateIsContended_ClassifiesTheCreateErrors(t *testing.T) {
	t.Parallel()

	if !lockCreateIsContended(fs.ErrExist) {
		t.Error("锁文件已存在是最常见的争用，必须等待而不是报错")
	}
	if !lockCreateIsContended(&os.PathError{Op: "open", Path: "x.lock", Err: fs.ErrExist}) {
		t.Error("裹在 *os.PathError 里的 ErrExist——os.OpenFile 真正返回的形状——必须等待")
	}
	if lockCreateIsContended(fs.ErrNotExist) {
		t.Error("父目录不存在不是争用：等多久都不会好")
	}
	if lockCreateIsContended(fs.ErrInvalid) {
		t.Error("无关的错误不能被吸进等待循环里")
	}
}

// TestLockUnderContention_NeverFailsSpuriously 是 errno 5 那条判据的真实证据。
//
// 本仓在 internal/plugin/fetch 与 internal/taskledger 上各被咬过一次：Windows 上
// 一次 create 撞上另一个 goroutine 对同一个锁文件的删除，返回的是
// ERROR_ACCESS_DENIED 而不是 ErrExist，因为那个名字短暂地处在 delete-pending 状态。
// 在 fetch 那次实测里约占争用创建的 2%——足以让并发用例随机变红，也远没有稀少到
// 可以不管。单次运行永远看不出它，所以只有把锁反复砸一遍才算守住。
//
// 它按 write 用锁的方式砸（取、放、再取）。任何一次失败都算失败：争用之下除了
// 「拿到锁」与「继续等」之外没有第三种正确结果。
func TestLockUnderContention_NeverFailsSpuriously(t *testing.T) {
	if testing.Short() {
		t.Skip("锁争用 hammer：上千次取放，-short 下跳过")
	}
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}

	const goroutines = 8
	const cycles = 250
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*cycles)
	deadline := time.Now().Add(60 * time.Second)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cycles && time.Now().Before(deadline); i++ {
				unlock, err := c.lock()
				if err != nil {
					errs <- err
					return
				}
				if err := unlock(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("一次争用中的取锁没有等待而是失败了：%v", err)
	}
}
