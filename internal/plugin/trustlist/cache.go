package trustlist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	listFileName    = "trustlist.json"
	sigFileName     = "trustlist.sig"
	revokedFileName = "revoked-ever.json"
	lockFileName    = ".lock"

	// lockWait 是等一个并发写者放手的上限。刷新不是热路径，等几秒远好过
	// 随机失败；超时的错误里会带上最后看到的那个错误（见 cache_lock_windows.go）。
	lockWait = 5 * time.Second
	// lockPoll 是两次重试之间的间隔。
	lockPoll = 20 * time.Millisecond
)

// errNoCache 表示这台机器还没有任何缓存的清单——目录是空的。
//
// 它与「缓存损坏」是不同的两件事，且必须分得开：前者是全新安装的正常状态，
// 后者需要人来看一眼。调用方对二者的状态归类相同（都是 unavailable），但
// 日志与告警不同。
var errNoCache = errors.New("no cached trustlist")

// errNoRevocationRecord 表示 revoked-ever.json 这个文件不存在。
//
// 它是本包内部的哨兵，两个调用方对同一件事的归类不同，必须由它们各自决定，
// 所以 readRevoked 只能如实报出「文件不存在」而不能替谁做主：write 把它当成
// 这台机器的第一次写（没有已记下的撤销可并），read 把它当成缓存不完整。
//
// 它刻意不是「返回一个空集」：空集说的是「见过清单，但没有任何撤销」，与
// 「这份记录根本不在」是两件事，而把后者读成前者正是这个文件能造成的最坏的谎。
var errNoRevocationRecord = errors.New("no revocation record")

// cache 是缓存目录。三个文件：
//
//	trustlist.json     最近一次被接受的清单**原始字节**
//	trustlist.sig      它的签名
//	revoked-ever.json  撤销累积集
//
// 存原始字节而不是解析后的结构，是为了让读回来时走的解析与验签代码和网络
// 路径**完全相同**——缓存无法成为一条绕过校验的旁路。
//
// 「见过的最大 serial」不单独存文件：它就是 trustlist.json 里的 serial。代价是
// 缓存损坏时防回滚保护随之失效；可以接受，因为损坏不会波及 revoked-ever.json
// （独立文件、只增不减），而回滚攻击的目标——让撤销失效——正是被累积集挡住的。
//
// # 并发
//
// write 全程在目录锁下进行，落盘的累积集因此只增不减，跨进程也成立（见 write）。
// read 不取锁，所以它返回的 *revokedSet 只是一份快照：另一个写者随时可能让它
// 过期。凡是跨 goroutine 共享同一个 *revokedSet 的调用方，仍须自己串行化整个
// read-modify-write 序列——revokedSet 本身不是并发安全的（理由见 revokedSet 的
// 注释），而 cache 的目录锁只罩住 write 内部那一段。
type cache struct {
	dir string
}

// newCache 返回落在 dir 的缓存，必要时把目录建出来（0700，含父目录）。
//
// 在这里建而不是等到第一次 write，是为了让一个不可用的缓存位置——路径是个普通
// 文件、进程对它没有写权限——在配置的时候就报出来，而不是在一次刷新做到一半时。
// dir 会解析成绝对路径，进程之后改工作目录不会把缓存挪走。
func newCache(dir string) (*cache, error) {
	if dir == "" {
		return nil, errors.New("trustlist cache dir is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve trustlist cache dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create trustlist cache dir %s: %w", abs, err)
	}
	return &cache{dir: abs}, nil
}

func (c *cache) path(name string) string { return filepath.Join(c.dir, name) }

