# GUI 插件授权同意流 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 A5c 的 `unauthorized` / `disabled` / `authorized` 三态搬上图形界面，让插件授权与撤销在 GUI 里点一下就当场生效。

**Architecture:** server 仓新建 `internal/plugin/consent` 共享包，把能力校验、allowlist 规则、并发守卫从 `internal/cli` 提取出来，CLI 与新的 HTTP 端点共用同一份实现；`internal/server` 经**接口**消费一个由 serve 装配注入的插件服务（不 import `internal/plugin/loader`，守住分层）；GUI 用已有的 `ServeResult.Token` + `BaseURL` 调这三个端点，前端只做 UI。

**Tech Stack:** Go 1.26.0（module `github.com/stardust/legion-agent`）；GUI 是 Wails + React + TypeScript（module 在 `../legionAgentGUI`，经 `replace` 依赖 server 模块）。**不新增任何第三方依赖**。

**Spec:** `docs/superpowers/specs/2026-08-25-gui-plugin-consent-design.md`

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常；错误用 `fmt.Errorf("<动作> <标识>: %w", err)` 包装；不变量违反用 `panic`。
- 公开 API 必须有 Go doc 风格注释，以标识符名开头，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿，`gofmt -l .` 输出为空。
- 涉及并发的任务额外跑 `go test -race`，**plugin / runtime / cli / server 包串行跑**（`-p 1`）。
- 错误路径必须有测试断言「确实返回 error」，不得只测 happy path。
- 每个 task 完成后做一次**变异验证**：把该 task 的核心机制改坏，确认测试确实 FAIL，把失败输出留在报告里，然后还原。
- 所有测试用 `httptest.Server` 或进程内 handler，**不得触真实网络**。
- **两个独立 git 仓库**：`legionAgent`（server）与 `legionAgentGUI`（GUI，remote `stardust-agent-gui`）。各自提交，绝不 `git add -A`——两个树里都有无关的运行时产物。
- **GUI 的 vitest 必须在 `frontend/` 目录里跑**。父目录另有一个没有 jsdom 的 v4，在那里跑会 `document is not defined` 假失败。

## 三条已定的裁决（实施时不再讨论）

1. **共享校验是纯搬移，不趁机改语义。** 判据：搬完之后 `internal/cli` 的既有插件测试**一条都不改**且全绿。这与 A4b 把 `TaskGate` 下沉到 `internal/taskgate` 时用的是同一个判据。
2. **`internal/server` 不 import `internal/plugin/loader`。** 经接口消费，实现由 serve 装配注入——`HTTPServer` 现有的每一项依赖（`TaskStore`、`AgentCatalog`、`SkillManager`…）都是这么做的。
3. **不新增状态机。** `pending_convergence` 只是 grant/deny 端点**单次调用**的即时结果字段，不是条目的持久状态。收敛超时后再 `GET /v1/plugins`，该条目落在 `mergePluginStatus` 既有的 `pending` 上。

## ⚠️ 为什么共享校验必须提取（本期最容易被做错的一步）

A5c 全期反复抓到同一类缺陷：**两条路径验证同一个概念，然后分道扬镳**。`install` 与 `grant` 在重复能力名、allowlist 规则、并发守卫上**各错过一次**，每一次都是「一条路补了、另一条没补」，而且两条命令的输出里都看不出矛盾。

HTTP 端点将是验证这套规则的**第三条路径**，也是 `plugins.json` 的**第四个 writer**。端点自己重写一遍校验，就是明知故犯地再造一次同样的分歧。

**提取时必须分层。** 现有函数接受的是 flag **字符串**：

```go
func resolveGrantCapabilities(capabilitiesFlag string, pm manifest.PluginManifest) ([]string, error)
```

它内部第一步就是 `splitFlagList("plugins grant", "capabilities", capabilitiesFlag)`。而 HTTP 端点拿到的是**已经解析好的数组**。原样搬移这个签名，会逼端点把数组拼成逗号串再让对方拆开——荒谬，而且名字里含逗号时会静默出错。

所以：**`splitFlagList` 之后的校验部分抽成接受 `[]string` 的函数**，CLI 保留自己那层「flag 字符串 → `[]string`」的转换。

## ⚠️ 四种结果，不是三种（顺序决定的）

端点**先写盘、再触发收敛**，所以除「写盘失败」外**磁盘都已经改了**：

