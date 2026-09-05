package trustlist

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublishUnderConcurrentReaders_NeverFailsSpuriously 是发布路径对并发读者的
// 承诺：read 不取目录锁（理由见 cache.read），所以一次发布几乎总是在有人正读同一
// 个名字的时候进行，而那不许让写失败。
//
// 这条规则是被咬出来的。修前，全包 -cpu=1 -count=10 跑出来的是：
//
//	cache_test.go:218: write: publish trustlist.sig:
//	    rename …\trustlist.sig.tmp-1794501151 …\trustlist.sig: Access is denied.
//
// Windows 上覆盖式改名要求目标名此刻没有打开的句柄，一个纯读的句柄就够让它失败于
// errno 5（见 renameIsContendedOnThisPlatform）。TestConcurrentWritesNeverLoseARevocation
// 只是碰巧撞上它——它守的是「撤销不丢」，撞见这个窗口时给出的错误跟撤销毫无关系。
// 所以单写一个用例，直接把读者压在发布上。
//
// 非 Windows 上它恒绿（rename(2) 不看目标名的打开句柄）。留着是因为它描述的是这个
// 包对并发的承诺，不是某一个平台的怪癖。
func TestPublishUnderConcurrentReaders_NeverFailsSpuriously(t *testing.T) {
	const (
		readerGoroutines = 2
		publishes        = 60
	)
	if testing.Short() {
		t.Skip("并发读者 hammer：几百次发布压在持续的读上，-short 下跳过")
	}
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("首次 write: %v", err)
	}

	// 读者走的是生产的 read，不是一个裸的 os.ReadFile：要压住的是这个包自己的
	// 读路径，而不是一个比它更凶的合成负载。
	var stop atomic.Bool
	var readers sync.WaitGroup
	for g := 0; g < readerGoroutines; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				// 读到一份配不上对方的清单是允许的（见 cache.read），所以这里
				// 不看 read 的错误——这个用例守的是**写**不许因读者而失败。
				_, _, _ = c.read()
			}
		}()
	}
	defer func() {
		stop.Store(true)
		readers.Wait()
	}()

	// 每次 write 发布三个文件，所以这是 180 次改名压在持续的读上。修前一次
	// TestConcurrentWritesNeverLoseARevocation（只有两个 goroutine 各读各写一次）
	// 就能撞红，这里的密度远在其上。
	//
	// 不设内部时限：一个「跑不完就算过」的用例会在变慢的那天悄悄什么都不测。
	// 跑不完该由 go test 的 -timeout 报出来。
	for i := 0; i < publishes; i++ {
		if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
			t.Fatalf("第 %d 次发布撞上并发读者就失败了：%v", i, err)
		}
	}
}

// TestPublishRenameReportsAFailureThatWillNotClear：不是争用的 rename 失败必须
// **立刻**报出来，不能被吸进重试循环里白等满整个时限。
//
// 这一条与「争用就等」是同一枚硬币的两面：把真正的故障误判为争用，操作者拿到的
// 是一句迟到的、说错了原因的话。
func TestPublishRenameReportsAFailureThatWillNotClear(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	start := time.Now()
	err := publishRenameWithin(filepath.Join(dir, "nope.tmp"), filepath.Join(dir, "trustlist.sig"),
		time.Minute, publishPoll)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("源文件不存在，rename 却成功了")
	}
	if !os.IsNotExist(err) {
		t.Errorf("错误没保住「文件不存在」这个原因：%v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("等了 %v 才报出来：一个不会好转的失败被当成争用了", elapsed)
	}
}

// TestPublishRenameRefusesNonPositiveBounds：等待时限与重试间隔都必须是正数。
//
// poll 为 0 会让重试循环烧掉一整个核直到时限；wait 为 0 则让「等一等」这个名字
// 名不副实。两者都是调用参数的错误，报出来才落在传错的那次调用上。
func TestPublishRenameRefusesNonPositiveBounds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := filepath.Join(dir, "x.tmp")
	if err := os.WriteFile(from, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	to := filepath.Join(dir, "trustlist.sig")
	for _, tt := range []struct {
		name       string
		wait, poll time.Duration
	}{
		{"wait 为零", 0, publishPoll},
		{"poll 为零", publishWait, 0},
		{"wait 为负", -time.Second, publishPoll},
		{"poll 为负", publishWait, -time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := publishRenameWithin(from, to, tt.wait, tt.poll)
			if err == nil {
				t.Fatal("非正的时限/间隔被接受了")
			}
			if !strings.Contains(err.Error(), "must be positive") {
				t.Errorf("错误没点名是参数的问题：%v", err)
			}
			if _, statErr := os.Stat(to); statErr == nil {
				t.Error("参数校验失败了，文件却已经被发布出去")
			}
		})
	}
}
