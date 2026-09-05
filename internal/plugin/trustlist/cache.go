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
// 过期，而 write 的返回值是把这份快照补齐的途径（见 write）。凡是跨 goroutine
// 共享同一个 *revokedSet 的调用方，仍须自己串行化整个 read-modify-write 序列
// ——revokedSet 本身不是并发安全的（理由见 revokedSet 的注释），而 cache 的目录
// 锁只罩住 write 内部那一段。
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
// # 返回值契约（三种结果，调用方必须分开处理）
//
//	err == nil                  清单与累积集都读回来了，*revokedSet 非 nil。
//	errors.Is(err, errNoCache)  清单文件不存在。此时 *revokedSet **仍然可能非
//	                            nil**：只要 revoked-ever.json 还在且读得懂，这台
//	                            机器已经记下的撤销就一并交回去，调用方必须拿它
//	                            起步。只有连 revoked-ever.json 都不存在时才是 nil。
//	其它 err                    缓存不完整或损坏，*revokedSet 为 nil。
//
// errNoCache 时仍然交回累积集，是因为「清单不在而累积集在」不是理论形态：一次
// 首写崩溃、一次磁盘损坏，或者任何能写这个目录的东西删掉一个文件，都会造出它。
// errNoCache 的语义是「从头重建是安全的」，而从头重建若以空集起步，一把这台机器
// 早已记录为撤销的钥匙就会在这一轮里重新可信——落盘的记录救得回下一次启动，救不了
// 这一轮。
// 而 read 之所以坚持验签，用的正是同一个威胁模型：那个攻击者连伪造都不必，删掉
// 一个文件就够了。
//
// 这里**不能**把「清单不在」改报成一个非 errNoCache 的错误：这两类错误的分界就是给
// 调用方的开关——errNoCache 说「从头重建是安全的」，其它错误说「别刷新，保住磁盘上
// 已经记下的撤销」。把一次首写崩溃归到后者，就永远写不出清单，缓存变成一块修不好
// 的砖。
//
// 清单不存在时报的 errNoCache 裹上目录名，因为同一个进程可以有不止一个缓存目录；
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
			return c.readWithoutList()
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

