# 插件声明的主动取回 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让运维在 GUI 里能主动把一个未缓存远程插件的声明取回来看见，从而在**看得见的前提下**决定要不要授权——而不是被迫退回 CLI。

**Architecture:** 三步。①`internal/plugin/manifest` 加哨兵 `ErrUntrustedPackage`，让「拿到了但不可信」能被 `errors.Is` 认出来；②`PluginConsentService` 加 `Resolve` 方法 + `POST /v1/plugins/{name}/resolve` 端点，复用 `Grant` 的同一条取回链路但**到验签为止**，不碰 `plugins.json`；③GUI 加「取回声明」按钮与两类失败呈现。

**Tech Stack:** Go 1.26.0，module `github.com/stardust/legion-agent`；GUI 为 Wails + React + TypeScript，vitest 测试。**两仓均不新增第三方依赖**。

**Spec:** `docs/superpowers/specs/2026-08-27-plugin-resolve-declaration-design.md`（commit `fdd9358`）

## Global Constraints

- Fail-loud 铁律（两仓各自的 `CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；Go 错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装。
- 公开标识符必须有 Go doc 风格注释，以标识符名开头，且**不得与代码矛盾**。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**（不是包子集）——上一期 `TestOpenAPIGolden` 住在 `internal/compat` 却覆盖 `internal/server`，按包跑一路漏到最后一刻。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 做**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原并用 `git status` 核对。
- 所有测试用 `httptest.Server`，**不得触真实网络**。
- `internal/server` **永不** import `internal/plugin/loader`。
- 提交只 stage 本 task 自己的文件（显式路径），**永不 `git add -A`**——server 仓工作区有无关的运行时产物（`tasks.md`、`tasks/.owners.json`）。

## 两仓两分支

| 仓 | 路径 | 分支 |
|---|---|---|
| server | `F:\source\stardust\Legion\legion\legionAgent` | `feat/plugin-resolve-declaration` |
| GUI | `F:\source\stardust\Legion\legion\legionAgentGUI` | `feat/plugin-resolve-ui` |

Task 1-3 在 server 仓，Task 4-5 在 GUI 仓。**GUI 依赖 server 的端点契约**，故顺序不可颠倒。

## GUI 测试的两个环境坑（Task 4-5 必读）

1. vitest **必须从 `frontend/` 目录跑**——父目录另有一个没有 jsdom 的 vitest，会产生 `document is not defined` 的假失败。
2. 默认并行 worker 池在开发机上会崩，而**崩掉的 worker 会静默带走整个测试文件**。必须用
   `npx vitest run --pool=forks --poolOptions.forks.singleFork=true`，且**判据是看文件数**而非只看「全绿」——一个被丢掉的文件是靠不运行来通过的。当前基线：**49 文件 / 271 测试**。
3. `ChatPanel.tsx:175` 有一个 unhandled error 是**既有的**，不是本期引入，不要去追。

## 前置事实（已在两仓 master，直接用）

```go
// internal/plugin/manifest —— assemble.go:139-156
func verifyManifestSignature(dir string, manifestData []byte, keyring *sign.Keyring) error
func LoadPackage(dir string, keyring *sign.Keyring) (PluginManifest, []byte, error)

// internal/cli —— plugins_command.go:1892
func resolvePluginPackageDir(ctx context.Context, entry manifest.Entry, remote loader.RemoteConfig, root string) (string, error)
// 缓存命中直接返回目录；未命中走 fetch.Fetch + Cache.Put（会联网）

// internal/cli —— plugin_consent_service.go
func (s *PluginConsentService) List(ctx context.Context) ([]server.PluginView, error)      // :152
func (s *PluginConsentService) Grant(ctx context.Context, name string, req server.GrantRequest) (server.ConsentResult, error)  // :317
func (s *PluginConsentService) Deny(ctx context.Context, name string) (server.ConsentResult, error)  // :434
// s.mu / s.remote / s.root / s.keyringFn 均为既有字段

// internal/server —— plugins.go
type PluginConsent interface { List / Grant / Deny }          // :61
var ErrPluginNotFound, ErrPluginDeploymentChanged, ErrPluginStorage, ErrPluginUnavailable  // :14/:27/:34/:41
func pluginConsentStatus(err error) int                        // :313，纯 errors.Is → 状态码
func parsePluginConsentName(path, suffix string) (string, bool)
func (s *HTTPServer) handleDenyPlugin(w, r)                    // :270，RBAC → nil 检查 → 解析名字 → 调用 → 响应
// 路由 switch 在 http.go:311-314

