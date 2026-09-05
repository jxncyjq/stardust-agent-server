package trustlist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/plugin/sign"
)

// newSigner 定义在 document_test.go 里（同一个包），直接用。
// 它换一次 root、签多份清单；用了它的用例一律不加 t.Parallel()。

// serveList 是测试服务当前该作答的内容。改 list/sig 就能改下一次取回拿到的东西。
//
// 加锁不是形式：改内容的是用例所在的 goroutine，读内容的是 net/http 的处理
// goroutine，两者之间没有 -race 认得的先后关系。
type serveList struct {
	mu   sync.Mutex
	list []byte
	sig  []byte
	// onRequest 在每次请求作答之前调用（不持锁），用例用它模拟「就在这次取回
	// 期间，另一个进程动了缓存目录」。
	onRequest func()
}

func newServeList(list, sig []byte) *serveList { return &serveList{list: list, sig: sig} }

func (s *serveList) set(list, sig []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list, s.sig = list, sig
}

func (s *serveList) setOnRequest(hook func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRequest = hook
}

func (s *serveList) snapshot() (list, sig []byte, hook func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list, s.sig, s.onRequest
}

func newListServer(t *testing.T, cur *serveList) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, sig, hook := cur.snapshot()
		if hook != nil {
			hook()
		}
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write(sig)
			return
		}
		_, _ = w.Write(list)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestStore(t *testing.T, srv *httptest.Server, cacheDir string, now func() time.Time) *Store {
	t.Helper()
	s, err := NewStore(Config{
		URL:      srv.URL + "/trustlist.json",
		CacheDir: cacheDir,
		Client:   srv.Client(),
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// fixedNow 落在 testDoc 的 issued_at 与 expires_at 之间。
func fixedNow() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) }

// afterExpiry 落在 testDoc 的 expires_at 之后。
func afterExpiry() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }

// withSecondKeyRevoking 返回一个给 newSigner 用的 mutate：往 keyring.keys 里加
// 第二把钥匙 dev-other，并把 keyring.revoked 设成 ids。
//
// 第二把钥匙不是装饰：testDoc 的 keys 里只有 dev-abc，撤销它就是「每把钥匙都被
// 撤销」，而 sign.ParseKeyring 对那种信任集是硬拒的。没有它，想考撤销的用例会卡
// 在解析那一步，考不到它真正想考的东西。
func withSecondKeyRevoking(t *testing.T, ids ...sign.KeyID) func(m map[string]any) {
	t.Helper()
	return func(m map[string]any) {
		pub, _, err := sign.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		entry, err := sign.MarshalKeyEntry("dev-other", pub)
		if err != nil {
			t.Fatalf("MarshalKeyEntry: %v", err)
		}
		var entryMap map[string]any
		if err := json.Unmarshal(entry, &entryMap); err != nil {
			t.Fatalf("unmarshal key entry: %v", err)
		}
		kr, ok := m["keyring"].(map[string]any)
		if !ok {
			t.Fatalf("testDoc 的 keyring 不是 map[string]any：%T", m["keyring"])
		}
		keys, ok := kr["keys"].([]any)
		if !ok {
			t.Fatalf("testDoc 的 keyring.keys 不是 []any：%T", kr["keys"])
		}
		kr["keys"] = append(keys, entryMap)
		if len(ids) == 0 {
			return
		}
		revoked := make([]any, 0, len(ids))
		for _, id := range ids {
			revoked = append(revoked, map[string]any{
				"key_id":     string(id),
				"revoked_at": "2026-08-29T10:00:00Z",
				"reason":     "私钥泄漏",
			})
		}
		kr["revoked"] = revoked
	}
}

// countPublishes 记下 write 依次发布了哪些文件名。
//
// 它换掉包级的 writeFileAtomically（与 export_test.go 里的 failPublishing 同一个
// 入口），所以用了它的用例不能 t.Parallel()。
func countPublishes(t *testing.T) func() []string {
	t.Helper()
	previous := writeFileAtomically
	var mu sync.Mutex
	var names []string
	writeFileAtomically = func(c *cache, name string, data []byte) error {
		mu.Lock()
		names = append(names, name)
		mu.Unlock()
		return previous(c, name, data)
	}
	t.Cleanup(func() { writeFileAtomically = previous })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), names...)
	}
}

func TestNewStoreRefusesAnUnusableConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"url 为空", Config{URL: "  ", CacheDir: t.TempDir()}, "url"},
		{"url 不以 .json 结尾", Config{URL: "https://example.com/list.txt", CacheDir: t.TempDir()}, ".json"},
		{"缓存目录为空", Config{URL: "https://example.com/list.json"}, "cache dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewStore(tc.cfg)
			if err == nil {
				t.Fatalf("%s 被接受了", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里没提到 %q：%v", tc.want, err)
			}
		})
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()

	cases := map[Status]string{
		StatusUnavailable: "unavailable",
		StatusStale:       "stale",
		StatusFresh:       "fresh",
		Status(42):        "Status(42)",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(status), got, want)
		}
	}
}

func TestRefreshAcceptsAndCachesAFreshList(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if trust.Status != StatusFresh {
		t.Errorf("Status = %v, want StatusFresh", trust.Status)
	}
	if trust.Serial != 7 {
		t.Errorf("Serial = %d, want 7", trust.Serial)
	}
	if trust.Keyring == nil {
		t.Fatal("Keyring 是 nil")
	}
	if got := trust.Publishers["dev-abc"].DisplayName; got != "张三" {
		t.Errorf("Publishers[dev-abc].DisplayName = %q, want 张三", got)
	}

	// Current 只读缓存、不发请求：把服务关掉再问一次，它必须照样答得出来。
	srv.Close()
	again, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if again.Serial != 7 {
		t.Errorf("Current().Serial = %d, want 7", again.Serial)
	}
	if again.Status != StatusFresh {
		t.Errorf("Current().Status = %v, want StatusFresh", again.Status)
	}
}

// TestRefreshRefusesASerialRollback 挡的是对这套机制最便宜的攻击：重放一份
// 签名完全合法的旧清单，把用户挡在某次撤销之前。
func TestRefreshRefusesASerialRollback(t *testing.T) {
	signer := newSigner(t)
	list9, sig9 := signer(9, nil)
	cur := newServeList(list9, sig9)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(9): %v", err)
	}
	// 换成一份 serial 更小、但签名同样合法的清单（同一把 root 签的）。
	list7, sig7 := signer(7, nil)
	cur.set(list7, sig7)

	trust, err := store.Refresh(context.Background())
	if !errors.Is(err, ErrSerialRegressed) {
		t.Fatalf("Refresh(7 after 9)：err = %v，want 裹 ErrSerialRegressed", err)
	}
	// 拒绝之后手上仍然是 9，而不是掉回 unavailable。
	if trust.Serial != 9 {
		t.Errorf("拒绝回滚后 Serial = %d, want 9", trust.Serial)
	}
	if trust.Status != StatusFresh {
		t.Errorf("拒绝回滚后 Status = %v, want StatusFresh", trust.Status)
	}
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cached.Serial != 9 {
		t.Errorf("缓存被回滚的清单覆盖了：Serial = %d", cached.Serial)
	}
}

// TestRefreshRefusesTheSameSerialWithDifferentContent：发布侧改了内容却没进
// serial，是发布流程事故；无法判断哪一份才是当前的，所以拒绝。
func TestRefreshRefusesTheSameSerialWithDifferentContent(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// 同一个 root 签的同一个 serial，但内容不同：换掉 publisher 的显示名。
	list2, sig2 := signer(7, func(m map[string]any) {
		m["publishers"] = []any{
			map[string]any{"key_id": "dev-abc", "display_name": "李四", "contact": ""},
		}
	})
	cur.set(list2, sig2)

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("serial 相同但内容不同的清单被接受了")
	}
	if trust.Serial != 7 {
		t.Errorf("拒绝之后 Serial = %d, want 7", trust.Serial)
	}
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got := cached.Publishers["dev-abc"].DisplayName; got != "张三" {
		t.Errorf("缓存被那份同 serial 的清单覆盖了：DisplayName = %q, want 张三", got)
	}
}