| # | 情况 | 端点返回 |
|---|---|---|
| 1 | 写盘失败 | `4xx/5xx`，磁盘一字未变 |
| 2 | 写盘成功 + 收敛成功 | `200`，`state` 取自 loader（通常 `loaded`） |
| 3 | 写盘成功 + **收敛没发生**（等待超时／被取消／另一个 apply 正在跑） | `200`，`pending_convergence: true` |
| 4 | 写盘成功 + 收敛发生了但**这一条目激活失败** | `200`，`state: failed` + loader 的失败原因 |

判据来自 `internal/taskgate/taskgate.go:327` —— 超时路径 wrap 的是 `waitCtx.Err()`，消息里明写 "nothing was applied"。

**把第 4 种误报成第 3 种，会让人一直等一个永远不会来的收敛。** 第 3 种误报成成功，则是 A5c 全期最痛恨的「报告成功但其实没生效」，而且这次是在安全边界上。

## ⚠️ Fork-bomb 安全规程（本仓已因此宕机两次）

`host.test.exe` 曾吃到 **170 GB 虚存**（Windows 事件 2004 + Kernel-Power 41），原因是测试把被测功能当唯一终止条件。

1. 任何循环边界必须独立于被测功能：轮数写成字面量（≤5），每轮断言实例数上限。
2. 每次 `go test` 带 `-timeout 120s`；plugin / runtime / cli / server 包用 `-p 1`。
3. 绝不把变异留在工作区，每次还原后用 `git status` 核对。

## 前置事实（已在 master `f22c0be`，直接用）

```go
// internal/cli/plugins_command.go —— 本期要提取的对象
func readPluginDeploymentWithSnapshot(path string) (manifest.Deployment, []byte, error)   // :626
func refusePluginDeploymentChanged(cmdContext, manifestPath string, snapshot []byte) error // :656
func refuseUnnamedAllowlist(cmdContext, flagLabel string, grants []string,
    pm manifest.PluginManifest, hosts, paths []string, hostsRemedy, pathsRemedy string) error // :1610
func resolveGrantCapabilities(capabilitiesFlag string, pm manifest.PluginManifest) ([]string, error) // :1837
func resolveGrantAllowedHosts(allowedHostsFlag string, pm manifest.PluginManifest) ([]string, error) // :1891
func resolveGrantAllowedPaths(allowedPathsFlag string, pm manifest.PluginManifest) ([]string, error) // :1917
func splitFlagList(cmdContext, flagName, flagValue string) ([]string, error)   // :1945 —— 留在 CLI，不搬
func findPluginEntry(dep manifest.Deployment, name string) (manifest.Entry, error) // :1973

// internal/plugin/manifest
func (e Entry) IsRemote() bool
func ParseDeployment(data []byte) (Deployment, error)
func UpdateEntry(dep Deployment, name string, mutate func(Entry) (Entry, error)) (Deployment, error)
func WriteDeployment(path string, dep Deployment) error
func LoadPackage(dir string, keyring *sign.Keyring) (PluginManifest, []byte, error)

// internal/plugin/loader
func (l *Loader) Apply(ctx context.Context, dep manifest.Deployment, root string) error // :767
func (l *Loader) Status() []InstanceStatus                                              // :922

// internal/server/http.go
type HTTPServer struct { /* 全部依赖都是接口：tasks TaskStore、agents AgentCatalog、skills SkillManager … */ } // :158
func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request)   // :245，路由 switch 在 :290-345
func (s *HTTPServer) authorized(r *http.Request) bool                    // :347
```

---

### Task 1: 提取共享校验包（纯搬移）

**Files:**
- Create: `internal/plugin/consent/consent.go`
- Create: `internal/plugin/consent/consent_test.go`
- Modify: `internal/cli/plugins_command.go`

**Interfaces:**
- Produces:
  - `func ResolveCapabilities(actor string, granted []string, pm manifest.PluginManifest) ([]string, error)`
  - `func ResolveAllowedHosts(actor string, granted []string, pm manifest.PluginManifest) ([]string, error)`
  - `func ResolveAllowedPaths(actor string, granted []string, pm manifest.PluginManifest) ([]string, error)`
  - `func RefuseUnnamedAllowlist(actor, subject string, capabilities []string, pm manifest.PluginManifest, hosts, paths []string, hostsRemedy, pathsRemedy string) error`
  - `func ReadDeploymentWithSnapshot(path string) (manifest.Deployment, []byte, error)`
  - `func RefuseDeploymentChanged(actor, manifestPath string, snapshot []byte) error`
  - `func FindEntry(dep manifest.Deployment, name string) (manifest.Entry, error)`