// readWithoutList 是 read 的「清单文件不存在」这一支：报 errNoCache，但把磁盘上
// 已经记下的撤销一并交回去（理由见 read 的返回值契约）。
//
// 累积集在、却读不回来时报的是那份损坏错误而**不是** errNoCache。两个理由：这一
// 条更精确，错误点落在真正坏掉的那个文件上；而且报成 errNoCache 也换不来一次成功
// 的刷新——同一份读不懂的累积集会在 write 里再被 mergeWithRecordedRevocations
// 拒绝一次，只是那时错误已经离现场远了一步。
func (c *cache) readWithoutList() (Document, *revokedSet, error) {
	noCache := fmt.Errorf("%w in %s", errNoCache, c.dir)
	revoked, err := c.readRevoked()
	if err != nil {
		if errors.Is(err, errNoRevocationRecord) {
			// 两个文件都不在：这台机器确实什么都没有，*revokedSet 为 nil。
			return Document{}, nil, noCache
		}
		return Document{}, nil, err
	}
	return Document{}, revoked, noCache
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

// write 落盘一份新接受的清单，并返回它实际写进 revoked-ever.json 的那份并集。
//
// # 返回值契约
//
// err == nil 时返回的集合与刚落盘的 revoked-ever.json 逐条相同，且是这次调用新造
// 的（不与 revoked 共享任何东西），调用方可以直接持有它。err != nil 时返回 nil：
// 这一次刷新整体没成功，不该有任何东西被采信。
//
// 返回并集不是顺手：调用方手里的 revoked 是它某次 read 出来的快照，而那次 read
// 不在锁里，另一个进程随时可能在这中间并入新的撤销。不把并集交回去，调用方接下来
// 拿去装配信任集的就是那份偏少的快照——少掉的正好是撤销。
//
// revoked 为 nil 直接报错。read 在「清单与累积集都不在」时返回的正是 nil，一路
// 传回来就会在 mergeWithRecordedRevocations 里解引用一个 nil map 而 panic；显式
// 报出来，错误点才落在传错参数的那次调用上。没有已记下的撤销时应当传
// newRevokedSet()——那说的是「已知没有撤销」，与「不知道」是两件事。
//
// # 落盘
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
// 不是直接拿 revoked 覆盖。两件事缺一不可：只加锁只能让两个写者排队，排在后面
// 的那个仍会用自己的过期快照把前一个刚记下的撤销覆盖掉。取并集才是这个类型的
// 语义——累积集只增不减。冲突时保留磁盘上那条（先见到的记录胜出，与 mergeFrom
// 同规则）。
//
// 并集不改动调用方手里那份快照（那是它自己的）；补齐它走的是返回值。
func (c *cache) write(doc Document, sigData []byte, revoked *revokedSet) (merged *revokedSet, err error) {
	if revoked == nil {
		return nil, errors.New("write cached trustlist: the revoked set is nil; pass newRevokedSet() " +
			`when nothing has been recorded yet, so that "known to be empty" is never read as "unknown"`)
	}
	unlock, err := c.lock()
	if err != nil {
		return nil, err
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			// 锁没放开的后果不是立刻可见的失败，而是此后每一次写都先等满
			// lockWait 再报一个不存在的持锁者。所以主错误已经存在时也不能把
			// 它丢掉——包里没有 logger，errors.Join 是让两件事都被看见的办法。
			// 连同把 merged 清成 nil：契约是「err != nil 就什么都别采信」。
			err = errors.Join(err, unlockErr)
			merged = nil
		}
	}()

	written, err := c.mergeWithRecordedRevocations(revoked)
	if err != nil {
		return nil, err
	}
	revokedData, err := written.marshal()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomically(c, revokedFileName, revokedData); err != nil {
		return nil, err
	}
	if err := writeFileAtomically(c, sigFileName, sigData); err != nil {
		return nil, err
	}
	if err := writeFileAtomically(c, listFileName, doc.Raw); err != nil {
		return nil, err
	}
	return written, nil
}

// mergeWithRecordedRevocations 返回「磁盘上已记下的撤销」与 revoked 的并集，
// 冲突时保留磁盘上那条。只在持锁期间调用。
//
// 冲突保留磁盘那条不是「反正条数一样」：两条记录的差别在 revoked_at 与 reason，
// 而 sign.Keyring 正是用这两个字段生成操作者能读的那句拒绝理由。后来者覆盖不会
// 让任何条目消失，坏掉的是那句话。
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
//
// 它是包级变量而不是方法，唯一的理由是让测试能把失败点精确钉在「发布哪一个文件」
// 这一步上：write 的落盘顺序（先累积集、后清单）是一条硬性要求，而用目录权限之类
// 的外部手段造出来的失败分不开「读磁盘上的累积集失败」与「发布清单失败」，顺序
// 反转就没人抓得住。生产路径上它始终是下面这个实现，不带任何开关；改写它的入口
// 只在 export_test.go 里。
var writeFileAtomically = func(c *cache, name string, data []byte) error {
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
// 用 O_CREATE|O_EXCL 建一个哨兵文件作锁：它是这里选用的做法——「检查」与「创建」
// 在一次系统调用里完成，两者之间没有能输掉的竞争窗口。（mkdir、link、平台咨询锁
// 是同一类做法，各有各的取舍；选哨兵文件是因为它在任何一台机器上都看得见、删得
// 掉。）争用（见 lockCreateIsContended）会等待重试；等满 lockWait 之后报错，且
// 错误里带上最后看到的那个 create 错误——一把因为权限而真正打不开的锁，必须以
// 权限问题的面目出现，而不是一个幽灵持锁者。
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