// TestRefreshOnAnIdenticalListDoesNotRewriteTheCache：serial 相同且逐字相同 →
// 无变化。重写缓存不只是多余的 I/O，它还会把「上一次真正变过的时刻」抹掉。
func TestRefreshOnAnIdenticalListDoesNotRewriteTheCache(t *testing.T) {
	signer := newSigner(t)
	published := countPublishes(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	first := published()
	if len(first) != 3 {
		t.Fatalf("第一次刷新发布了 %v，want 三个文件", first)
	}

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("对同一份清单再刷新一次：%v", err)
	}
	if trust.Serial != 7 || trust.Status != StatusFresh {
		t.Errorf("Serial = %d, Status = %v，want 7 / StatusFresh", trust.Serial, trust.Status)
	}
	if got := published(); len(got) != len(first) {
		t.Errorf("逐字相同的清单又被写了一次：%v", got[len(first):])
	}
}

// TestRefreshOnAFailureReturnsBothTheCachedTrustAndTheError：网络断了的时候，
// 调用方两件事都需要知道——拉取失败了，以及手上还有一份能用的。
func TestRefreshOnAFailureReturnsBothTheCachedTrustAndTheError(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.Close() // 断网

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("服务器关掉之后 Refresh 却成功了")
	}
	if trust.Status == StatusUnavailable {
		t.Error("一次网络失败把状态降成了 unavailable；缓存还在，它应该继续可用")
	}
	if trust.Serial != 7 {
		t.Errorf("失败时返回的 Serial = %d, want 7", trust.Serial)
	}
	if trust.Keyring == nil {
		t.Error("失败时返回的 Keyring 是 nil；缓存那份仍然可用")
	}
}

// TestNoCacheAndNoNetworkIsUnavailable：从没成功取到过、且这次也没拉到 →
// unavailable，且 Keyring 必须是 nil。
//
// 调用方看到 unavailable 必须把所有插件按「未登记」处理。反过来做的诱惑很实在
// （「拉不到就先放行吧」），而那等于给了攻击者一个把清单打掉就全线放行的开关。
// 这里断言 Keyring == nil，让「拿它去放行」在类型上就做不到。
func TestNoCacheAndNoNetworkIsUnavailable(t *testing.T) {
	cur := newServeList(nil, nil)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)
	srv.Close()

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("既无缓存又无网络时 Refresh 却成功了")
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
	if trust.Keyring != nil {
		t.Error("unavailable 却带回了一个非 nil 的 Keyring")
	}
}

// TestUnavailableNeverCarriesAKeyring 把上一条的断言铺到每一条能产出 unavailable
// 的路径上：既无缓存又无网络、缓存损坏、以及只读缓存的 Current。
//
// 分开写是因为它守的不是某一条路径的行为，而是一条跨路径的不变量：Status 为
// unavailable 时 Keyring 必须是 nil。少守住任何一条，「拿不到清单就先放行」就
// 又变得写得出来了。
func TestUnavailableNeverCarriesAKeyring(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)

	assertBlank := func(t *testing.T, name string, trust Trust, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s：err 是 nil，want 一个错误", name)
		}
		if trust.Status != StatusUnavailable {
			t.Fatalf("%s：Status = %v, want StatusUnavailable", name, trust.Status)
		}
		if trust.Keyring != nil {
			t.Errorf("%s：unavailable 却带回了一个非 nil 的 Keyring", name)
		}
		if trust.Publishers != nil {
			t.Errorf("%s：unavailable 却带回了 %d 个 publisher", name, len(trust.Publishers))
		}
	}

	// 空缓存目录上的 Current。
	emptyDir := t.TempDir()
	emptyStore := newTestStore(t, srv, emptyDir, fixedNow)
	trust, err := emptyStore.Current()
	assertBlank(t, "空缓存上的 Current", trust, err)

	// 损坏缓存上的 Current 与 Refresh。
	corruptDir := t.TempDir()
	corruptStore := newTestStore(t, srv, corruptDir, fixedNow)
	if _, err := corruptStore.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, listFileName), []byte(`{"serial":`), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	trust, err = corruptStore.Current()
	assertBlank(t, "损坏缓存上的 Current", trust, err)
	trust, err = corruptStore.Refresh(context.Background())
	assertBlank(t, "损坏缓存上的 Refresh", trust, err)

	// 既无缓存又无网络。
	srv.Close()
	trust, err = emptyStore.Refresh(context.Background())
	assertBlank(t, "无缓存且无网络的 Refresh", trust, err)
}