`actor` 取代原来的 `cmdContext`：CLI 传 `"plugins grant"`，端点传 `"POST /v1/plugins/{name}/grant"`。错误消息里那个位置原本就是给人看「是谁拒绝了我」的。

`subject` 取代 `flagLabel`：CLI 传 `"--capabilities"`，端点传 `"capabilities"`。

**这是纯搬移。** 校验逻辑一行不改，只把 `splitFlagList` 的调用留在 CLI 侧。

- [ ] **Step 1: 建包并搬入不带 flag 解析的三个**

`ReadDeploymentWithSnapshot`、`RefuseDeploymentChanged`、`FindEntry` 的函数体原样搬入 `consent.go`，只改名（首字母大写）与 `cmdContext` → `actor`。`RefuseUnnamedAllowlist` 同样原样搬入（它本来就接受 `[]string`，是仓内已经可复用的形态）。

- [ ] **Step 2: 搬入三个 resolve，剥掉 flag 解析**

原 `resolveGrantCapabilities` 的第一句是 `splitFlagList(...)`。新版直接接受 `granted []string`：

```go
// ResolveCapabilities checks a proposed capability grant against what the
// plugin itself declares, and returns the set to record.
//
// The two sets must be EQUAL, not merely compatible. manifest.reconcileCapabilities
// (assemble.go) refuses any entry whose grant does not cover every declared
// capability, so a strict subset produces an entry the deployment can never
// load; extras are ignored there anyway. A plugin's declaration is not a menu.
//
// actor names whoever is asking, for the error message ("plugins grant",
// "POST /v1/plugins/{name}/grant"). subject names the input that carried the
// list ("--capabilities", "capabilities").
func ResolveCapabilities(actor string, granted []string, pm manifest.PluginManifest) ([]string, error) {
	for _, capability := range granted {
		if !slices.Contains(pm.Capabilities, capability) {
			return nil, fmt.Errorf("%s: names capability %q, which plugin %q does not declare in "+
				"plugin.json (it declares: %v); granting a capability the plugin did not ask for is "+
				"a config error, not generosity", actor, capability, pm.Name, pm.Capabilities)
		}
	}
	var missing []string
	for _, declared := range pm.Capabilities {
		if !slices.Contains(granted, declared) {
			missing = append(missing, declared)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: %v does not grant %v, which plugin %q declares in plugin.json; "+
			"a partial grant produces an entry the deployment can never load (every declared "+
			"capability must be granted, not a subset) -- name the complete list, or deny it "+
			"instead of half-authorizing it", actor, granted, missing, pm.Name)
		}
	return granted, nil
}
```

`ResolveAllowedHosts` / `ResolveAllowedPaths` 同法：保留原有的大小写不敏感主机比较与「未声明即拒绝」方向，**不要**给它们加上 set-equality（原 doc 注释里详细解释了为什么 hosts 是交集语义、允许严格子集——把那段注释一并搬过去）。

- [ ] **Step 3: CLI 改调新包**

`internal/cli/plugins_command.go` 里三个 `resolveGrant*` 函数**改成薄包装**，其余调用点不动：

```go
func resolveGrantCapabilities(capabilitiesFlag string, pm manifest.PluginManifest) ([]string, error) {
	capabilities, err := splitFlagList("plugins grant", "capabilities", capabilitiesFlag)
	if err != nil {
		return nil, err
	}
	return consent.ResolveCapabilities("plugins grant", capabilities, pm)
}
```

`refuseUnnamedAllowlist` / `readPluginDeploymentWithSnapshot` / `refusePluginDeploymentChanged` / `findPluginEntry` 的**函数体删掉**，改为直接调 `consent.` 对应函数（或直接把调用点换成 `consent.X`，二选一，以 diff 更小者为准）。

- [ ] **Step 4: 回归护体——CLI 测试一条不改**

Run: `go test ./internal/cli/ -count=1 -timeout 300s -p 1`
Expected: PASS，且 `git diff --stat internal/cli/plugins_command_test.go` **输出为空**。

**测试文件有任何改动，就说明这不是纯搬移。** 停下来查清楚哪里改了语义，而不是改测试迁就。

- [ ] **Step 5: 给新包补自己的测试**

`consent_test.go` 覆盖：能力严格子集被拒且错误点名缺的那个、能力含未声明项被拒且点名、hosts 严格子集被接受、hosts 含未声明项被拒、hosts 大小写不敏感、授 `http` 但 hosts 为空被拒（声明非空时）、声明为空时不拒、`RefuseDeploymentChanged` 在字节变化时报错。

