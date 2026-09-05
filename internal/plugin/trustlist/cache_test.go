package trustlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// newSigner 定义在 document_test.go 里（同一个包），直接用。
// 它换一次 root、签多份清单；用了它的用例一律不加 t.Parallel()。

func TestCacheRoundTrips(t *testing.T) {
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	set := newRevokedSet()
	if err := set.mergeFrom(doc.KeyringRaw); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if _, err := c.write(doc, sigData, set); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, backSet, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.Serial != 7 {
		t.Errorf("read().Serial = %d, want 7", back.Serial)
	}
	if backSet.len() != set.len() {
		t.Errorf("撤销累积集往返后 len = %d, want %d", backSet.len(), set.len())
	}
}

func TestCacheReportsNoCacheOnAnEmptyDir(t *testing.T) {
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, _, err := c.read(); !errors.Is(err, errNoCache) {
		t.Errorf("空目录的 read()：err = %v，want 裹 errNoCache", err)
	}
}

// TestCorruptCacheIsNeverSilentlyRepaired：损坏的缓存必须报错、文件必须保留。
//
// 静默重建会抹掉现场，而现场是判断「是磁盘坏了还是有人动过」的唯一依据；
// 静默当成空缓存则更糟——那等于宣告这台机器从没见过任何撤销。
func TestCorruptCacheIsNeverSilentlyRepaired(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{"清单被截断", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "trustlist.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := os.WriteFile(p, data[:len(data)/2], 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"清单被改一个字节", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "trustlist.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			data[len(data)/2] ^= 0x01
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"签名与清单不配对", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "trustlist.sig"),
				[]byte(`{"key_id":"x","algorithm":"ed25519","signature":"`+
					"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="+
					`"}`), 0o600); err != nil {
				t.Fatalf("write sig: %v", err)
			}
		}},
		{"累积集损坏", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "revoked-ever.json"),
				[]byte(`{"revoked":[{"key_id":`), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			signer := newSigner(t)
			list, sigData := signer(7, nil)
			doc, err := VerifyDocument(list, sigData)
			if err != nil {
				t.Fatalf("VerifyDocument: %v", err)
			}
			c, err := newCache(dir)
			if err != nil {
				t.Fatalf("newCache: %v", err)
			}
			if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
				t.Fatalf("write: %v", err)
			}
			tc.corrupt(t, dir)

			if _, _, err := c.read(); err == nil {
				t.Fatal("损坏的缓存被当成有效的读了回来")
			}
			// 文件必须还在：现场不能被抹掉。
			for _, name := range []string{"trustlist.json", "trustlist.sig", "revoked-ever.json"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("%s 在一次失败的 read 之后不见了：%v", name, err)
				}
			}
		})
	}
}