// internal/security —— rbac.go
security.ActionWritePlugin / security.ResourcePlugin           // :30 / ResourcePlugin
```

---

### Task 1: 让「不可信」能被认出来

**Files:**
- Modify: `internal/plugin/manifest/assemble.go`（`verifyManifestSignature`，139-156 行）
- Test: `internal/plugin/manifest/assemble_test.go`

**Interfaces:**
- Produces:
  - `var manifest.ErrUntrustedPackage error`

`verifyManifestSignature` 今天四条失败路径全是 `fmt.Errorf` 字符串包装，`errors.Is` 认不出任何一条。Task 3 要把「拿不到」和「拿到了但不可信」分开呈现，类型必须先能表达这个区别。

**哨兵挂三条，第四条刻意不挂：**

| 路径 | 挂 | 理由 |
|---|---|---|
| `plugin.sig` 缺失（`fs.ErrNotExist`） | 是 | 部署要求签名而包没有——这个包不可信，重试无用 |
| `ParseSignature` 失败 | 是 | 签名文件本身是坏的 |
| `keyring.Verify` 失败 | 是 | 核心验签不通过 |
| 读 `plugin.sig` 的其它 I/O 错误 | **否** | 磁盘/权限问题，属于「拿不到」，重试有意义 |

第四条排除是本 task 的核心判断：它与源站超时同类，把它归进安全告警会让真正的安全事件贬值。

- [ ] **Step 1: 写失败测试**

在 `internal/plugin/manifest/assemble_test.go` 追加。夹具沿用该文件既有的包构造方式（先读一遍它现有的 `LoadPackage` 测试怎么造 `dir` + keyring）。

```go
func TestLoadPackageMarksAnUnsignedPackageUntrusted(t *testing.T) {
	// 包齐全但没有 plugin.sig，且部署要求签名（keyring 非 nil）
	dir := writeTestPackage(t) // 既有夹具助手：写 plugin.json + plugin.wasm
	kr := testKeyring(t)       // 既有夹具助手
	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage on an unsigned package = nil error, want an untrusted-package error")
	}
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

func TestLoadPackageMarksACorruptSignatureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	if err := os.WriteFile(filepath.Join(dir, "plugin.sig"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt plugin.sig: %v", err)
	}
	_, _, err := LoadPackage(dir, kr)
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

func TestLoadPackageMarksAWrongSignatureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	// 用一把 keyring 不信任的密钥签名
	signTestPackageWithForeignKey(t, dir) // 见 Step 3 说明；若既有助手可用则复用
	_, _, err := LoadPackage(dir, kr)
	if !errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want it to wrap ErrUntrustedPackage", err)
	}
}

// 第四条：I/O 故障不是信任问题，必须 NOT 命中哨兵。
// 用一个目录冒充 plugin.sig：读它会得到 EISDIR 一类的 I/O 错误，而非 fs.ErrNotExist。
func TestLoadPackageDoesNotMarkAnIOFailureUntrusted(t *testing.T) {
	dir := writeTestPackage(t)
	kr := testKeyring(t)
	if err := os.Mkdir(filepath.Join(dir, "plugin.sig"), 0o755); err != nil {
		t.Fatalf("mkdir plugin.sig: %v", err)
	}
	_, _, err := LoadPackage(dir, kr)
	if err == nil {
		t.Fatal("LoadPackage with an unreadable plugin.sig = nil error, want an I/O error")
	}
	if errors.Is(err, ErrUntrustedPackage) {
		t.Errorf("LoadPackage error = %v, want an I/O failure NOT classified as untrusted: "+
			"a disk or permission problem is a retryable environment fault, not a trust verdict", err)
	}
}
```

若 `writeTestPackage` / `testKeyring` / 用外来密钥签名的助手在该测试文件里不存在，就按该文件既有的构造方式现写，**不要**新建 testdata 文件。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/plugin/manifest/ -run TestLoadPackageMarks -count=1 -timeout 120s -p 1
```

预期：编译失败 `undefined: ErrUntrustedPackage`。

- [ ] **Step 3: 实现**

在 `assemble.go` 里 `verifyManifestSignature` 上方加哨兵：

```go
// ErrUntrustedPackage marks the failures that mean "this package is not
// trustworthy", as opposed to "the package could not be obtained". A caller
// showing a human why a fetch failed needs that distinction: an untrusted
// package will never become trustworthy by retrying, and offering a retry for
// it would be a control that cannot work.
//
// It deliberately does NOT cover an I/O error while reading plugin.sig. A
// disk or permission fault is an environment problem in the same class as a
// source-site timeout — retrying it is meaningful — and folding it in here
// would devalue the signal for the failures that really are trust verdicts.
var ErrUntrustedPackage = errors.New("plugin package is not trusted")
```

改写 `verifyManifestSignature` 的三条路径（保留每条原有措辞，只挂上哨兵）：

```go
func verifyManifestSignature(dir string, manifestData []byte, keyring *sign.Keyring) error {
	sigPath := filepath.Join(dir, "plugin.sig")
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("plugin.sig is missing: this deployment requires a signed package: %w: %w",
				ErrUntrustedPackage, err)
		}
		// NOT ErrUntrustedPackage: see that sentinel's doc comment.
		return fmt.Errorf("read plugin.sig: %w", err)
	}
	sig, err := sign.ParseSignature(sigData)
	if err != nil {
		return fmt.Errorf("parse plugin.sig: %w: %w", ErrUntrustedPackage, err)
	}
	if err := keyring.Verify(sig, manifestData); err != nil {
		return fmt.Errorf("verify plugin.json signature: %w: %w", ErrUntrustedPackage, err)
	}
	return nil
}
```