每条断言错误消息里出现了 `actor` 传入的值——这是端点能报出自己名字的凭据。

- [ ] **Step 6: 全量验证**

Run: `go build ./... && go vet ./... && go test ./... -count=1 -timeout 900s && gofmt -l .`
Expected: 全绿，`gofmt -l .` 无输出。

- [ ] **Step 7: 变异验证**

把 `ResolveCapabilities` 的 missing 检查删掉 → CLI 的 `TestPluginsGrantRefusesAPartialGrantMissingADeclaredCapability` **与** consent 包自己的子集测试都必须 FAIL。粘贴失败输出，还原，`git status` 核对。

**两处都红才算数**：只有新包红说明 CLI 没真的改调过去。

- [ ] **Step 8: 提交**

```bash
git add internal/plugin/consent/consent.go internal/plugin/consent/consent_test.go internal/cli/plugins_command.go
git commit -m "refactor(plugin): extract grant validation so a second caller cannot diverge"
```

---

### Task 2: `GET /v1/plugins`

**Files:**
- Create: `internal/server/plugins.go`
- Create: `internal/server/plugins_test.go`
- Create: `internal/cli/plugin_consent_service.go`
- Create: `internal/cli/plugin_consent_service_test.go`
- Modify: `internal/server/http.go`（`HTTPServer` 加字段、`Config` 加字段、路由 switch 加 case）
- Modify: `internal/cli/command.go`（serve 装配注入实现）

**Interfaces:**
- Consumes: Task 1 的 `consent.ReadDeploymentWithSnapshot`、`consent.FindEntry`
- Produces:
  - `type PluginConsent interface { List(ctx context.Context) ([]PluginView, error) }`（Task 3 会往这个接口上加 `Grant` / `Deny`）
  - `type PluginView struct { … }`
  - `func NewPluginConsentService(manifestPath, root string, pluginsFn func() *loader.Loader, keyringFn func() *sign.Keyring) *PluginConsentService`

- [ ] **Step 1: 在 server 包定义接口与 DTO（消费方定义接口，Go 惯例）**

`internal/server/plugins.go`：

```go
// PluginConsent is the plugin-authorization surface the HTTP layer consumes.
//
// It is an interface so internal/server never imports internal/plugin/loader:
// every other HTTPServer dependency (TaskStore, AgentCatalog, SkillManager…)
// follows the same rule, and serve assembly injects the implementation.
type PluginConsent interface {
	List(ctx context.Context) ([]PluginView, error)
}

// PluginView is one deployment entry as the consent UI needs to see it.
//
// Declared and Granted are separate on purpose: the consent dialog renders the
// checklist from what the plugin DECLARES, and marks current state from what is
// already GRANTED. Collapsing them would make "this plugin wants http" and
// "http is authorized" indistinguishable.
type PluginView struct {
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	State            string   `json:"state"`
	Detail           string   `json:"detail,omitempty"`
	Tools            []string `json:"tools"`
	DeclaredCaps     []string `json:"declared_capabilities"`
	DeclaredHosts    []string `json:"declared_allowed_hosts"`
	DeclaredPaths    []string `json:"declared_allowed_paths"`
	GrantedCaps      []string `json:"granted_capabilities"`
	GrantedHosts     []string `json:"granted_allowed_hosts"`
	GrantedPaths     []string `json:"granted_allowed_paths"`
}
```

- [ ] **Step 2: 写失败测试（handler 层）**

`internal/server/plugins_test.go`：用一个假的 `PluginConsent` 实现，断言 `GET /v1/plugins` 返回 200 且 body 里 declared 与 granted 分开；断言未鉴权时被 `authorized` 挡下；断言 `List` 报错时返回 5xx 且 body 里有原因而不是空对象。

Run: `go test ./internal/server/ -run TestPlugins -count=1 -timeout 120s -p 1`
Expected: FAIL，`handleListPlugins` 未定义。

- [ ] **Step 3: 实现 handler 与路由**

`plugins.go` 加：

```go
func (s *HTTPServer) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusNotFound, "this process assembled no plugin loader; plugins are not enabled")
		return
	}
	views, err := s.plugins.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list plugins: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": views})
}
```

（`writeJSON` 在 `http.go:1719`、`writeError` 在 `http.go:1742`，签名如上直接用，不要新造。）