// TestAnExpiredListIsStaleButUsable：过期不作废。断网久了清单会过期，
// 而作废意味着所有插件立刻失信——可用性代价大于收益。
func TestAnExpiredListIsStaleButUsable(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), afterExpiry)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if trust.Status != StatusStale {
		t.Errorf("Status = %v, want StatusStale", trust.Status)
	}
	if trust.Keyring == nil {
		t.Error("过期的清单不该让 Keyring 变成 nil")
	}
	// 只读路径必须给出同一个判定，否则「过期」会随着走哪条路而变。
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cached.Status != StatusStale {
		t.Errorf("Current().Status = %v, want StatusStale", cached.Status)
	}
}

// TestRefreshRefusesToOverwriteACorruptCache：缓存损坏时停下，不继续取回、
// 不落盘。
//
// 继续的两个后果都不可接受：这一轮没有可比的 serial 基准，一份重放的旧清单会被
// 当成新的收下（防回滚保护正是靠缓存里那个 serial）；而一次成功的发布会把损坏
// 的那几个文件覆盖掉，把「是磁盘坏了还是有人动过」的唯一现场抹平。损坏要人来
// 看一眼，不能靠一次成功的取回悄悄「修好」。
func TestRefreshRefusesToOverwriteACorruptCache(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// 把清单文件改坏，再记下磁盘上此刻的两份内容。
	corrupt := []byte(`{"serial":`)
	if err := os.WriteFile(filepath.Join(cacheDir, listFileName), corrupt, 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	beforeRevoked, err := os.ReadFile(filepath.Join(cacheDir, revokedFileName))
	if err != nil {
		t.Fatalf("read %s: %v", revokedFileName, err)
	}

	// 服务这时端上来的是一份 serial 更大、签名完全合法的清单——正常路径下它会
	// 被接受。缓存损坏必须让它止步于此。
	list9, sig9 := signer(9, nil)
	cur.set(list9, sig9)

	if _, err := store.Refresh(context.Background()); err == nil {
		t.Fatal("缓存损坏时 Refresh 却成功了——它会把损坏的现场覆盖掉，且这一轮没有可比的 serial")
	}
	afterList, err := os.ReadFile(filepath.Join(cacheDir, listFileName))
	if err != nil {
		t.Fatalf("read %s: %v", listFileName, err)
	}
	if !bytes.Equal(afterList, corrupt) {
		t.Errorf("损坏的清单被一次刷新悄悄修好了：%s", afterList)
	}
	afterRevoked, err := os.ReadFile(filepath.Join(cacheDir, revokedFileName))
	if err != nil {
		t.Fatalf("read %s: %v", revokedFileName, err)
	}
	if !bytes.Equal(beforeRevoked, afterRevoked) {
		t.Errorf("撤销累积集在一次被拒的 Refresh 之后被改写了：\nbefore=%s\nafter=%s",
			beforeRevoked, afterRevoked)
	}
}

// TestRevocationSurvivesAcrossRefreshes 是端到端版本的「撤销永不遗忘」：
// 走完整的取回 → 落盘 → 再取回 → 装配，确认撤销没有在这条链上丢掉。
//
// revoked_test.go 里的同名规则只覆盖内存里的合并；这一条覆盖它穿过缓存的往返。
func TestRevocationSurvivesAcrossRefreshes(t *testing.T) {
	signer := newSigner(t)

	// A：撤销 dev-abc。
	listA, sigA := signer(7, withSecondKeyRevoking(t, "dev-abc"))
	cur := newServeList(listA, sigA)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)
	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(A): %v", err)
	}

	// B：serial 更大，但 revoked 里没有 dev-abc 了。
	listB, sigB := signer(9, withSecondKeyRevoking(t))
	cur.set(listB, sigB)
	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh(B): %v", err)
	}
	rev, ok := trust.Keyring.Revoked("dev-abc")
	if !ok {
		t.Fatal("dev-abc 在 B 之后不再是撤销状态——撤销穿过缓存时丢了")
	}
	if rev.Reason != "私钥泄漏" {
		t.Errorf("撤销理由丢了：%q", rev.Reason)
	}
	// 只读路径也必须看得见它。
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if _, ok := cached.Keyring.Revoked("dev-abc"); !ok {
		t.Error("Current() 看不到 dev-abc 的撤销")
	}
}