// TestWriteOrdersRevocationsFirst：先写累积集、成功后再写清单。
//
// 顺序反了会出现「清单已更新但撤销没记下」的窗口，而那个方向的丢失正是
// 累积集存在要防的事。这里靠把累积集所在的路径做成不可写来制造失败，
// 然后断言清单没有被更新。
func TestWriteOrdersRevocationsFirst(t *testing.T) {
	dir := t.TempDir()
	signer := newSigner(t)
	list7, sig7 := signer(7, nil)
	doc7, err := VerifyDocument(list7, sig7)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, err := c.write(doc7, sig7, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 让累积集写不进去：把它变成一个目录。
	if err := os.Remove(filepath.Join(dir, "revoked-ever.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "revoked-ever.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	list9, sig9 := signer(9, nil)
	doc9, err := VerifyDocument(list9, sig9)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	if _, err := c.write(doc9, sig9, newRevokedSet()); err == nil {
		t.Fatal("累积集写不进去时 write 却成功了")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "trustlist.json"))
	if err != nil {
		t.Fatalf("read trustlist.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(onDisk, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["serial"].(float64) != 7 {
		t.Errorf("累积集写失败之后清单却被更新到了 serial=%v", m["serial"])
	}
}

// readRetrying 反复调用 read 直到成功，或者试满次数。
//
// read 不取目录锁，所以撞上一次正在进行的 write 时它**允许**失败——那是 read 的
// 契约明写的代价，「这个窗口只有一次 write 那么长，且下一次 read 就好了」（见
// cache.read）。撞上的形态不止一种：清单与签名之间读到一对配不上的文件会验签失败，
// 而在 Windows 上，一次覆盖式改名对目标名的短暂占用会让读者的 open 失败于
// ERROR_SHARING_VIOLATION。实测两个读者压着一个写者跑 15 秒，约 7 万次 read 里有
// 几十次落在这个窗口里（这个比例与发布是否重试无关，两种实现下相同）。
//
// 所以并发用例里的 read 必须**重试**而不是直接判失败：一次落在窗口里的 read 说明
// 不了任何事，而把它当成失败会让用例报出一句与它真正要守的规则毫无关系的话。重试
// 而不是忽略错误：试满还不行就如实报出来，那才是真出了问题。
func readRetrying(t *testing.T, c *cache) (Document, *revokedSet, error) {
	t.Helper()

	const attempts = 50
	var err error
	for i := 0; i < attempts; i++ {
		var doc Document
		var set *revokedSet
		doc, set, err = c.read()
		if err == nil {
			return doc, set, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return Document{}, nil, fmt.Errorf("试了 %d 次都没读成功，这不是一次落在写窗口里的失败: %w",
		attempts, err)
}

// TestConcurrentWritesNeverLoseARevocation：两个写者各自并入不同的撤销，
// 并发写完之后两条都必须还在。
//
// 累积集是 read-modify-write，没有锁就会丢更新，而丢掉的正好是撤销。
func TestConcurrentWritesNeverLoseARevocation(t *testing.T) {
	dir := t.TempDir()
	c, err := newCache(dir)
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
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []sign.KeyID{"gone-a", "gone-b"} {
		wg.Add(1)
		go func(id sign.KeyID) {
			defer wg.Done()
			_, set, err := readRetrying(t, c)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if err := set.mergeFrom(keyringWith(t, []sign.KeyID{id}, []sign.KeyID{id})); err != nil {
				t.Errorf("mergeFrom: %v", err)
				return
			}
			if _, err := c.write(doc, sigData, set); err != nil {
				t.Errorf("write: %v", err)
			}
		}(id)
	}
	wg.Wait()

	_, set, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if set.len() != 2 {
		t.Errorf("并发写之后累积集有 %d 条，want 2——有撤销被丢了", set.len())
	}
}

// TestWriteKeepsRevocationsAlreadyOnDisk 是上面那个并发用例的确定性版本：
// 拿一份**过期的**内存快照去写，磁盘上此前记下的撤销不能因此消失。
//
// 并发用例只能概率性地撞上这个窗口；这一个把窗口写死了——快照是在 gone-a
// 落盘之前读出来的，正是一个慢写者手里会拿着的东西。
func TestWriteKeepsRevocationsAlreadyOnDisk(t *testing.T) {
	dir := t.TempDir()
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}

	// stale 是「另一个写者动手之前」读出来的那份快照。
	_, stale, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// 另一个写者记下了 gone-a。
	_, other, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := other.mergeFrom(keyringWith(t, []sign.KeyID{"gone-a"}, []sign.KeyID{"gone-a"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if _, err := c.write(doc, sigData, other); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 拿过期快照写：gone-b 要进去，gone-a 不能出来。
	if err := stale.mergeFrom(keyringWith(t, []sign.KeyID{"gone-b"}, []sign.KeyID{"gone-b"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if _, err := c.write(doc, sigData, stale); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, back, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.len() != 2 {
		t.Fatalf("落盘的累积集有 %d 条，want 2——过期快照把已记下的撤销抹掉了", back.len())
	}
	for _, id := range []sign.KeyID{"gone-a", "gone-b"} {
		if _, ok := back.entries[id]; !ok {
			t.Errorf("%q 不在落盘的累积集里", id)
		}
	}
}

// TestWriteRefusesWhenTheOnDiskRevocationsAreCorrupt：磁盘上的累积集读不回来时
// write 必须失败，且不能把它覆盖掉。
//
// 覆盖等于用一份读不懂的记录换一份读得懂但更短的——被抹掉的恰好是无从恢复的撤销。
func TestWriteRefusesWhenTheOnDiskRevocationsAreCorrupt(t *testing.T) {
	dir := t.TempDir()
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}

	corrupt := []byte(`{"revoked":[{"key_id":`)
	if err := os.WriteFile(filepath.Join(dir, "revoked-ever.json"), corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err == nil {
		t.Fatal("磁盘上的累积集损坏时 write 却成功了")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "revoked-ever.json"))
	if err != nil {
		t.Fatalf("read revoked-ever.json: %v", err)
	}
	if string(onDisk) != string(corrupt) {
		t.Errorf("损坏的累积集被 write 覆盖了：%q", onDisk)
	}
}

// TestWriteReleasesTheLock：write 返回之后锁必须已经放开，下一次 write 不用等。
//
// 锁没放开的后果不是立刻可见的失败，而是此后每一次写都先等满 lockWait 再报
// 「有人持锁」——一个不存在的持锁者。
func TestWriteReleasesTheLock(t *testing.T) {
	dir := t.TempDir()
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write 返回之后锁文件还在：stat err = %v", err)
	}
	if _, err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("第二次 write: %v", err)
	}
}

// writtenCache 建一个新缓存目录，往里写一份 serial=7 的清单，以及 revoked 里列出
// 的每一条撤销（时间与理由用 keyringWith 的缺省值），返回缓存、目录，以及那份清单
// 与签名——后面要再写一次的用例直接拿去用。
func writtenCache(t *testing.T, revoked []sign.KeyID) (*cache, string, Document, []byte) {
	t.Helper()
	dir := t.TempDir()
	signer := newSigner(t)
	list, sigData := signer(7, nil)
	doc, err := VerifyDocument(list, sigData)
	if err != nil {
		t.Fatalf("VerifyDocument: %v", err)
	}
	c, err := newCache(dir)
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	set := newRevokedSet()
	if len(revoked) > 0 {
		if err := set.mergeFrom(keyringWith(t, revoked, revoked)); err != nil {
			t.Fatalf("mergeFrom: %v", err)
		}
	}
	if _, err := c.write(doc, sigData, set); err != nil {
		t.Fatalf("write: %v", err)
	}
	return c, dir, doc, sigData
}

// TestReadReturnsTheRecordedRevocationsWhenTheListIsGone：清单文件不在、
// revoked-ever.json 还在时，read 报 errNoCache 的同时必须把已经记下的撤销交回去。
//
// 「清单不在而累积集在」不是理论形态：一次首写崩溃、一次磁盘损坏，或者任何能写
// 这个目录的东西删掉一个文件，都会造出它。errNoCache 的语义是「从头重建是安全
// 的」，拿不到这份记录就等于从空集起步，这一轮里一把早已被撤销的钥匙会重新可信
// ——而攻击者连伪造都不必，删一个文件就够了。
func TestReadReturnsTheRecordedRevocationsWhenTheListIsGone(t *testing.T) {
	c, dir, _, _ := writtenCache(t, []sign.KeyID{"gone-a"})
	if err := os.Remove(filepath.Join(dir, listFileName)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	_, set, err := c.read()
	if !errors.Is(err, errNoCache) {
		t.Fatalf("清单不在时 read() 的 err = %v，want 裹 errNoCache", err)
	}
	if set == nil {
		t.Fatal("read() 在 errNoCache 时交回了 nil 累积集——磁盘上记下的撤销被丢掉了，本轮会用空集做信任判定")
	}
	if _, ok := set.entries["gone-a"]; !ok {
		t.Errorf("errNoCache 时交回的累积集里没有 gone-a：%+v", set.entries)
	}
}

// TestReadReturnsNoSetWhenNothingWasEverRecorded：两个文件都不在才是真正的空手，
// 此时累积集必须是 nil。
//
// nil 说的是「这台机器没有任何记录」，空集说的是「记录说没有撤销」。把前者交成
// 后者，就等于替这台机器宣称它从没见过任何撤销——那是 revoked-ever.json 这个文件
// 能造成的最坏的谎。
func TestReadReturnsNoSetWhenNothingWasEverRecorded(t *testing.T) {
	c, err := newCache(t.TempDir())
	if err != nil {
		t.Fatalf("newCache: %v", err)
	}
	_, set, err := c.read()
	if !errors.Is(err, errNoCache) {
		t.Fatalf("空目录的 read()：err = %v，want 裹 errNoCache", err)
	}
	if set != nil {
		t.Errorf("什么都没记过的目录却交回了一个累积集：%+v", set.entries)
	}
}

// TestWriteReturnsTheUnionItPersisted：write 必须把它实际落盘的那份并集交回来。
//
// 调用方手里的 revoked 是它某次 read 出来的快照，而那次 read 不在锁里——另一个
// 进程随时可能在这中间并入新的撤销。不把并集交回去，调用方接下来拿去装配信任集
// 的就是那份偏少的快照，少掉的正好是撤销。
func TestWriteReturnsTheUnionItPersisted(t *testing.T) {
	c, _, doc, sigData := writtenCache(t, []sign.KeyID{"gone-a"})

	// stale 是「gone-a 落盘之前」那种快照：它只知道 gone-b。
	stale := newRevokedSet()
	if err := stale.mergeFrom(keyringWith(t, []sign.KeyID{"gone-b"}, []sign.KeyID{"gone-b"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}

	merged, err := c.write(doc, sigData, stale)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if merged == nil {
		t.Fatal("write 成功却没有交回落盘的并集，调用方手里只剩那份偏少的快照")
	}
	for _, id := range []sign.KeyID{"gone-a", "gone-b"} {
		if _, ok := merged.entries[id]; !ok {
			t.Errorf("write 交回的集合里没有 %q：%+v", id, merged.entries)
		}
	}
	if stale.len() != 1 {
		t.Errorf("write 改动了调用方手里那份快照：len = %d, want 1", stale.len())
	}

	// 交回来的那份必须与磁盘上的逐条相同，不能只是「条数对得上」。
	_, onDisk, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(merged.entries) != len(onDisk.entries) {
		t.Fatalf("write 交回 %d 条，磁盘上 %d 条", len(merged.entries), len(onDisk.entries))
	}
	for id, want := range onDisk.entries {
		if got := merged.entries[id]; got != want {
			t.Errorf("write 交回的 entries[%q] = %+v, want %+v", id, got, want)
		}
	}
}

// TestWriteRecordsRevocationsBeforeItPublishesTheList：write 在发布签名或清单那一
// 步失败时，撤销累积集必须已经落盘。
//
// 顺序反过来会留下「清单已更新但撤销没记下」的窗口，而那个方向的丢失正是累积集
// 存在要防的事。这条只有把失败点钉在中间某一步才验得出来：靠「目录不可写」造出来
// 的失败分不开「读磁盘上的累积集」与「发布清单」，把落盘顺序整个反转过来照样全绿。
func TestWriteRecordsRevocationsBeforeItPublishesTheList(t *testing.T) {
	for _, target := range []string{sigFileName, listFileName} {
		t.Run("发布 "+target+" 失败", func(t *testing.T) {
			c, dir, doc, sigData := writtenCache(t, nil)
			set := newRevokedSet()
			if err := set.mergeFrom(keyringWith(t, []sign.KeyID{"gone-a"}, []sign.KeyID{"gone-a"})); err != nil {
				t.Fatalf("mergeFrom: %v", err)
			}
			failPublishing(t, target, func(*cache) error {
				return errors.New("注入：发布 " + target + " 失败")
			})

			if _, err := c.write(doc, sigData, set); err == nil {
				t.Fatal("注入的发布失败没有让 write 失败")
			}
			data, err := os.ReadFile(filepath.Join(dir, revokedFileName))
			if err != nil {
				t.Fatalf("read %s: %v", revokedFileName, err)
			}
			onDisk, err := parseRevokedSet(data)
			if err != nil {
				t.Fatalf("parseRevokedSet: %v", err)
			}
			if _, ok := onDisk.entries["gone-a"]; !ok {
				t.Errorf("发布 %s 失败时撤销还没落盘：%+v——顺序反了", target, onDisk.entries)
			}
		})
	}
}

// TestReadSeparatesAnIncompleteCacheFromNoCacheAtAll：清单在、另一个文件被删，
// read 必须报错，且**不是** errNoCache。
//
// 这条分界是调用方的开关：errNoCache 意味着从空集重建，其它错误意味着拒绝刷新、
// 保住磁盘上已经记下的撤销。掰反任何一边都是「撤销消失」的方向。
func TestReadSeparatesAnIncompleteCacheFromNoCacheAtAll(t *testing.T) {
	for _, name := range []string{revokedFileName, sigFileName} {
		t.Run("删掉 "+name, func(t *testing.T) {
			c, dir, _, _ := writtenCache(t, []sign.KeyID{"gone-a"})
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatalf("remove: %v", err)
			}
			_, set, err := c.read()
			if err == nil {
				t.Fatalf("缺了 %s 的缓存被当成有效的读了回来", name)
			}
			if errors.Is(err, errNoCache) {
				t.Errorf("缺了 %s 被归类成「还没有缓存」，调用方会据此从空集重建：%v", name, err)
			}
			if set != nil {
				t.Errorf("一次失败的 read 交回了非 nil 的累积集：%+v", set.entries)
			}
		})
	}
}

// TestWriteKeepsTheFirstRecordedRevocation：磁盘上已有 k 的撤销时，再写一份对同一
// 个 key 说法不同的记录，落盘与交回的都必须还是先见到的那条。
//
// 与 mergeFrom 的「先见到的记录胜出」是同一条规则的第二处实现。改成后来者覆盖不会
// 让任何条目消失、条数分毫不差，丢掉的是 revoked_at 与 reason——sign.Keyring 正是
// 用这两个字段生成操作者能读的那句拒绝理由。
func TestWriteKeepsTheFirstRecordedRevocation(t *testing.T) {
	c, _, doc, sigData := writtenCache(t, []sign.KeyID{"k"})

	later := newRevokedSet()
	if err := later.mergeFrom(keyringRevoking(t, []sign.KeyID{"k", "other"}, []revocation{laterAndEmpty})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	merged, err := c.write(doc, sigData, later)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	want := rawRevocationEntry{KeyID: "k", RevokedAt: "2026-08-29T10:00:00Z", Reason: "私钥泄漏"}
	if got := merged.entries["k"]; got != want {
		t.Errorf("write 交回的 entries[k] = %+v, want %+v", got, want)
	}
	_, back, err := c.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := back.entries["k"]; got != want {
		t.Errorf("落盘的 entries[k] = %+v, want %+v——后来那条把先见到的记录换掉了", got, want)
	}
}

// TestWriteLeavesNoTempFileWhenPublishingFails：一次失败的发布不能在缓存目录里
// 留下临时文件。
//
// 原子落盘靠的是「写临时文件 → rename」，失败路径每漏掉一个残片，缓存目录就多一份
// 此后没人会清的垃圾。这里把清单的目标名做成一个目录，让最后那步 rename 必然失败。
func TestWriteLeavesNoTempFileWhenPublishingFails(t *testing.T) {
	c, dir, doc, sigData := writtenCache(t, nil)
	if err := os.Remove(filepath.Join(dir, listFileName)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, listFileName), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := c.write(doc, sigData, newRevokedSet()); err == nil {
		t.Fatal("rename 到一个已存在的目录上却成功了")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("一次失败的发布留下了临时文件：%v", leftovers)
	}
}

// TestWriteReportsAFailedUnlockAlongsideTheFailureThatCausedIt：解锁失败不能因为
// 已经有一个主错误就被丢掉。
//
// 锁没放开的后果不是立刻可见的失败，而是此后每一次写都先等满 lockWait 再报一个
// 不存在的持锁者。这里在发布清单那一步顺手把锁文件删掉，造出「主错误 + 解锁失败」
// 这一对，两条都必须出现在返回的错误里。
func TestWriteReportsAFailedUnlockAlongsideTheFailureThatCausedIt(t *testing.T) {
	c, _, doc, sigData := writtenCache(t, nil)
	failPublishing(t, listFileName, func(c *cache) error {
		// 这段与 write 在同一个 goroutine 里执行，所以用 t 是安全的。
		if err := os.Remove(c.path(lockFileName)); err != nil {
			t.Errorf("用例想删掉锁文件却失败了：%v", err)
		}
		return errors.New("注入：发布清单失败")
	})

	merged, err := c.write(doc, sigData, newRevokedSet())
	if err == nil {
		t.Fatal("注入的发布失败没有让 write 失败")
	}
	if merged != nil {
		t.Errorf("失败的 write 却交回了一个集合：%+v", merged.entries)
	}
	if !strings.Contains(err.Error(), "注入：发布清单失败") {
		t.Errorf("主错误不见了：%v", err)
	}
	if !strings.Contains(err.Error(), "release trustlist cache lock") {
		t.Errorf("解锁失败被主错误盖掉了：%v", err)
	}
}

// TestWriteRefusesANilRevokedSet：nil 累积集必须换来一个说得清的错误，不是 panic。
//
// read 在「清单与累积集都不在」时交回的正好是 nil，一路传回来就会在并集那一步解
// 引用一个 nil map。显式报出来，错误点才落在传错参数的那次调用上。
func TestWriteRefusesANilRevokedSet(t *testing.T) {
	c, _, doc, sigData := writtenCache(t, nil)
	merged, err := c.write(doc, sigData, nil)
	if err == nil {
		t.Fatal("write 收下了一个 nil 累积集")
	}
	if merged != nil {
		t.Errorf("失败的 write 却交回了一个集合：%+v", merged.entries)
	}
	if !strings.Contains(err.Error(), "newRevokedSet()") {
		t.Errorf("错误没告诉调用方该传什么：%v", err)
	}
}