`http.go` 的 `HTTPServer` 加 `plugins PluginConsent` 字段、`Config` 加同名字段并在构造函数里赋值，路由 switch 加：

```go
case r.Method == http.MethodGet && r.URL.Path == "/v1/plugins":
	s.handleListPlugins(rec, r)
```

- [ ] **Step 4: 在 cli 侧实现服务**

`internal/cli/plugin_consent_service.go`：`List` 读清单（`consent.ReadDeploymentWithSnapshot`）、取 loader 的 `Status()`，按 `mergePluginStatus` 同一套规则合并出 state，并对每条目 `LoadPackage` 取声明（远程条目缓存命中时不联网）。

**state 取值必须与 `plugins status` 完全一致**，不新增：`unauthorized` / `disabled` / `loaded` / `failed` / `suspended` / `pending`。若 `mergePluginStatus` 可直接复用就复用；不能则提取共用，**绝不写第二套判据**——那正是本期在防的事。

- [ ] **Step 5: serve 装配注入**

`internal/cli/command.go` 的 `BuildServeService` 里，在插件 loader 装配完成之后，把 `NewPluginConsentService(...)` 塞进 server 的 `Config`。**没有 loader 时注入 nil**，handler 会据此回 404 说明插件未启用（不是空列表——空列表会被读成「没装插件」，而实情是这个部署根本没开插件）。

- [ ] **Step 6: 跑测试**

Run: `go test ./internal/server/ ./internal/cli/ -count=1 -timeout 300s -p 1`
Expected: PASS

- [ ] **Step 7: 变异验证**

把 `PluginView` 的 `DeclaredCaps` 与 `GrantedCaps` 填成同一个值 → Step 2 里「declared 与 granted 分开」那条测试必须 FAIL。粘贴输出，还原，`git status` 核对。

- [ ] **Step 8: 提交**

```bash
git add internal/server/plugins.go internal/server/plugins_test.go internal/server/http.go internal/cli/plugin_consent_service.go internal/cli/plugin_consent_service_test.go internal/cli/command.go
git commit -m "feat(plugin): expose the deployment and its declarations over HTTP"
```

---

### Task 3: `grant` / `deny` 端点与四种结果

**Files:**
- Modify: `internal/server/plugins.go`
- Modify: `internal/server/plugins_test.go`
- Modify: `internal/server/http.go`（两个路由 case）
- Modify: `internal/cli/plugin_consent_service.go`
- Modify: `internal/cli/plugin_consent_service_test.go`

**Interfaces:**
- Consumes: Task 1 的全部 `consent.*`；Task 2 的 `PluginConsent`、`PluginView`
- Produces:
  - 接口扩为 `Grant(ctx context.Context, name string, req GrantRequest) (ConsentResult, error)` 与 `Deny(ctx context.Context, name string) (ConsentResult, error)`
  - `type GrantRequest struct { Capabilities, AllowedHosts, AllowedPaths []string }`
  - `type ConsentResult struct { View PluginView; PendingConvergence bool; ConvergenceDetail string }`

- [ ] **Step 1: 写失败测试——四种结果各一条**

`plugins_test.go` 用假实现分别返回四种情况，断言：

1. 写盘失败 → 4xx/5xx，且测试另行断言磁盘文件字节未变
2. 收敛成功 → 200，`pending_convergence` 为 false，`state` 是 loader 给的
3. 收敛没发生 → **200**，`pending_convergence` 为 **true**，`convergence_detail` 非空
4. 该条目激活失败 → 200，`pending_convergence` 为 **false**，`state` 为 `failed`，detail 带原因

第 3 与第 4 必须是两条独立测试，且断言 `pending_convergence` 取值相反。**这是本 task 的核心不变量**：把第 4 种报成 `pending`，会让人一直等一个永远不会来的收敛。

Run: `go test ./internal/server/ -run TestPluginsGrant -count=1 -timeout 120s -p 1`
Expected: FAIL

- [ ] **Step 2: 实现 handler**

解析 body、调接口、按 `ConsentResult` 写响应。**校验一律交给 cli 侧的实现（它调 `consent.*`），handler 不做任何能力/allowlist 判断**——handler 里出现第二套判断，就是本期要防的第三条路径。

- [ ] **Step 3: 实现 Grant / Deny 服务方法**

顺序不可颠倒，写进方法的 doc：