// TestRefreshStartsFromTheRecordedRevocationsWhenTheListIsMissing：清单文件没了、
// 撤销记录还在（一次首写崩溃、一次磁盘损坏，或者任何能写这个目录的东西删掉一个
// 文件，都会造出这个形态）。
//
// 这一轮必须从磁盘上那份撤销记录起步。以空集起步的话，一把这台机器早已记录为撤销
// 的钥匙就会在这一轮里重新可信——攻击者不需要伪造任何东西，删一个文件就够了。
func TestRefreshStartsFromTheRecordedRevocationsWhenTheListIsMissing(t *testing.T) {
	signer := newSigner(t)
	// 服务端这份清单的 revoked 是空的：dev-abc 的撤销只存在于磁盘那份记录里。
	list, sig := signer(7, withSecondKeyRevoking(t))
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)

	cacheDir := t.TempDir()
	recorded := []byte(`{"revoked":[{"key_id":"dev-abc","revoked_at":"2026-08-29T10:00:00Z",` +
		`"reason":"这台机器早就记下了"}]}`)
	if err := os.WriteFile(filepath.Join(cacheDir, revokedFileName), recorded, 0o600); err != nil {
		t.Fatalf("seed %s: %v", revokedFileName, err)
	}
	store := newTestStore(t, srv, cacheDir, fixedNow)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	rev, ok := trust.Keyring.Revoked("dev-abc")
	if !ok {
		t.Fatal("清单文件缺失的那一轮把已记录的撤销丢了")
	}
	if rev.Reason != "这台机器早就记下了" {
		t.Errorf("撤销理由被清单里的空 revoked 段顶掉了：%q", rev.Reason)
	}
}

// TestRefreshKeepsRecordedRevocationsWhenTheRecordVanishesMidRefresh 把上一条的
// 规则钉在唯一能把它单独看见的时序上：撤销记录在这次取回**期间**消失了（另一个
// 进程的清理、一次磁盘故障，或者任何能写这个目录的东西删掉它）。
//
// 平时看不见，是因为 cache.write 自己也会把磁盘上那份并进来，从空集起步与从快照
// 起步落到同一个并集。记录消失之后那条退路就没了：这一轮里唯一还记得 dev-abc 被
// 撤销的，就是 Refresh 开头从 cache.read 拿到的那份快照。
func TestRefreshKeepsRecordedRevocationsWhenTheRecordVanishesMidRefresh(t *testing.T) {
	signer := newSigner(t)
	// 服务端这份清单的 revoked 是空的：dev-abc 的撤销只存在于那份即将消失的记录里。
	list, sig := signer(7, withSecondKeyRevoking(t))
	cur := newServeList(list, sig)
	cacheDir := t.TempDir()
	recordPath := filepath.Join(cacheDir, revokedFileName)
	recorded := []byte(`{"revoked":[{"key_id":"dev-abc","revoked_at":"2026-08-29T10:00:00Z",` +
		`"reason":"这台机器早就记下了"}]}`)
	if err := os.WriteFile(recordPath, recorded, 0o600); err != nil {
		t.Fatalf("seed %s: %v", revokedFileName, err)
	}
	// 取回已经开始，说明 Refresh 早已读完缓存。此刻删掉记录。
	cur.setOnRequest(func() {
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("模拟记录消失：%v", err)
		}
	})
	store := newTestStore(t, newListServer(t, cur), cacheDir, fixedNow)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	rev, ok := trust.Keyring.Revoked("dev-abc")
	if !ok {
		t.Fatal("记录一消失，这台机器就把 dev-abc 的撤销忘了——这一轮是从空集起步的")
	}
	if rev.Reason != "这台机器早就记下了" {
		t.Errorf("撤销理由 = %q", rev.Reason)
	}
}