`%w: %w` 是 Go 1.20+ 的多重包装，两个哨兵都能被 `errors.Is` 命中。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/plugin/manifest/ -count=1 -timeout 120s -p 1
go test ./... -count=1 -timeout 900s
```

预期：全部 `ok`。

- [ ] **Step 5: 变异验证**

把第四条也挂上哨兵（`return fmt.Errorf("read plugin.sig: %w: %w", ErrUntrustedPackage, err)`），重跑：

```bash
go test ./internal/plugin/manifest/ -run TestLoadPackageDoesNotMarkAnIOFailureUntrusted -count=1 -timeout 120s -p 1 -v
```

预期：该测试 **FAIL**。把输出留进报告，然后**还原**并 `git status` 核对。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/manifest/assemble.go internal/plugin/manifest/assemble_test.go
git commit -m "feat(plugin): tell an untrustworthy package from an unobtainable one"
```

---

### Task 2: `Resolve` —— 只取回，不授权

**Files:**
- Modify: `internal/cli/plugin_consent_service.go`
- Test: `internal/cli/plugin_consent_service_test.go`

**Interfaces:**
- Consumes: `manifest.ErrUntrustedPackage`（Task 1）
- Produces:
  - `func (s *PluginConsentService) Resolve(ctx context.Context, name string) (server.PluginView, error)`

与 `Grant` 走**同一条链路**（`consent.ReadDeploymentWithSnapshot` → `consent.FindEntry` → `resolvePluginPackageDir` → `manifest.LoadPackage`），但**到验签为止**：不碰 `plugins.json`，不写 `grant` 段，不触发收敛。

**四条不变量：**

1. **`plugins.json` 一字节不动。** 取回只是「看清楚」。运维可以取回完就走人。
2. **持锁。** `Grant`/`Deny` 都在方法第一件事 `s.mu.Lock()` + `defer Unlock()`，`Resolve` 同样——它与它们竞争同一个缓存目录与同一份清单读取。
3. **沿用全部前置检查。** 明文 `http://` 未开则拒、缓存未配置则拒——这些检查在 `resolvePluginPackageDir` 内部与其调用点已有，复用即可，**不要**另写一套。
4. **返回现成的 `server.PluginView`**，不新增 DTO。成功时 `DeclaredUnresolved` 为 false、三个 `Declared*` 填满。

**错误分类**：`Resolve` 的错误必须像 `Grant` 一样包装到 `internal/server` 的哨兵上（`ErrPluginNotFound` 等），Task 3 才能机械映射状态码。**新增一类**：包不可信时，错误链上同时带 `manifest.ErrUntrustedPackage`。

- [ ] **Step 1: 写失败测试**

在 `internal/cli/plugin_consent_service_test.go` 追加。沿用该文件既有的夹具助手（先读一遍 `Grant` 的测试怎么搭 service + httptest 源站）。

```go
func TestPluginConsentServiceResolveFillsDeclarationsWithoutTouchingTheManifest(t *testing.T) {
	// 一条远程条目，缓存未命中，源站可达
	f := newConsentFixture(t) // 既有夹具助手
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json: %v", err)
	}

	view, err := f.svc.Resolve(context.Background(), f.pluginName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if view.DeclaredUnresolved {
		t.Error("view.DeclaredUnresolved = true after a successful Resolve, want false")
	}
	if len(view.DeclaredCaps) == 0 {
		t.Error("view.DeclaredCaps is empty after a successful Resolve, want the plugin's declared capabilities")
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("re-read plugins.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("plugins.json changed during Resolve:\nbefore: %s\nafter:  %s", before, after)
	}
}

// 缓存命中不联网：先取回一次填满缓存，然后 CLOSE 掉源站再取回一次。
// 任何意外的第二次 fetch 都会 connection-refused 而不是静默成功。
func TestPluginConsentServiceResolveDoesNotRefetchOnACacheHit(t *testing.T) {
	f := newConsentFixture(t)
	if _, err := f.svc.Resolve(context.Background(), f.pluginName); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	f.origin.Close() // 源站下线

	view, err := f.svc.Resolve(context.Background(), f.pluginName)
	if err != nil {
		t.Fatalf("second Resolve after the origin went away = %v, want it served from cache", err)
	}
	if view.DeclaredUnresolved {
		t.Error("view.DeclaredUnresolved = true on a cache hit, want false")
	}
}

func TestPluginConsentServiceResolveReportsAnUntrustedPackage(t *testing.T) {
	f := newConsentFixtureWithUntrustedPackage(t) // 源站给出的包签名不被 keyring 信任
	before, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("read plugins.json: %v", err)
	}

	_, err = f.svc.Resolve(context.Background(), f.pluginName)
	if err == nil {
		t.Fatal("Resolve on an untrusted package = nil error, want an error")
	}
	if !errors.Is(err, manifest.ErrUntrustedPackage) {
		t.Errorf("Resolve error = %v, want it to wrap manifest.ErrUntrustedPackage", err)
	}

	after, err := os.ReadFile(f.manifestPath)
	if err != nil {
		t.Fatalf("re-read plugins.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("plugins.json changed while Resolve was rejecting an untrusted package")
	}
}

func TestPluginConsentServiceResolveReportsAnUnknownEntry(t *testing.T) {
	f := newConsentFixture(t)
	_, err := f.svc.Resolve(context.Background(), "no-such-plugin")
	if !errors.Is(err, server.ErrPluginNotFound) {
		t.Errorf("Resolve error = %v, want it to wrap server.ErrPluginNotFound", err)
	}
}
```