```
1. consent.ReadDeploymentWithSnapshot  取目标态与快照字节
2. consent.FindEntry                   定位条目（找不到 → 错误，磁盘未变）
3. LoadPackage                         取插件自己的声明
4. consent.ResolveCapabilities / ResolveAllowedHosts / ResolveAllowedPaths / RefuseUnnamedAllowlist
                                       校验（任一失败 → 错误，磁盘未变）
5. consent.RefuseDeploymentChanged     并发守卫：快照与当前字节不一致就拒绝
6. manifest.UpdateEntry + WriteDeployment   写盘
7. loader.Apply                        触发收敛
```

`Deny` 走同一条路，跳过 3 与 4，且 `UpdateEntry` 里置 `Enabled=false`、清空 `Grant`、**保留 `GrantStated=true`**（决定做过了），`Source`/`Digest`/`Tools` 一字不动。

第 7 步的错误分流：

```go
if err := ld.Apply(ctx, dep, root); err != nil {
	// The disk write in step 6 already succeeded, so this is never "nothing
	// happened" — it is either "not applied yet" or "applied and this entry
	// failed". taskgate's timeout path wraps waitCtx.Err() and says
	// "nothing was applied" (taskgate.go:327); a cancelled parent ctx and a
	// second in-flight apply are the same shape. Everything else means
	// convergence DID run, so the entry's own state is the honest answer.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, taskgate.ErrApplyPending) {
		return ConsentResult{View: view, PendingConvergence: true, ConvergenceDetail: err.Error()}, nil
	}
	return ConsentResult{}, fmt.Errorf("apply plugin grant for %q: %w", name, err)
}
```

收敛跑完后**重新读一次 loader 的 `Status()`** 填 `View`——第 4 种情况的 `failed` 与原因只能从那里拿到，不能靠推断。

（`errApplyInProgress` 是 taskgate 的未导出变量。若 `errors.Is` 够不到它，本 task 顺带把它导出为 `ErrApplyInProgress` 并补一行 doc；这属于必要的可观测性修补，不算越界。）

- [ ] **Step 4: 路由**

```go
case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/grant"):
	s.handleGrantPlugin(rec, r)
case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/plugins/") && strings.HasSuffix(r.URL.Path, "/deny"):
	s.handleDenyPlugin(rec, r)
```

放在 `GET /v1/plugins` 那条**之后**，与 switch 里既有的 `/v1/sessions/...` 前后缀匹配写法保持一致。

- [ ] **Step 5: cli 侧测试**

覆盖：能力严格子集被拒且磁盘字节未变、授 `http` 但 hosts 为空被拒（声明非空时）、并发编辑（写盘前改动磁盘）被拒、`deny` 后 `Source`/`Digest`/`Tools` 逐字段未变且 `GrantStated` 仍为 true、`deny` 后可再 `Grant` 回来。

- [ ] **Step 6: 全量验证**

Run: `go build ./... && go vet ./... && go test ./... -count=1 -timeout 900s && gofmt -l .`
再跑 `go test -race ./internal/plugin/... ./internal/cli/ ./internal/server/ -count=1 -timeout 900s -p 1`
Expected: 全绿

- [ ] **Step 7: 变异验证（两个）**

a) 把 `PendingConvergence` 恒置为 false → 第 3 种的测试必须 FAIL
b) 把并发守卫那步删掉 → Step 5 的并发编辑测试必须 FAIL
粘贴两份失败输出，全部还原，`git status` 核对。

- [ ] **Step 8: 提交**

```bash
git add internal/server/plugins.go internal/server/plugins_test.go internal/server/http.go internal/cli/plugin_consent_service.go internal/cli/plugin_consent_service_test.go
git commit -m "feat(plugin): authorize and revoke over HTTP without lying about convergence"
```

---

### Task 4: GUI 绑定

**Files（GUI 仓 `F:\source\stardust\Legion\legion\legionAgentGUI`）:**
- Create: `app_plugins.go`
- Create: `app_plugins_test.go`

**Interfaces:**
- Consumes: Task 2/3 的三个端点
- Produces（前端可见的 Wails 绑定）：
  - `func (a *App) ListPlugins() ([]PluginDTO, error)`
  - `func (a *App) GrantPlugin(name string, capabilities, allowedHosts, allowedPaths []string) (ConsentResultDTO, error)`
  - `func (a *App) DenyPlugin(name string) (ConsentResultDTO, error)`

- [ ] **Step 1: 先读既有先例**