// TestRefreshAdoptsTheRevokedSetReturnedByWrite：另一个进程在这次取回期间并入了
// 一条新撤销。
//
// Store 手上那份快照是取回之前读出来的，恒比磁盘旧。落盘走的是 cache.write，
// 而它返回的是**它实际写进磁盘的那份并集**；不拿这个返回值取代快照，装配出来的
// 信任集就会少掉那条撤销——少的正好是止血手段。
func TestRefreshAdoptsTheRevokedSetReturnedByWrite(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	cacheDir := t.TempDir()
	// dev-ghost 不在清单的 keys 里：撤销一个未登记的 id 是合法的，而且它保证这条
	// 记录只可能来自磁盘，不可能来自这次取回的清单。
	ghost := []byte(`{"revoked":[{"key_id":"dev-ghost","revoked_at":"2026-08-29T10:00:00Z",` +
		`"reason":"另一个进程记下的"}]}`)
	cur.setOnRequest(func() {
		if err := os.WriteFile(filepath.Join(cacheDir, revokedFileName), ghost, 0o600); err != nil {
			t.Errorf("模拟另一个进程写 %s：%v", revokedFileName, err)
		}
	})
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, cacheDir, fixedNow)

	trust, err := store.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	rev, ok := trust.Keyring.Revoked("dev-ghost")
	if !ok {
		t.Fatal("装配用的是取回之前那份过期快照，另一个进程刚并入的撤销没被采纳")
	}
	if rev.Reason != "另一个进程记下的" {
		t.Errorf("撤销理由 = %q", rev.Reason)
	}
}

// TestRefreshReportsAWriteThatPublishedButFailedToUnlock 钉住一个说不清的中间
// 态：三个文件都发布成功了，只有释放目录锁那一步失败。
//
// cache.write 此时返回 nil 并集加一个只讲锁的错误，Store 无从知道磁盘其实已经推进
// 了一轮。它只能按契约办：这次刷新算失败，返回缓存那份旧的（这一轮的信任集不会
// 比刷新之前更宽松），并把错误如实往上报。磁盘上那份新的会在下一次读缓存时被采用。
func TestRefreshReportsAWriteThatPublishedButFailedToUnlock(t *testing.T) {
	signer := newSigner(t)
	list7, sig7 := signer(7, nil)
	cur := newServeList(list7, sig7)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(7): %v", err)
	}

	// 发布完最后一个文件（清单）之后立刻把锁文件删掉，让 write 的释放锁那一步
	// 失败——三个文件已经全部落盘，锁却放不开。
	previous := writeFileAtomically
	writeFileAtomically = func(c *cache, name string, data []byte) error {
		if err := previous(c, name, data); err != nil {
			return err
		}
		if name == listFileName {
			if err := os.Remove(c.path(lockFileName)); err != nil {
				t.Errorf("删除锁文件：%v", err)
			}
		}
		return nil
	}
	t.Cleanup(func() { writeFileAtomically = previous })

	list9, sig9 := signer(9, nil)
	cur.set(list9, sig9)

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("释放锁失败却被当成一次成功的刷新")
	}
	if trust.Serial != 7 {
		t.Errorf("失败时返回的 Serial = %d, want 7（缓存里那份旧的）", trust.Serial)
	}
	// 磁盘确实已经推进了一轮，下一次读缓存就会看到它。
	cached, err := store.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cached.Serial != 9 {
		t.Errorf("磁盘上的 Serial = %d, want 9", cached.Serial)
	}
}

