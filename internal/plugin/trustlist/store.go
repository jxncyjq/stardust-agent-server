package trustlist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// ErrSerialRegressed 标记「收到的清单 serial 小于本地已见的那个」。
//
// 它是哨兵，因为它与网络故障是完全不同的事：对这套机制最便宜的攻击不是伪造
// 清单（那要 root 私钥），而是重放一份**签名完全合法**的旧清单，把用户挡在
// 某次撤销之前。它值得比一次超时高得多的告警级别，而且重试绝不会让它变好。
var ErrSerialRegressed = errors.New("trustlist serial regressed")

// Status 是手上这份清单的时效状态。
type Status int

const (
	// StatusUnavailable：没有可用的清单——从没成功取得过，或缓存损坏，或装配
	// 信任集失败。此时 Trust.Keyring 是 nil、Trust.Publishers 是 nil，调用方
	// 必须把所有插件按「未登记」处理。
	StatusUnavailable Status = iota
	// StatusStale：验签通过但已过 expires_at（多半是长期断网）。信任集照常
	// 可用，撤销照常生效。
	StatusStale
	// StatusFresh：验签通过且未过期。
	StatusFresh
)

// String 返回状态的小写名字，未定义的值渲染成 Status(<数字>)。
func (s Status) String() string {
	switch s {
	case StatusUnavailable:
		return "unavailable"
	case StatusStale:
		return "stale"
	case StatusFresh:
		return "fresh"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// Trust 是一次装配的结果：信任集、发布者名录，以及它有多新。
//
// Status 为 StatusUnavailable 时 Keyring 与 Publishers 都是 nil。这不是「留空
// 待填」——它是让「拿不到清单就先放行」在类型层面做不到：没有信任集，就没有
// 任何东西可以用来判一个插件可信。
type Trust struct {
	Keyring    *sign.Keyring
	Publishers map[sign.KeyID]Publisher
	Status     Status
	Serial     int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// Config 是造一个 Store 需要的东西。
type Config struct {
	// URL 是清单文档的地址，必须以 .json 结尾且不带 query 或 fragment
	// （签名地址由它推导，见 sigURL）。取回时还要求它是 https（见 fetchBytes）。
	URL string
	// CacheDir 是缓存目录，不能为空。
	CacheDir string
	// Client 是发请求用的客户端。为 nil 时用 http.DefaultClient。
	Client *http.Client
	// Now 供测试注入时钟；为 nil 时用 time.Now。它只影响 fresh/stale 的判定。
	Now func() time.Time
}

// Store 持有缓存目录与取回配置，是这个包唯一有状态的类型。
//
// 造出来之后每个字段都不再改动，所以并发调用是安全的。mu 只串行化 Refresh：
// 一次刷新是「读缓存 → 取回 → 比较 serial → 并入撤销 → 落盘」的读改写序列，
// 两个并发的它会各自拿着一份取回前的快照去写，后写的那个把先写的成果盖掉。
// 跨进程的同一问题由缓存目录锁挡住（见 cache.write）。
//
// Current 刻意不取 mu：它只读缓存文件，而 Refresh 会在持锁期间等一次网络往返，
// 共享一把锁会把那次往返的时延加到每一次只读调用上。缓存读取本来就要能撞上一次
// 正在进行的写并把撞见的中间态报成错误（见 cache.read）。
type Store struct {
	url    string
	sigURL string
	client *http.Client
	cache  *cache
	now    func() time.Time
	mu     sync.Mutex
}

// NewStore 校验 cfg 并造出 Store，必要时把缓存目录建出来。
//
// 空 URL 直接报错而不是造一个「没配远端」的 Store：Refresh 要那个地址，而配置
// 错误应当在装配期暴露，不是留到第一次刷新时才现形——那时错误点已经离配置错误
// 很远。Current 不用那个地址（它只读缓存），但一个刷不出新清单的 Store 不是一个
// 可用的 Store。同理，签名地址在这里就推导好：一个推导不出签名地址的清单地址
// 同样是配置错误。
func NewStore(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("trustlist: url is empty; an empty url means the remote list is not " +
			"configured, and building a Store for it only moves the failure to every later call")
	}
	sig, err := sigURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	c, err := newCache(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Store{url: cfg.URL, sigURL: sig, client: client, cache: c, now: now}, nil
}

// Current 返回当前信任状态，只读缓存，**绝不发起网络请求**。
//
// 它只读本地文件、不取 Refresh 那把锁，因此它的时延不含任何网络往返。缓存缺失、
// 损坏或装不出信任集时返回 StatusUnavailable 的 Trust 与一个 error——两者都返回，
// 因为调用方既要知道出了什么事，也要一个能安全使用的零状态（Keyring 为 nil，
// 见 Trust）。
func (s *Store) Current() (Trust, error) {
	doc, revoked, err := s.cache.read()
	if err != nil {
		return Trust{Status: StatusUnavailable}, err
	}
	return s.assemble(doc, revoked)
}

// Refresh 取回一次，通过全部校验后落盘，返回新的状态。
//
// 它同时返回 Trust 和 error，而不是常规的「返回零值 + error」：零值状态
// （StatusUnavailable）有明确的安全含义——调用方必须把所有插件按未登记处理——
// 把一次网络超时渲染成它，会造成一次不必要的全线降级。所以失败时返回的是缓存
// 里那份仍然可用的状态，加上说明这次没拉到的错误。没有缓存可退时那个状态才是
// unavailable。
//
// serial 有三种情况：小于缓存里那个直接拒绝（裹 ErrSerialRegressed）；相等但
// 内容不同也拒绝，因为发布侧改了内容却没进 serial，无法判断哪一份才是当前的；
// 相等且逐字相同则是无变化，不重写缓存。
//
// # 一种它分辨不出来的失败
//
// cache.write 可能三个文件全都发布成功、只有释放目录锁那一步失败；那时它返回
// nil 并集加一个只讲锁的错误，这里无从知道磁盘其实已经推进了一轮。只能按契约
// 办：这次刷新算失败，返回缓存那份旧的。这个方向是安全的——旧的那份不会比刷新
// 之前更宽松，而磁盘上那份新的（连同它并进去的撤销）会在下一次读缓存时被采用。
// 反过来采信一份没写成的并集才是危险的：那会让内存里的信任集比磁盘上宽。
func (s *Store) Refresh(ctx context.Context) (Trust, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先把手上已有的读出来，它既是 serial 比较的基准，也是失败时要返回的东西。
	cachedDoc, cachedRevoked, cacheErr := s.cache.read()
	fallback := Trust{Status: StatusUnavailable}
	// fallbackErr 是「缓存那份也装不出信任集」。它必须跟着每一次失败一起交回去，
	// 否则调用方看到一个 unavailable 却只拿到网络错误，找不到状态为什么是空的。
	var fallbackErr error
	haveCache := false
	// fail 把这次刷新的失败原因与 fallbackErr 一起交回去。errors.Join 保留两条
	// 错误链，errors.Is 仍然认得其中任何一个哨兵。
	//
	// 它读的是 fallback 与 fallbackErr 在**调用那一刻**的值，所以下面那个 switch
	// 先把它们填好再往下走：定义在前、取值在后是有意的，不是笔误。
	fail := func(err error) (Trust, error) { return fallback, errors.Join(err, fallbackErr) }

	switch {
	case cacheErr == nil:
		haveCache = true
		fallback, fallbackErr = s.assemble(cachedDoc, cachedRevoked)
		// 装不出信任集（例如撤销已累积到覆盖当前清单的全部 keys）时 fallback 是
		// unavailable，但 haveCache 仍为 true：serial 基准与撤销累积集都还有效，
		// 而一份新清单带进一把新钥匙就能重新装得出来。

	case errors.Is(cacheErr, errNoCache):
		// 清单文件不在。这既可能是全新安装，也可能是一次首写崩溃或有人删了一个
		// 文件，两者都由 cache.read 归到 errNoCache——它的语义是「从头重建是安全
		// 的」。但重建不等于从空的撤销累积集起步：cache.read 在这一支仍然会把磁盘
		// 上那份撤销记录交回来（只有连 revoked-ever.json 都不存在时才是 nil），
		// 下面用的就是它。

	default:
		// 缓存损坏。**到此为止，不继续取回**。
		//
		// 继续的后果有两个。一是这一轮没有可比的 serial：防回滚保护靠的就是缓存
		// 里那个 serial，读不出来就等于关掉它，一份重放的旧清单会被当成新的收下。
		// 二是一次成功的发布会覆盖掉损坏的那几个文件，把「是磁盘坏了还是有人动过」
		// 的唯一现场抹平。
		//
		// 损坏要人来看一眼，不能靠一次成功的取回悄悄「修好」。这条与 errNoCache
		// 的分界也是为此：全新安装（以及首写崩溃留下的残局）必须能正常起步，否则
		// 缓存会变成一块修不好的砖。
		return fail(fmt.Errorf("trustlist cache at %s is unusable; refusing to refresh over it, because "+
			"this round would have no serial to compare against and publishing over the damaged files "+
			"would erase the only evidence of what happened: %w", s.cache.dir, cacheErr))
	}

	listData, err := fetchBytes(ctx, s.client, s.url, maxListBytes)
	if err != nil {
		return fail(err)
	}
	sigData, err := fetchBytes(ctx, s.client, s.sigURL, maxSigBytes)
	if err != nil {
		return fail(err)
	}
	doc, err := VerifyDocument(listData, sigData)
	if err != nil {
		return fail(err)
	}

	if haveCache {
		switch {
		case doc.Serial < cachedDoc.Serial:
			return fail(fmt.Errorf("trustlist at %s has serial %d but this machine has already seen %d; "+
				"refusing a list that would undo revocations recorded since then: %w",
				s.url, doc.Serial, cachedDoc.Serial, ErrSerialRegressed))
		case doc.Serial == cachedDoc.Serial && !bytes.Equal(doc.Raw, cachedDoc.Raw):
			return fail(fmt.Errorf("trustlist at %s has serial %d, the same as the cached one, but "+
				"different content; the publisher changed the list without advancing the serial, and "+
				"there is no way to tell which one is current", s.url, doc.Serial))
		case doc.Serial == cachedDoc.Serial:
			// 逐字相同：无变化，不重写缓存。重写不只是多余的 I/O——它会把「上一次
			// 真正变过的时刻」从这几个文件上抹掉。
			return fallback, fallbackErr
		}
	}

	// cachedRevoked 只在「清单与撤销记录都不在」时为 nil（缓存损坏那一支上面
	// 已经返回了）。绝不能从空集起步：一把这台机器早已记录为撤销的钥匙会在这一
	// 轮里重新可信，而落盘的记录救得回下一次启动，救不了这一轮。
	revoked := cachedRevoked
	if revoked == nil {
		revoked = newRevokedSet()
	}
	if err := revoked.mergeFrom(doc.KeyringRaw); err != nil {
		return fail(err)
	}
	// write 返回的是它实际写进 revoked-ever.json 的那份并集，必须拿它取代手上
	// 这份快照：快照是取回之前读出来的，恒比磁盘旧——另一个进程可能刚并入了一条
	// 新撤销。用旧的那份去装配，少掉的正好是撤销。
	written, err := s.cache.write(doc, sigData, revoked)
	if err != nil {
		return fail(fmt.Errorf("cache trustlist serial %d from %s: %w", doc.Serial, s.url, err))
	}
	return s.assemble(doc, written)
}

// assemble 把一份文档与撤销累积集装配成 Trust，并按当前时间判定 fresh/stale。
//
// 装配失败时返回 StatusUnavailable 的空 Trust 与错误，不吞掉换一个空信任集：
// 「合并后每把钥匙都被撤销」是真实可能的状态（见 assembleKeyring），而它的正确
// 表达是「没有可用的信任集」，不是「一个谁都不认的信任集」。
func (s *Store) assemble(doc Document, revoked *revokedSet) (Trust, error) {
	keyring, err := assembleKeyring(doc.KeyringRaw, revoked)
	if err != nil {
		return Trust{Status: StatusUnavailable}, err
	}
	status := StatusFresh
	if !s.now().Before(doc.ExpiresAt) {
		status = StatusStale
	}
	return Trust{
		Keyring:    keyring,
		Publishers: doc.Publishers,
		Status:     status,
		Serial:     doc.Serial,
		IssuedAt:   doc.IssuedAt,
		ExpiresAt:  doc.ExpiresAt,
	}, nil
}