读 `app_agents.go` 与 `sse_bridge.go`，照它们的方式取 `ServeResult` 的 Token 与 BaseURL 并发 HTTP。**不要新造一套取地址/取 token 的方式。** 前端不直接 fetch——走 Wails 绑定到 Go、Go 再发 HTTP，这是仓内既定架构（避 CORS）。

- [ ] **Step 2: 写失败测试**

`app_plugins_test.go` 用 `httptest.Server` 伪装端点，断言：正常响应被正确解码；端点回 4xx 时**返回 error 而不是空切片**（fail-loud）；`pending_convergence` 为 true 时 DTO 里如实保留。

Run: `go test ./... -run TestPlugins -count=1 -timeout 120s`（在 GUI 仓根目录）
Expected: FAIL

- [ ] **Step 3: 实现三个绑定**

DTO 字段与 server 的 JSON 一一对应。**`pending_convergence` 必须原样传到前端**，不得在这一层被吞掉或翻译成布尔成功——那会让 UI 无法区分「已生效」与「已授权待收敛」。

- [ ] **Step 4: 跑测试并生成绑定**

Run: `go test ./... -count=1 -timeout 300s`
然后 `wails generate module` 刷新前端的 TypeScript 绑定（`frontend/wailsjs/`）。

- [ ] **Step 5: 变异验证**

把端点 4xx 的分支改成返回空切片加 nil error → Step 2 的 fail-loud 测试必须 FAIL。粘贴输出，还原。

- [ ] **Step 6: 提交（GUI 仓）**

```bash
git add app_plugins.go app_plugins_test.go frontend/wailsjs
git commit -m "feat(plugin): bind the plugin consent endpoints for the settings UI"
```

---

### Task 5: 插件面板与同意对话框

**Files（GUI 仓）:**
- Create: `frontend/src/components/settings/PluginsPage.tsx`
- Create: `frontend/src/components/settings/PluginsPage.test.tsx`
- Create: `frontend/src/components/settings/PluginConsentDialog.tsx`
- Create: `frontend/src/components/settings/PluginConsentDialog.test.tsx`
- Modify: `frontend/src/components/settings/SettingsModal.tsx`

**Interfaces:**
- Consumes: Task 4 的三个 Wails 绑定

- [ ] **Step 1: 先读既有先例**

读 `AgentConfigPage.tsx`（面板结构与导航）与 `ConfirmDialog.tsx` / `ApprovalPrompt.tsx`（对话框的样式语言）。照它们的模式写，不新造设计语言。

- [ ] **Step 2: 写失败测试——对话框的三条规则**

`PluginConsentDialog.test.tsx`：

1. 能力渲染成**只读清单**，DOM 里**没有**能力对应的 checkbox（`queryAllByRole('checkbox')` 不包含能力项）
2. hosts/paths 渲染成 checkbox，默认全选
3. 插件声明了 hosts 且能力含 `http` 时，把 hosts 全部取消 → 确认按钮 disabled，并显示原因

第 1 条是本 task 的核心：**能力不是禁用的勾选框，是只读清单**。禁用的勾选框会让人以为是自己权限不足，而实际原因是「插件的能力声明不是菜单」。对话框要用一句话讲清这点。

第 3 条把 A5c 的拒绝规则前移到提交之前，别让人提交后才吃一个端点错误。

Run: `cd frontend && npx vitest run src/components/settings/PluginConsentDialog.test.tsx`
Expected: FAIL

**必须在 `frontend/` 目录里跑。** 父目录另有一个没有 jsdom 的 v4，在那里跑会 `document is not defined` 假失败。

- [ ] **Step 3: 实现对话框**

- [ ] **Step 4: 写失败测试——面板**

`PluginsPage.test.tsx`：三态各渲染出不同徽章与不同的下一步文案（`unauthorized` 给「授权」、`disabled` 不催人操作、`failed` 显示原因）；`pending_convergence` 为 true 时显示「已授权，待收敛」、把 `convergence_detail` 里的「还在等几个任务」呈现出来，并给出重试按钮。

**等待期间不放取消按钮**，且要有一条测试断言它不存在。阻塞的是 server 端的 Apply，Wails 绑定没有 abort 语义——前端放弃等待拦不住 server 把 `apply_wait_ms` 等完。一个实际取消不了任何东西的按钮，正是本期在防的那类谎。

- [ ] **Step 5: 实现面板并挂进 SettingsModal**

与 `AgentConfigPage` 平级加一页。

- [ ] **Step 6: 跑全部前端测试**

Run: `cd frontend && npx vitest run`
Expected: PASS（含既有测试）