// TestRefreshRejectsAListSignedByAnUnknownKey 守的是「取回这条路径确实走了
// VerifyDocument」这条接线本身。
//
// 这份清单形状完全合法、serial 完全正常，只有签名是另一把私钥做的。少了这条
// 用例，把取回路径上的 VerifyDocument 换成 ParseDocument 不会让本包任何测试变红：
// 一份陌生私钥签的清单会被收下、装配成 StatusFresh，还会落盘顶掉本机的缓存。
// document_test.go 考的是 VerifyDocument 自己，cache_test.go 的
// TestCorruptCacheIsNeverSilentlyRepaired 考的是读缓存那条路径——两者都不经过
// 取回这一条。
func TestRefreshRejectsAListSignedByAnUnknownKey(t *testing.T) {
	signer := newSigner(t)
	list, _ := signer(7, nil)
	// 陌生私钥：它不在 root 信任集里，冒用 root 的 key_id 也验不过。
	_, impostor, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	forged, err := sign.Sign(impostor, "test-root", list)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	forgedSig, err := sign.MarshalSignature(forged)
	if err != nil {
		t.Fatalf("MarshalSignature: %v", err)
	}

	cur := newServeList(list, forgedSig)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("一份陌生私钥签的清单被 Refresh 收下了")
	}
	if !errors.Is(err, ErrUntrustedList) {
		t.Errorf("错误没裹 ErrUntrustedList：%v", err)
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
	if trust.Keyring != nil {
		t.Error("被拒的清单却带回了一个非 nil 的 Keyring")
	}
	// 它也不能落盘：验签不过的清单一旦写进缓存，下一次读缓存要么把它当成可信的，
	// 要么把整个缓存报成损坏——两个后果都不可接受。
	if _, err := os.Stat(filepath.Join(cacheDir, listFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("被拒的清单被写进了缓存：stat %s = %v", listFileName, err)
	}
}

// TestRefreshRefusesAnOversizedSignature 守的是「签名文档用的是 maxSigBytes」。
//
// 4 KiB 是签名文档唯一的内存/带宽防线，而清单那道是 1 MiB——差 256 倍。断言必须
// 落在**错误的种类**上（错误里得有 4096 这道上限）：只断言「失败了」是不够的，
// 把上限换成 maxListBytes 之后这 8 KiB 照样会失败，只是失败在后面的
// sign.ParseSignature 那一步，用例会照绿。
func TestRefreshRefusesAnOversizedSignature(t *testing.T) {
	signer := newSigner(t)
	list, _ := signer(7, nil)
	cur := newServeList(list, bytes.Repeat([]byte("a"), 8<<10))
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("一份 8 KiB 的签名文档被收下了")
	}
	if want := "body exceeds 4096 bytes"; !strings.Contains(err.Error(), want) {
		t.Errorf("错误里没有那道签名上限（%q）——取签名文档用的不是 maxSigBytes：%v", want, err)
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
}

// TestRefreshDoesNotHoldTheListToTheSignatureLimit 是上一条的另一半：清单那道
// 上限是 1 MiB，不是签名那 4 KiB。
//
// 两条取回只差一个参数，写反了不会有任何编译错误。一份登记了足够多发布者的清单
// 会超过 4 KiB，所以这里端上 8 KiB 的清单：它必须走到验签才被拒，不能在体积那
// 一关就被拦下。
func TestRefreshDoesNotHoldTheListToTheSignatureLimit(t *testing.T) {
	signer := newSigner(t)
	_, sig := signer(7, nil)
	cur := newServeList(bytes.Repeat([]byte("a"), 8<<10), sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	_, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("一份签名对不上的清单被收下了")
	}
	if !errors.Is(err, ErrUntrustedList) {
		t.Errorf("8 KiB 的清单没走到验签就被拒了——取清单用的不是 maxListBytes：%v", err)
	}
	if strings.Contains(err.Error(), "body exceeds") {
		t.Errorf("8 KiB 的清单被体积上限拦下了：%v", err)
	}
}

// TestRefreshHonoursACancelledContext 守的是「ctx 一路传到取回」。
//
// 换成 context.Background() 不会有任何编译错误，后果是 Refresh 不再可取消：
// 调用方的超时与关停信号都拦不住一次已经开始的取回。
func TestRefreshHonoursACancelledContext(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	trust, err := store.Refresh(ctx)
	if err == nil {
		t.Fatal("ctx 已经取消，Refresh 却照样取回并成功了")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误链里没有 context.Canceled：%v", err)
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
}

// seedCacheThatCannotBeAssembled 把缓存目录置成「读得回来、却装不出信任集」：
// 清单与签名都合法（cache.read 成功），而磁盘上那份撤销累积集撤掉了清单里唯一
// 那把钥匙，于是 assemble 失败——sign.ParseKeyring 对「每把钥匙都被撤销」是硬拒的。
//
// Refresh 里的 fallbackErr 就是这一次 assemble 的错误，所以这个形态是下面两条
// 用例的共同前提。
func seedCacheThatCannotBeAssembled(t *testing.T, store *Store, cacheDir string) {
	t.Helper()
	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("填缓存的那次 Refresh: %v", err)
	}
	allRevoked := []byte(`{"revoked":[{"key_id":"dev-abc","revoked_at":"2026-08-29T10:00:00Z",` +
		`"reason":"清单里唯一那把钥匙也被撤销了"}]}`)
	if err := os.WriteFile(filepath.Join(cacheDir, revokedFileName), allRevoked, 0o600); err != nil {
		t.Fatalf("seed %s: %v", revokedFileName, err)
	}
	// 夹具自检：这个状态必须是「读得回来、装不出来」，否则下面两条考的就不是它们
	// 要考的东西。
	if _, err := store.Current(); err == nil {
		t.Fatal("夹具没造出预期状态：缓存居然还装得出信任集")
	}
}

// TestRefreshCarriesTheCacheAssemblyFailureAlongsideTheFetchError 守的是
// 「fallbackErr 跟着每一次失败一起交回去」。
//
// 缓存那份装不出信任集，这次又没拉到：调用方拿到的是一个 unavailable。只交回
// 网络错误的话，它看到的就是「拉取失败」加一个说不清为什么是空的状态——而真正
// 让状态变空的是缓存那份装不出来，那条才是要人来看的。
func TestRefreshCarriesTheCacheAssemblyFailureAlongsideTheFetchError(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)
	seedCacheThatCannotBeAssembled(t, store, cacheDir)

	srv.Close() // 断网：这一轮既拉不到，缓存那份也装不出来

	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("既拉不到、缓存也装不出信任集，Refresh 却成功了")
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
	if !strings.Contains(err.Error(), "assemble keyring") {
		t.Errorf("错误里没说明状态为什么是空的（缓存那份装不出信任集）：%v", err)
	}
	if !strings.Contains(err.Error(), listFileName) {
		t.Errorf("错误里没说明这次是从哪个地址没拉到：%v", err)
	}
}