若 `newConsentFixture` / `newConsentFixtureWithUntrustedPackage` 不存在，按该文件既有的搭法现写；**不要**新建 testdata 文件，恶意/不可信夹具一律测试里现造。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/cli/ -run TestPluginConsentServiceResolve -count=1 -timeout 300s -p 1
```

预期：编译失败 `s.svc.Resolve undefined`。

- [ ] **Step 3: 实现**

在 `plugin_consent_service.go` 里 `Grant` 附近加：

```go
// Resolve fetches and verifies the package behind one deployment entry so the
// caller can SEE what the plugin declares, and stops there: it never touches
// plugins.json, never writes a grant, never converges. It exists because a
// remote entry whose package is not cached reports DeclaredUnresolved from
// List — GET must not carry a network fetch as a side effect — which leaves an
// operator unable to review what they would be authorizing. This is the
// deliberate, operator-initiated fetch that closes that gap.
//
// It runs the SAME chain Grant does (read the deployment, find the entry,
// resolvePluginPackageDir, manifest.LoadPackage) and reuses every precondition
// that chain enforces: a plaintext http source is still refused unless the
// deployment opted in, and a missing plugin cache is still refused. Resolving
// through a second, laxer path would be a way around those checks.
//
// The package is left in the cache on success. That is intended — it is what
// spares a following Grant a second download, and it matches what the CLI's
// own grant already does — and it happens even if the operator then decides
// NOT to authorize. Callers are expected to say so; the settings panel does.
//
// An untrusted package (see manifest.ErrUntrustedPackage) is reported with
// that sentinel on the error chain, so a caller can tell it apart from a
// package it merely could not obtain and refrain from offering a retry that
// could never succeed.
func (s *PluginConsentService) Resolve(ctx context.Context, name string) (server.PluginView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ... 与 Grant 的前半段同构：读部署 → FindEntry → resolvePluginPackageDir → LoadPackage
	// 然后把 pm 的声明填进一个 server.PluginView 并返回，不做任何写入。
}
```

实现时**照抄 `Grant` 前半段的错误包装方式**（哪个失败对应哪个 `server.Err*` 哨兵），保持两条路径的分类一致。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/cli/ -count=1 -timeout 300s -p 1
go test ./... -count=1 -timeout 900s
go test -race ./internal/cli/ -count=1 -timeout 600s -p 1
```

Windows 上多包同跑偶发 `rename ... Access is denied`（`.unpack-*` 临时目录被杀软/索引器碰），**是环境问题不是回归**，重跑即可。

- [ ] **Step 5: 变异验证（两个）**

a) 把 `Resolve` 里的 `s.mu.Lock()` 去掉 → `-race` 下的并发测试应报数据竞争或失败。
b) 让 `Resolve` 在成功后顺手写一次 `plugins.json`（比如调 `manifest.WriteDeployment`）→ 「不动清单」那条测试 MUST FAIL。

两个输出都留进报告，然后**还原**并 `git status` 核对。

- [ ] **Step 6: 提交**

```bash
git add internal/cli/plugin_consent_service.go internal/cli/plugin_consent_service_test.go
git commit -m "feat(plugin): fetch a declaration without authorizing anything"
```

---

### Task 3: `POST /v1/plugins/{name}/resolve`

**Files:**
- Modify: `internal/server/plugins.go`（接口、哨兵、状态码映射、handler）
- Modify: `internal/server/http.go`（路由 switch，311-314 行附近）
- Test: `internal/server/plugins_test.go`
- Modify: `internal/compat/testdata/openapi-agent.json`（golden，见 Step 5）

**Interfaces:**
- Consumes: `PluginConsentService.Resolve`（Task 2）、`manifest.ErrUntrustedPackage`（Task 1）
- Produces:
  - `PluginConsent` 接口新增 `Resolve(ctx context.Context, name string) (PluginView, error)`
  - `var ErrPluginUntrusted error`
  - `func (s *HTTPServer) handleResolvePlugin(w http.ResponseWriter, r *http.Request)`

