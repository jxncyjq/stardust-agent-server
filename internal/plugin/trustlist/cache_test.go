package trustlist

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
	if err := c.write(doc, sigData, set); err != nil {
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
			if err := c.write(doc, sigData, newRevokedSet()); err != nil {
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
	if err := c.write(doc7, sig7, newRevokedSet()); err != nil {
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
	if err := c.write(doc9, sig9, newRevokedSet()); err == nil {
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
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []sign.KeyID{"gone-a", "gone-b"} {
		wg.Add(1)
		go func(id sign.KeyID) {
			defer wg.Done()
			_, set, err := c.read()
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if err := set.mergeFrom(keyringWith(t, []sign.KeyID{id}, []sign.KeyID{id})); err != nil {
				t.Errorf("mergeFrom: %v", err)
				return
			}
			if err := c.write(doc, sigData, set); err != nil {
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
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
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
	if err := c.write(doc, sigData, other); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 拿过期快照写：gone-b 要进去，gone-a 不能出来。
	if err := stale.mergeFrom(keyringWith(t, []sign.KeyID{"gone-b"}, []sign.KeyID{"gone-b"})); err != nil {
		t.Fatalf("mergeFrom: %v", err)
	}
	if err := c.write(doc, sigData, stale); err != nil {
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
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}

	corrupt := []byte(`{"revoked":[{"key_id":`)
	if err := os.WriteFile(filepath.Join(dir, "revoked-ever.json"), corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if err := c.write(doc, sigData, newRevokedSet()); err == nil {
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
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write 返回之后锁文件还在：stat err = %v", err)
	}
	if err := c.write(doc, sigData, newRevokedSet()); err != nil {
		t.Fatalf("第二次 write: %v", err)
	}
}