// TestRefreshOnAnIdenticalListStillReportsAnUnusableCache 守的是「逐字相同」
// 那一支也要把 fallbackErr 交回去。
//
// 清单确实没变，但缓存那份装不出信任集：返回一个 unavailable 却配 nil error，
// 就是拿「清单没变」盖住「手上什么都没有」——调用方会以为这是一次正常的无变化。
func TestRefreshOnAnIdenticalListStillReportsAnUnusableCache(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	cacheDir := t.TempDir()
	store := newTestStore(t, srv, cacheDir, fixedNow)
	seedCacheThatCannotBeAssembled(t, store, cacheDir)

	// 服务端端上的还是那份逐字相同的清单。
	trust, err := store.Refresh(context.Background())
	if err == nil {
		t.Fatal("清单逐字未变就被当成一次成功的刷新，可缓存那份根本装不出信任集")
	}
	if !strings.Contains(err.Error(), "assemble keyring") {
		t.Errorf("错误里没说明缓存那份装不出信任集：%v", err)
	}
	if trust.Status != StatusUnavailable {
		t.Errorf("Status = %v, want StatusUnavailable", trust.Status)
	}
	if trust.Keyring != nil {
		t.Error("unavailable 却带回了一个非 nil 的 Keyring")
	}
}

// TestRefreshHoldsItsLockAcrossTheFetch 守的是 Store 注释写成硬规则的那条：
// 一次刷新是「读缓存 → 取回 → 比较 serial → 并入撤销 → 落盘」的读改写序列，两个
// 并发的它会各自拿着一份取回前的快照去写，后写的那个把先写的成果盖掉。
//
// 判据落在取回上：请求正在被应答时——也就是这段序列走到一半时——那把锁必须已经
// 被这次 Refresh 攥着。用 TryLock 而不是「起两个 goroutine 看会不会撞上」，是因为
// 后者要靠调度撞出竞争，绿了不说明问题；TryLock 在两个方向上都是确定的。
func TestRefreshHoldsItsLockAcrossTheFetch(t *testing.T) {
	signer := newSigner(t)
	list, sig := signer(7, nil)
	cur := newServeList(list, sig)
	srv := newListServer(t, cur)
	store := newTestStore(t, srv, t.TempDir(), fixedNow)

	// 钩子跑在 net/http 的处理 goroutine 上，锁由调用 Refresh 的那个 goroutine
	// 持有——正是另一个并发刷新会看到的视角。
	var lockWasFree atomic.Bool
	cur.setOnRequest(func() {
		if store.mu.TryLock() {
			store.mu.Unlock()
			lockWasFree.Store(true)
		}
	})

	if _, err := store.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if lockWasFree.Load() {
		t.Error("取回期间那把锁是空着的：另一个 Refresh 能同时挤进来，" +
			"两个都拿着取回前的快照去写，后写的会把先写的成果盖掉")
	}
}