**RBAC 用 `ActionWritePlugin`**，按**副作用**分类而非按「改没改授权」：它让服务端发出站请求、往磁盘写文件，只读角色不该能驱动这个。顺序与 `handleDenyPlugin` 完全一致：RBAC → `plugins == nil` → 解析名字 → 调用 → 响应。RBAC 在 nil 检查**之前**，顺带不向无权限者泄漏这个部署有没有开插件。

**不可信包给 422**：请求本身合法，但目标资源不可处理。GUI 靠这个状态码分岔到「无重试」的告警呈现。

- [ ] **Step 1: 写失败测试**

```go
func TestPluginsResolveFillsDeclarations(t *testing.T) {
	srv := newTestServerWithPlugins(t, &fakePluginConsent{
		resolveView: PluginView{
			Name: "weather", State: "unauthorized",
			DeclaredCaps: []string{"http"}, DeclaredUnresolved: false,
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/weather/resolve", nil)
	srv.ServeHTTP(rec, adminRequest(req))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got PluginView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeclaredUnresolved {
		t.Error("declared_unresolved = true after a successful resolve, want false")
	}
	if len(got.DeclaredCaps) != 1 || got.DeclaredCaps[0] != "http" {
		t.Errorf("declared_capabilities = %v, want [http]", got.DeclaredCaps)
	}
}

func TestPluginsResolveReportsAnUntrustedPackageAs422(t *testing.T) {
	srv := newTestServerWithPlugins(t, &fakePluginConsent{
		resolveErr: fmt.Errorf("resolve: %w", ErrPluginUntrusted),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/weather/resolve", nil)
	srv.ServeHTTP(rec, adminRequest(req))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for an untrusted package; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPluginsResolveRequiresPluginWritePermission(t *testing.T) {
	srv := newTestServerWithPlugins(t, &fakePluginConsent{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/weather/resolve", nil)
	srv.ServeHTTP(rec, viewerRequest(req)) // 只读角色

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a viewer", rec.Code)
	}
}

// RBAC 拒绝必须落审计——上一期发现代码写对了却没有测试钉住。
func TestPluginsResolveRBACDenialIsAudited(t *testing.T) {
	audit := &fakeAuditStore{}
	srv := newTestServerWithPluginsAndAudit(t, &fakePluginConsent{}, audit)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/weather/resolve", nil)
	srv.ServeHTTP(rec, viewerRequest(req))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !audit.hasAction("access_denied.rbac") {
		t.Error("RBAC denial on resolve was not audited")
	}
}

func TestPluginsResolveWithoutConsentServiceReports404(t *testing.T) {
	srv := newTestServerWithPlugins(t, nil) // 没装配 loader
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/weather/resolve", nil)
	srv.ServeHTTP(rec, adminRequest(req))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when plugins are not enabled", rec.Code)
	}
}
```

`fakePluginConsent` 是该测试文件既有的桩，加 `resolveView` / `resolveErr` 两个字段并实现新方法。助手名（`adminRequest` / `viewerRequest` / `fakeAuditStore` / `newTestServerWithPlugins`）以该文件现有的为准，先读再写。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./internal/server/ -run TestPluginsResolve -count=1 -timeout 300s -p 1
```

预期：编译失败（`ErrPluginUntrusted` 未定义、桩没实现 `Resolve`）。

- [ ] **Step 3: 实现**

`plugins.go` 加哨兵（挨着既有四个）：

```go
// ErrPluginUntrusted is what PluginConsent.Resolve reports when the package
// was obtained but is not trustworthy — unsigned, corruptly signed, or signed
// by a key this deployment does not trust. It is deliberately separate from
// the could-not-obtain classes: retrying an untrusted package can never make
// it trusted, so a caller must not offer that as a remedy.
var ErrPluginUntrusted = errors.New("plugin package is not trusted")
```

`pluginConsentStatus` 加一支（放在 default 之前）：

```go
	case errors.Is(err, ErrPluginUntrusted):
		return http.StatusUnprocessableEntity
```

`PluginConsent` 接口加方法，doc 说明它不改任何状态、以及不可信错误的契约。

handler 照 `handleDenyPlugin`（:270）的骨架写，仅把最后一步换成返回 `PluginView`：

```go
func (s *HTTPServer) handleResolvePlugin(w http.ResponseWriter, r *http.Request) {
	principal := security.PrincipalFromRequest(r)
	if !s.policy.Allows(principal, security.ActionWritePlugin, security.ResourcePlugin) {
		s.auditRBACDenied(r, principal, security.ResourcePlugin)
		writeError(w, http.StatusForbidden, "plugin access denied")
		return
	}
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "this process assembled no plugin loader; plugins are not enabled")
		return
	}
	name, ok := parsePluginConsentName(r.URL.Path, "/resolve")
	if !ok {
		writeError(w, http.StatusNotFound, "bad plugin resolve path")
		return
	}
	view, err := s.plugins.Resolve(r.Context(), name)
	if err != nil {
		writeError(w, pluginConsentStatus(err), fmt.Sprintf("resolve plugin %q: %v", name, err))
		return
	}
	writeJSON(w, http.StatusOK, view)
}
```

`http.go` 路由 switch 加一支（与 `/grant`、`/deny` 并列）：

```go
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/resolve"):
		s.handleResolvePlugin(rec, r)
