package trustlist

import (
	"bytes"
	"context"
	"encoding/json"
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

// ErrRevocationsUnknown 标记「这台机器已经记下的撤销读不出来」。
//
// 它与「没有可用的清单」是必须分开的两件事，因为对它们的正确反应相反。清单读
// 不出来时降级是设计好的行为：登记那一半只剩本地 keyring，按安装期已经记下的
// 结论继续挂载。而撤销累积集读不出来时，这台机器无从回答「这把钥匙是不是已经
// 被撤销了」——把它读成「没有撤销」正是「删掉一个文件就绕过撤销」那条路，也正是
// 这份记录存在要挡的事。所以它是哨兵：调用方必须认得出这一种失败并**拒绝**，
// 而不是像清单不可用那样降级。
var ErrRevocationsUnknown = errors.New("trustlist revocation record is unknown")

// Status 是手上这份清单的时效状态。
type Status int

const (
	// StatusUnavailable：没有可用的清单——从没成功取得过，或缓存损坏，或装配
	// 信任集失败。此时 Trust.Keyring、Trust.KeyringRaw 与 Trust.Publishers 都是
	// nil，调用方必须把所有插件按「未登记」处理。
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
// Status 为 StatusUnavailable 时 Keyring、KeyringRaw 与 Publishers 都是 nil。
// 这不是「留空待填」——它是让「拿不到清单就先放行」在类型层面做不到：没有信任集，
// 就没有任何东西可以用来判一个插件可信。
//
// Keyring 与 KeyringRaw 各带一半、缺一不可，因为两者装的不是同一份事实：
//
//   - KeyringRaw 是清单信封里那段 keyring 文档的原始字节，是这里**唯一**还留着
//     公钥的地方——sign.Keyring 的导出面（IDs / RevokedIDs / Revoked / Verify）
//     没有按 id 取公钥的方法，所以一个 *sign.Keyring 没法把自己的登记贡献给另一
//     份 keyring 文档；
//   - Keyring 装的撤销集比 KeyringRaw 里那段宽：assembleKeyring 把本机的撤销
//     累积集（revoked-ever.json）并了进去，而原始字节里只有这一份清单自己写下的
//     那些。
type Trust struct {
	Keyring    *sign.Keyring
	KeyringRaw json.RawMessage
	Publishers map[sign.KeyID]Publisher
	Status     Status
	Serial     int64
	IssuedAt   time.Time
	ExpiresAt  time.Time

	// revocations 是随这份 Trust 一起交回的撤销累积集：这台机器见过的全部撤销。
	//
	// 它与上面三个 nil 掉的字段并列存在，是因为**撤销的可用性必须与清单文档的
	// 可用性解耦**。清单过期不作废（否则一断网所有插件立刻失信），所以断网不能
	// 成为绕过撤销的办法；如果一个缺失或损坏的 trustlist.json 反而能把这台机器
	// 攒了几个月的撤销一起带走，那么「删掉一个文件」就重新变成了那个办法——攻击
	// 者连伪造都不必。这正是 S1 的「撤销单调，登记不单调」：登记可以随清单不可用
	// 而降级，撤销不行。
	//
	// Keyring 非 nil 时这份集合已经被 assembleKeyring 并进 Keyring 里，两处说的
	// 是同一批撤销；Keyring 为 nil 时它是撤销仅剩的载体，也是 Merge 唯一还能从
	// 这一侧读到撤销的地方。
	//
	// nil 说的是「这份 Trust 不携带任何撤销记录」，**不是**「这台机器没有撤销」。
	// 后者的表达是一个非 nil 的空集合。Current 只在裹 ErrRevocationsUnknown 硬错
	// 的时候交回一份 revocations 为 nil 的 Trust（见 Current）。
	//
	// 它不导出：这个集合的语义（只增不减、并集时先见到的胜出、空与 nil 有别）
	// 由本包持有，而包外唯一需要它的地方是 Merge，就在本包内。
	revocations *revokedSet
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
// 因为调用方既要知道出了什么事，也要一个仍然能安全使用的状态。
//
// 「能安全使用」在这里有两半，缺一半就不安全：Keyring 为 nil（没有任何东西可以
// 用来把一个插件判成可信，见 Trust），而这份 Trust **仍然携带这台机器已知的撤销
// 累积集**。少了后一半，一个缺失或损坏的 trustlist.json 就会让这台机器忘掉它已经
// 记下的每一条撤销，被撤销的钥匙签的包重新挂得上去——理由见 Trust.revocations。
//
// 累积集本身读不出来是另一回事：那不是「没有撤销」，是判不了。这时返回的错误裹
// ErrRevocationsUnknown（与清单那半的错误 join 在一起，两条链都保住），Trust 的
// revocations 为 nil。调用方必须认出这个哨兵并拒绝，不得当成「清单不可用」降级。
func (s *Store) Current() (Trust, error) {
	doc, revoked, err := s.cache.read()
	if err == nil {
		return s.assemble(doc, revoked)
	}
	known, knownErr := s.knownRevocations(revoked, err)
	if knownErr != nil {
		return Trust{Status: StatusUnavailable}, errors.Join(err, knownErr)
	}
	return Trust{Status: StatusUnavailable, revocations: known}, err
}

// knownRevocations 在清单那一半已经不可用之后回答「这台机器已经记下了哪些撤销」。
// fromRead 与 cause 是 cache.read 这一次的第二、第三个返回值。
//
// 三条路各有各的理由：
//
//   - fromRead 非 nil：cache.read 已经读过盘并按它的返回值契约把记录交了回来
//     （errNoCache 那一支就是这样）。直接用它，不再读第二次——两次读之间磁盘可能
//     被另一个进程改写，而这一轮该用的是这一次读到的那份。
//   - fromRead 为 nil 且 cause 是 errNoCache：按 cache.read 的契约，那意味着清单
//     与撤销记录两个文件都不在，也就是这台机器确实一条撤销都没记过。返回一个非
//     nil 的**空集合**而不是 nil：空集说的是「已知没有撤销」，nil 说的是「不知道」，
//     而这里是知道的。
//   - 其余（缓存损坏）：cache.read 在这一支不交回记录，所以单独读一次
//     revoked-ever.json。读不到就报 ErrRevocationsUnknown，**文件不存在也算**——
//     清单文件在（只是不可信）而撤销记录不在，正是 cache.read 判为「缓存不可用，
//     而不是这台机器从没撤销过任何东西」的那个形态，这里不另立一套更松的说法。
func (s *Store) knownRevocations(fromRead *revokedSet, cause error) (*revokedSet, error) {
	if fromRead != nil {
		return fromRead, nil
	}
	if errors.Is(cause, errNoCache) {
		return newRevokedSet(), nil
	}
	revoked, err := s.cache.readRevoked()
	if err != nil {
		return nil, fmt.Errorf("read the revocations recorded in %s while the cached trustlist is "+
			"unusable; without them there is no way to tell whether a key has been revoked, and reading "+
			"that as \"nothing was revoked\" is the bypass this record exists to stop: %w: %w",
			s.cache.dir, ErrRevocationsUnknown, err)
	}
	return revoked, nil
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
// 装配失败时返回 StatusUnavailable 的 Trust 与错误，不吞掉换一个空信任集：
// 「合并后每把钥匙都被撤销」是真实可能的状态（见 assembleKeyring），而它的正确
// 表达是「没有可用的信任集」，不是「一个谁都不认的信任集」。那份 Trust 的
// Keyring 为 nil，但撤销累积集照样带着走——装不出信任集与「这台机器忘了它记下的
// 撤销」是两件事，而后一件在「每把钥匙都被撤销」这个状态下恰恰最不能发生。
func (s *Store) assemble(doc Document, revoked *revokedSet) (Trust, error) {
	keyring, err := assembleKeyring(doc.KeyringRaw, revoked)
	if err != nil {
		return Trust{Status: StatusUnavailable, revocations: revoked}, err
	}
	status := StatusFresh
	if !s.now().Before(doc.ExpiresAt) {
		status = StatusStale
	}
	return Trust{
		Keyring:     keyring,
		KeyringRaw:  doc.KeyringRaw,
		Publishers:  doc.Publishers,
		Status:      status,
		Serial:      doc.Serial,
		IssuedAt:    doc.IssuedAt,
		ExpiresAt:   doc.ExpiresAt,
		revocations: revoked,
	}, nil
}