// read 读回缓存的清单与撤销累积集。
//
// 清单走的是 VerifyDocument——与网络路径同一个函数。这不是多余的谨慎：如果
// 缓存读取绕过验签，那么任何能写这个目录的东西就能给这台机器换一份信任集。
//
// 清单不存在报 errNoCache（裹上目录名，因为同一个进程可以有不止一个缓存目录）；
// 其余任何一个文件缺失或损坏都报错，且**不删除、不重建**：静默重建会抹掉判断
// 「是磁盘坏了还是有人动过」的唯一现场，静默当成空缓存则等于宣告这台机器从没
// 见过任何撤销。
//
// read 不取目录锁。代价是它可能撞上一次正在进行的 write 的中间态：清单与签名是
// 两次各自原子的 rename，两次之间读到的是一份配不上对方的清单，会被报成「缓存
// 不可信」。这个窗口只有一次 write 那么长，且下一次 read 就好了；而让 read 去争
// 同一把排他锁的代价要大得多——一个被杀死的写者留下的锁文件会把启动时的缓存读取
// 一起挡住，那是比一次可重试的失败更坏的故障。
func (c *cache) read() (Document, *revokedSet, error) {
	listData, err := os.ReadFile(c.path(listFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Document{}, nil, fmt.Errorf("%w in %s", errNoCache, c.dir)
		}
		return Document{}, nil, fmt.Errorf("read cached trustlist %s: %w", c.path(listFileName), err)
	}
	sigData, err := os.ReadFile(c.path(sigFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 清单在而签名不在：这不是「还没缓存」，是缓存不完整。
			return Document{}, nil, fmt.Errorf("cached trustlist has no signature at %s; "+
				"the cache is incomplete and will not be used", c.path(sigFileName))
		}
		return Document{}, nil, fmt.Errorf("read cached trustlist signature %s: %w", c.path(sigFileName), err)
	}
	doc, err := VerifyDocument(listData, sigData)
	if err != nil {
		return Document{}, nil, fmt.Errorf("cached trustlist at %s: %w", c.dir, err)
	}
	revoked, err := c.readRevoked()
	if err != nil {
		if errors.Is(err, errNoRevocationRecord) {
			return Document{}, nil, fmt.Errorf("%w; the cached trustlist is unusable rather than "+
				"evidence that nothing was ever revoked", err)
		}
		return Document{}, nil, err
	}
	return doc, revoked, nil
}

// readRevoked 读回 revoked-ever.json。文件不存在报 errNoRevocationRecord——那是
// 唯一一种「没有这份记录」与「记录说没有撤销」有区别的情况，两个调用方对它的
// 归类不同（见 errNoRevocationRecord）。
//
// 文件在但读不回来一律报错。**不能**退化成空集：空集说的是「这台机器从没见过
// 任何撤销」，那是这个文件能造成的最坏的谎。空文件、`{}`、`{"revoked":null}` 都
// 会被 parseRevokedSet 拒绝，所以这个文件永远不会被本包用一份占位内容初始化——
// 首启走的是「文件不存在」这条路。
func (c *cache) readRevoked() (*revokedSet, error) {
	data, err := os.ReadFile(c.path(revokedFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", errNoRevocationRecord, c.path(revokedFileName))
		}
		return nil, fmt.Errorf("read cached revocations %s: %w", c.path(revokedFileName), err)
	}
	revoked, err := parseRevokedSet(data)
	if err != nil {
		return nil, fmt.Errorf("cached revocations at %s: %w", c.path(revokedFileName), err)
	}
	return revoked, nil
}