```

**同时**：`PluginConsentService.Resolve`（Task 2）的不可信错误要包装到 `server.ErrPluginUntrusted` 上——在 Task 2 的实现里把 `manifest.ErrUntrustedPackage` 转成同时带两个哨兵的错误，两层各自 `errors.Is` 都能命中。若 Task 2 尚未这么做，本 task 补上并在报告里说明。

OpenAPI 文档里登记 `resolvePlugin`（照 `grantPlugin` / `denyPlugin` 的写法）。

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./internal/server/ -count=1 -timeout 300s -p 1
```

- [ ] **Step 5: 刷新 OpenAPI golden**

端点进了 OpenAPI 文档，golden 必须同步。**先看 diff 再提交**：

```bash
cp internal/compat/testdata/openapi-agent.json /tmp/golden-before.json
go test ./internal/compat/ -run TestOpenAPIGolden -update -count=1
diff /tmp/golden-before.json internal/compat/testdata/openapi-agent.json
```

预期：**零删除行**，新增的全是 `resolvePlugin` 那一段。删除行不为零就说明改动超出预期，停下来查。

```bash
go test ./... -count=1 -timeout 900s
go test -race ./internal/server/ ./internal/cli/ -count=1 -timeout 600s -p 1
```

- [ ] **Step 6: 变异验证（两个）**

a) 把 handler 里的 RBAC 检查删掉 → 403 与审计两个测试 MUST FAIL。
b) 把 `pluginConsentStatus` 的 `ErrPluginUntrusted` 那一支删掉（落到 default 的 400）→ 422 测试 MUST FAIL。

输出留进报告，**还原**，`git status` 核对。

- [ ] **Step 7: 提交**

```bash
git add internal/server/plugins.go internal/server/http.go internal/server/plugins_test.go internal/compat/testdata/openapi-agent.json
git commit -m "feat(plugin): expose the deliberate declaration fetch over HTTP"
```

---

### Task 4: GUI 绑定

**Files:**（GUI 仓 `F:\source\stardust\Legion\legion\legionAgentGUI`）
- Modify: `app_plugins.go`
- Test: `app_plugins_test.go`

**Interfaces:**
- Consumes: `POST /v1/plugins/{name}/resolve`（Task 3）
- Produces:
  - `func (a *App) ResolvePlugin(name string) (PluginDTO, error)`
  - `var errPluginUntrusted error`

**必须复用既有的 `pluginsPost`**——它已经带了 `requireServePort` 守卫（serve 未就绪时拒绝拨号端口 0）、每次调用现取 token 与 BaseURL（`ServeManager.Restart` 会换端口并重新铸 token）。绕开它就把这两件事都丢了。

**422 要能被前端认出来**：`pluginsPost` 在非 2xx 时返回一个带状态码的错误文本。本 task 加一个 `errPluginUntrusted` 哨兵，在状态码为 422 时挂上它，React 侧据此分岔到「无重试」的告警呈现。

- [ ] **Step 1: 写失败测试**

```go
func TestResolvePluginDecodesTheView(t *testing.T) {
	a := newFakeBackendApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/plugins/weather/resolve" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"weather","version":"1.0.0","state":"unauthorized","tools":[],"declared_capabilities":["http"],"declared_allowed_hosts":[],"declared_allowed_paths":[],"declared_unresolved":false,"granted_capabilities":[],"granted_allowed_hosts":[],"granted_allowed_paths":[]}`))
	})

	view, err := a.ResolvePlugin("weather")
	if err != nil {
		t.Fatalf("ResolvePlugin: %v", err)
	}
	if view.DeclaredUnresolved {
		t.Error("DeclaredUnresolved = true after a successful resolve, want false")
	}
	if len(view.DeclaredCapabilities) != 1 || view.DeclaredCapabilities[0] != "http" {
		t.Errorf("DeclaredCapabilities = %v, want [http]", view.DeclaredCapabilities)
	}
}