- [ ] **Step 7: 变异验证**

把能力也渲染成 checkbox → Step 2 第 1 条必须 FAIL。粘贴输出，还原。

- [ ] **Step 8: 提交（GUI 仓）**

```bash
git add frontend/src/components/settings/PluginsPage.tsx frontend/src/components/settings/PluginsPage.test.tsx frontend/src/components/settings/PluginConsentDialog.tsx frontend/src/components/settings/PluginConsentDialog.test.tsx frontend/src/components/settings/SettingsModal.tsx
git commit -m "feat(plugin): add the plugin consent panel to settings"
```

---

### Task 6: 端到端验收与文档回写

**Files:**
- Modify: `internal/plugin/loader/e2e_test.go`（server 仓，追加，不重写既有用例）
- Modify: docs 仓 `F:\source\stardust\Legion\docs\design\architecture\legion-plugin-system.md`

**⚠️ 本 task 反复挂载真 wasm 实例，fork-bomb 规程逐条适用**：轮数写死 ≤5，每轮断言实例数上限，`-timeout`，`-p 1`，**写完再跑**，绝不拿半成品挂载循环去迭代。

- [ ] **Step 1: 端到端闭环（走 HTTP，不走 CLI）**

```text
装一个未授权插件（直接写 plugins.json，装本身不在本期范围）
  → GET /v1/plugins        => 该条目 state=unauthorized，declared 有能力、granted 为空
  → POST .../grant         => 200，插件挂载，工具进注册表
  → GET /v1/plugins        => state=loaded，granted 与 declared 一致
  → POST .../deny          => 200，工具消失
  → GET /v1/plugins        => state=disabled（不是 unauthorized——决定做过了）
  → POST .../grant         => 200，重新挂载
```

倒数第二步是本期最容易做错的断言：`deny` 之后是 `disabled` 而**不是** `unauthorized`，因为 `GrantStated` 仍为 true。两者运维要做的下一步完全不同。

- [ ] **Step 2: 拒绝路径**

能力严格子集被拒且 `plugins.json` 逐字节未变；授 `http` 但 hosts 为空被拒（声明非空时）；并发编辑被拒。

- [ ] **Step 3: 全量回归**

```bash
go build ./... && go vet ./... && go test ./... -count=1 -timeout 900s && gofmt -l .
go test -race ./internal/plugin/... ./internal/cli/ ./internal/server/ -count=1 -timeout 900s -p 1
```
GUI 仓：`go test ./... -count=1 -timeout 300s` 与 `cd frontend && npx vitest run`

- [ ] **Step 4: 文档回写（docs 仓）**

§9 路线图里「GUI 授权同意流（下一期）」那一行标交付，写明：三个端点、共享校验包、四种结果里 `pending_convergence` 的语义。§7 补一句：GUI 与 CLI 走**同一套**校验与并发守卫，不是两条各自验证的路径。

**不要写代码没做的事**：本期不做安装、不做实时推送、不做真机验证——这三条要如实列进「未包含」。A5b 那份缓存文档宣称了代码没有的跨进程安全，不要重演。

- [ ] **Step 5: 提交（两个仓，两次提交）**

```bash
# server 仓
git add internal/plugin/loader/e2e_test.go
git commit -m "test(plugin): accept the consent loop end to end over HTTP"
```

```bash
# docs 仓（cd F:\source\stardust\Legion\docs）
git add design/architecture/legion-plugin-system.md
git commit -m "docs(plugin): the GUI consent flow ships on the shared validation path"
```

---

## 交付后状态

- `GET /v1/plugins` / `POST /v1/plugins/{name}/grant|deny` 三个端点，继承 loopback hardening 与 Bearer 鉴权
- 能力校验、allowlist 规则、并发守卫在 `internal/plugin/consent` 里**只有一份**，CLI 与端点共用
- 授权在 GUI 里点一下就触发收敛；收敛没发生时如实报 `pending_convergence`，不假装已生效
- 同意对话框：能力只读清单 + 确认，hosts/paths 可取消勾选，明显违规在提交前就被拦住
- `deny` 后条目仍在，`Source`/`Digest` 完好，且状态是 `disabled` 而非 `unauthorized`

**尚未包含**：GUI 里安装插件、`search` 与插件市场、OCI registry 传输、插件状态实时推送、密钥吊销与透明日志。

**未做真机验证**：证据来自单测、端到端测试与真 wasm 夹具。六期下来从没有第三方插件在真机上挂载过，本期同样不改变这一点。