// write 落盘一份新接受的清单。
//
// 顺序是**先写撤销累积集，成功后再写清单**。反过来会留下一个「清单已更新但
// 撤销没记下」的窗口，而这个方向的丢失正是累积集存在要防的事。签名排在清单
// 之前是同一个道理的小一号版本：首次写到一半崩掉时，缺清单会被 read 读成
// errNoCache（正常的全新状态），缺签名则会被读成一份需要人来看的不完整缓存。
//
// 每个文件都走「写临时文件 → Sync → rename」，rename 在同一目录内是原子的，
// 所以读者永远看不到半份文件。跨文件没有这种原子性：清单与签名之间崩掉会留下
// 一对配不上的文件，下次 read 会把它报成不可信的缓存——响亮，且一次成功的 write
// 就能修好，好过静默使用一份签名对不上的清单。
//
// 整个操作在目录锁下进行，而且落盘的累积集是**磁盘上那份与 revoked 的并集**，
// 不是直接拿 revoked 覆盖。两件事缺一不可：调用方手里的 revoked 是它某个时刻
// read 出来的快照，而那次 read 不在锁里，所以快照可能已经过期；只加锁只能让两个
// 写者排队，排在后面的那个仍会用自己的过期快照把前一个刚记下的撤销覆盖掉。取并集
// 才是这个类型的语义——累积集只增不减。冲突时保留磁盘上那条（先见到的记录胜出，
// 与 mergeFrom 同规则）。
//
// 并集只保证**落盘的**记录不丢；它不会把调用方手里那份快照补新。跨 goroutine
// 共享同一个 *revokedSet 的调用方仍须自己串行化整个 read-modify-write 序列。
func (c *cache) write(doc Document, sigData []byte, revoked *revokedSet) (err error) {
	unlock, err := c.lock()
	if err != nil {
		return err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()

	merged, err := c.mergeWithRecordedRevocations(revoked)
	if err != nil {
		return err
	}
	revokedData, err := merged.marshal()
	if err != nil {
		return err
	}
	if err := c.writeFileAtomically(revokedFileName, revokedData); err != nil {
		return err
	}
	if err := c.writeFileAtomically(sigFileName, sigData); err != nil {
		return err
	}
	return c.writeFileAtomically(listFileName, doc.Raw)
}

// mergeWithRecordedRevocations 返回「磁盘上已记下的撤销」与 revoked 的并集，
// 冲突时保留磁盘上那条。只在持锁期间调用。
//
// 磁盘上那份读不回来时报错而不是绕过去：覆盖一份读不懂的累积集，等于拿一份读得懂
// 但更短的记录换掉它，而被换掉的恰好是无从恢复的撤销。这确实意味着一个损坏的
// revoked-ever.json 会让此后每一次写都失败，直到有人来看一眼——那正是想要的结果。
//
// 它不改动 revoked 本身：调用方的那份快照是它自己的，这里多写一个字节都会让
// 「谁在改这个集合」变成一个没有答案的问题。
func (c *cache) mergeWithRecordedRevocations(revoked *revokedSet) (*revokedSet, error) {
	merged := newRevokedSet()
	recorded, err := c.readRevoked()
	switch {
	case err == nil:
		for id, entry := range recorded.entries {
			merged.entries[id] = entry
		}
	case errors.Is(err, errNoRevocationRecord):
		// 这台机器的第一次写：还没有已记下的撤销可并。这是唯一一种「读不到就
		// 接着走」是对的情况，因为文件不存在本身就说明没有任何记录会被覆盖掉。
	default:
		return nil, err
	}
	for id, entry := range revoked.entries {
		if _, seen := merged.entries[id]; seen {
			continue
		}
		merged.entries[id] = entry
	}
	return merged, nil
}

// writeFileAtomically 把 data 发布成缓存目录里的 name：先写同目录下的临时文件、
// Sync、再 rename 过去。rename 在同一目录内是原子的，所以读者要么看到旧的那份，
// 要么看到完整的新的那份，不会看到半份。
func (c *cache) writeFileAtomically(name string, data []byte) error {
	tmp, err := os.CreateTemp(c.dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	// rename 成功之后这次 Remove 找不到文件，失败也无害：临时文件名带随机后缀，
	// 不会与任何已发布的文件重名，留下一个也只是一个残片；反过来把它的失败报成
	// write 的失败，会让一次已经落盘成功的发布被当成没成功。
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, c.path(name)); err != nil {
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// lock 取得缓存目录的排他锁，返回释放它的函数。
//
// 用 O_CREATE|O_EXCL 建一个哨兵文件作锁：它是唯一一种在「检查」与「创建」之间
// 不会输掉竞争的做法。争用（见 lockCreateIsContended）会等待重试；等满 lockWait
// 之后报错，且错误里带上最后看到的那个 create 错误——一把因为权限而真正打不开
// 的锁，必须以权限问题的面目出现，而不是一个幽灵持锁者。
func (c *cache) lock() (func() error, error) {
	path := c.path(lockFileName)
	release := func() error {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("release trustlist cache lock %s: %w", path, err)
		}
		return nil
	}
	deadline := time.Now().Add(lockWait)
	// last 是这次等待卡在的那个 create 失败，留着让超时的错误能点名它。没有它，
	// 一把因为自身原因建不出来的锁——ACL 拒绝、只读位置——会被报成「有人持有它」，
	// 把操作者支去找一个不存在的进程。
	var last error
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// 锁是这个文件的存在本身，不是这个打开的句柄，所以立刻关掉。关失败
			// 时锁仍然是持有状态，所以要连同释放一起报，不能吞掉。
			if closeErr := f.Close(); closeErr != nil {
				return nil, errors.Join(
					fmt.Errorf("acquire trustlist cache lock %s: close: %w", path, closeErr),
					release(),
				)
			}
			return release, nil
		}
		if !lockCreateIsContended(err) {
			return nil, fmt.Errorf("acquire trustlist cache lock %s: %w", path, err)
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire trustlist cache lock %s: another writer has held it for more "+
				"than %s (last attempt: %v); if no other process is refreshing the trustlist, that file is "+
				"a leftover from one that was killed and clearing it means deleting exactly that file — and "+
				"if the last attempt above reports a permission failure rather than an existing file, the "+
				"problem is the location's permissions, not a lock holder", path, lockWait, last)
		}
		time.Sleep(lockPoll)
	}
}