func TestResolvePluginMarksA422AsUntrusted(t *testing.T) {
	a := newFakeBackendApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"resolve plugin \"weather\": plugin package is not trusted"}`))
	})

	_, err := a.ResolvePlugin("weather")
	if err == nil {
		t.Fatal("ResolvePlugin on a 422 = nil error, want an untrusted-package error")
	}
	if !errors.Is(err, errPluginUntrusted) {
		t.Errorf("ResolvePlugin error = %v, want it to wrap errPluginUntrusted", err)
	}
}

func TestResolvePluginFailsLoudOnOtherNon2xx(t *testing.T) {
	a := newFakeBackendApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"resolve plugin \"weather\": disk on fire"}`))
	})

	_, err := a.ResolvePlugin("weather")
	if err == nil {
		t.Fatal("ResolvePlugin on a 500 = nil error, want an error")
	}
	if errors.Is(err, errPluginUntrusted) {
		t.Errorf("ResolvePlugin error = %v, want a 500 NOT classified as untrusted", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./... -count=1 -timeout 300s -run TestResolvePlugin
```

预期：编译失败 `a.ResolvePlugin undefined`。

- [ ] **Step 3: 实现**

`pluginsPost` 目前把非 2xx 拼成文本错误。给它一条按状态码挂哨兵的分支（或在 `ResolvePlugin` 内部按状态码判断——两种都行，取改动更小的那个，并在报告里说明选了哪个、为什么）：

```go
// errPluginUntrusted marks a 422 from the resolve endpoint: the package was
// obtained but is not trustworthy. The settings panel must not offer a retry
// for it — retrying cannot make an untrusted package trusted, and a control
// that can never work is the class of lie this panel exists to avoid.
var errPluginUntrusted = errors.New("插件包不被信任")

// ResolvePlugin fetches and verifies one plugin's package so the panel can
// show what it declares, WITHOUT authorizing anything. …
func (a *App) ResolvePlugin(name string) (PluginDTO, error) { … }
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./... -count=1 -timeout 300s
```

然后重新生成前端绑定：

```bash
wails generate module
```

- [ ] **Step 5: 变异验证**

把 422 的哨兵分支去掉 → `TestResolvePluginMarksA422AsUntrusted` MUST FAIL。输出留进报告，还原，`git status` 核对。

- [ ] **Step 6: 提交**

```bash
git add app_plugins.go app_plugins_test.go frontend/wailsjs
git commit -m "feat(plugin): bind the declaration fetch for the settings panel"
```

---

### Task 5: 面板按钮与两类失败呈现

**Files:**（GUI 仓）
- Modify: `frontend/src/components/settings/PluginsPage.tsx`
- Test: `frontend/src/components/settings/PluginsPage.test.tsx`

**Interfaces:**
- Consumes: `ResolvePlugin`（Task 4）、`beginPluginConsent` / `endPluginConsent`（`frontend/src/stores/pluginConsentStore.ts`，既有）

**四条规则，逐条要有测试：**

1. **`declared_unresolved` 为真的行显示「取回声明」按钮**，次要样式（取回不是目的，授权才是）；「授权」保持禁用不变——那条规则是对的，不动它。
2. **取回中必须走 `beginPluginConsent` / `endPluginConsent`。** 这一步会真的下载；不登记的话 `SettingsModal` 的 Escape / 标题栏 X / 背景点击三道关卡看不见它，运维一按 Esc 就能关掉窗口而服务端还在下载。上一期终审逐扇门堵过这个洞，**新入口不能重新打开它**。
3. **成功后**那一行原地换成带声明的状态（`declared_*` 填满、「授权」解禁），并**常驻**一句说明：`已取回并缓存该插件包（未授权，可随时撤销）`。常驻而非几秒即逝，因为这个事实持续为真。
4. **失败分两条岔路**：不可信（`errPluginUntrusted`）→ 醒目告警、**没有重试按钮**、文案点明重试无用需联系包的提供方；其余 → 普通错误 + 重试按钮。

- [ ] **Step 1: 写失败测试**

```tsx
it('offers 取回声明 for an unresolved row and keeps 授权 disabled', async () => {
  mocks.ListPlugins.mockResolvedValue([
    makePlugin({ name: 'plugin-u', state: 'unauthorized', declared_unresolved: true }),
  ])
  render(<PluginsPage />)
  const row = await screen.findByRole('group', { name: '插件 plugin-u' })
  expect(within(row).getByRole('button', { name: '取回声明' })).toBeInTheDocument()
  expect(within(row).getByRole('button', { name: '授权' })).toBeDisabled()
})

it('registers the fetch as in-flight so the modal close guards can see it', async () => {
  mocks.ListPlugins.mockResolvedValue([
    makePlugin({ name: 'plugin-i', state: 'unauthorized', declared_unresolved: true }),
  ])
  mocks.ResolvePlugin.mockReturnValue(new Promise(() => {})) // 永不 resolve
  render(<PluginsPage />)
  const row = await screen.findByRole('group', { name: '插件 plugin-i' })
  fireEvent.click(within(row).getByRole('button', { name: '取回声明' }))

  await waitFor(() => {
    expect(usePluginConsentStore.getState().inFlight).toBeGreaterThan(0)
  })
})

it('shows the declaration and a persistent cache note after a successful fetch', async () => {
  mocks.ListPlugins.mockResolvedValue([
    makePlugin({ name: 'plugin-r', state: 'unauthorized', declared_unresolved: true }),
  ])
  mocks.ResolvePlugin.mockResolvedValue(
    main.PluginDTO.createFrom({
      name: 'plugin-r', state: 'unauthorized',
      declared_capabilities: ['http'], declared_allowed_hosts: [], declared_allowed_paths: [],
      declared_unresolved: false,
      granted_capabilities: [], granted_allowed_hosts: [], granted_allowed_paths: [],
      tools: [],
    }),
  )
  render(<PluginsPage />)
  const row = await screen.findByRole('group', { name: '插件 plugin-r' })
  fireEvent.click(within(row).getByRole('button', { name: '取回声明' }))

  const updated = await screen.findByRole('group', { name: '插件 plugin-r' })
  expect(within(updated).getByText(/已取回并缓存该插件包/)).toBeInTheDocument()
  expect(within(updated).getByRole('button', { name: '授权' })).not.toBeDisabled()
})

it('offers no retry when the package is untrusted', async () => {
  mocks.ListPlugins.mockResolvedValue([
    makePlugin({ name: 'plugin-x', state: 'unauthorized', declared_unresolved: true }),
  ])
  mocks.ResolvePlugin.mockRejectedValue(new Error('插件包不被信任'))
  render(<PluginsPage />)
  const row = await screen.findByRole('group', { name: '插件 plugin-x' })
  fireEvent.click(within(row).getByRole('button', { name: '取回声明' }))

  const updated = await screen.findByRole('group', { name: '插件 plugin-x' })
  expect(within(updated).queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
  expect(within(updated).getByText(/不被信任/)).toBeInTheDocument()
})

it('offers a retry when the fetch merely failed', async () => {
  mocks.ListPlugins.mockResolvedValue([
    makePlugin({ name: 'plugin-n', state: 'unauthorized', declared_unresolved: true }),
  ])
  mocks.ResolvePlugin.mockRejectedValue(new Error('resolve plugin "plugin-n": dial tcp: connection refused'))
  render(<PluginsPage />)
  const row = await screen.findByRole('group', { name: '插件 plugin-n' })
  fireEvent.click(within(row).getByRole('button', { name: '取回声明' }))

  const updated = await screen.findByRole('group', { name: '插件 plugin-n' })
  expect(within(updated).getByRole('button', { name: '重试' })).toBeInTheDocument()
})
```

前端如何识别「不可信」：Go 侧的错误经 Wails 传到 JS 只剩消息字符串，所以**约定以 `errPluginUntrusted` 的消息文本 `插件包不被信任` 作为判据**，并在代码注释里写明这条约定与它的脆弱性（改 Go 侧文案就会破坏它）。实现时若 Wails 有更结构化的错误通道，优先用它并在报告里说明。

- [ ] **Step 2: 跑测试确认失败**

```bash
cd frontend && npx vitest run --pool=forks --poolOptions.forks.singleFork=true src/components/settings/PluginsPage.test.tsx
```

预期：新用例 FAIL（找不到「取回声明」按钮）。

- [ ] **Step 3: 实现**

按四条规则改 `PluginsPage.tsx`。取回的 in-flight 登记照 `retryConvergence` 现有的写法（`beginPluginConsent()` … `finally { endPluginConsent() }`）。

- [ ] **Step 4: 跑测试确认通过**

```bash
cd frontend && npx vitest run --pool=forks --poolOptions.forks.singleFork=true
```

预期：**文件数 ≥ 49**，测试数 ≥ 271 + 5。文件数掉了就是 worker 崩了带走了文件，重跑。

```bash
cd .. && go build ./... && go vet ./... && gofmt -l .
```

- [ ] **Step 5: 变异验证（两个）**

a) 去掉取回路径的 `beginPluginConsent()` → in-flight 那条测试 MUST FAIL。
b) 让不可信分支也渲染重试按钮 → 「无重试」那条测试 MUST FAIL。

输出留进报告，**还原**，`git status` 核对。

- [ ] **Step 6: 提交**

```bash
git add frontend/src/components/settings/PluginsPage.tsx frontend/src/components/settings/PluginsPage.test.tsx
git commit -m "feat(plugin): let an operator fetch a declaration before deciding"
```

---

## 交付后应当为真

- 一条未缓存的远程插件，在 GUI 里能取回声明、看清它要什么、然后授权——不必退回 CLI。
- 取回**不写** `plugins.json`；运维可以只看不授。
- 取回会落缓存，且**界面明说了这件事**。
- 签名不可信的包给出醒目告警且**没有重试按钮**；拿不到的包给普通错误 + 重试。
- 取回期间 `SettingsModal` 的三道关卡照常拦得住。

## 明确不做

- 批量「取回全部」——一次点击引发 N 个出站下载，值得单独想清楚。
- 缓存清理与容量上限（既有待办）。
- 不改 `unresolvedBlocked` 规则本身，只给它出路。
- 「取回后反悔、清掉缓存」的入口——加它等于开始做缓存生命周期管理。

## 真机验证

五个 task 全绿后，按上一期的方式做一次真机走查（`agent serve` 真进程 + 真 wasm 夹具 + GUI）：**清空缓存**造出未解析状态 → 面板点「取回声明」→ 看声明出现、缓存说明常驻 → 授权。再把包的签名改坏，验证告警呈现且无重试按钮。上一期三个缺陷全部是真机才暴露的，这一步不能省。
