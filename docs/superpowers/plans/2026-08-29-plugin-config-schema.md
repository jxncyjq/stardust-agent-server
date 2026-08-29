# 插件配置 schema 实施计划（G3）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让写错的插件配置在**加载期**被点名字段地拒绝，而不是运行时在 guest 里炸——今天宿主对 `plugins.json` 的 `config` 一个字都不校验，原文直传。

**Architecture:** 三段。①`plugin.json` 增加可选 `config_schema`，`manifest.ParsePlugin` 校验**这份 schema 本身**是不是本仓支持的子集（不合法的 schema 是插件作者的错，要在包被接受前就拒绝）；②`manifest` 新增 `ValidateConfig(pm, config)`，按 schema 校验部署侧写的 `config`；③`loader.prepare` 在装配 Spec 之后、激活之前调用它，不通过则该条走既有的 `fail` 路径变成 `failed`，`detail` 点名是哪个字段错在哪。

**Tech Stack:** Go 1.26.0，标准库 `encoding/json`。**不引入 JSON Schema 库**——见下方「为什么是子集」。

**上游依据:** 路线 `plans/2026-08-28-plugin-gap-closure-roadmap.md` 的 G3；与 Cordis 的比对（Cordis 用 zod `Config`，坏配置在加载期报错，Legion 今天什么都不做）。

## 为什么是子集而不是完整 JSON Schema

完整 JSON Schema 需要一个库（`santhosh-tekuri/jsonschema` 之类），而这条路上的每个字节都要被**不可信插件**带进部署：多一个依赖就是多一份要审的攻击面，也多一份要跟着升的东西。插件配置的实际形状是"一层或两层的对象，字段是字符串/数字/布尔/数组"，覆盖它不需要 `$ref`、`allOf`、`if/then`、正则等等。

因此支持的构造是**明确列出来的**，其余一律在 `ParsePlugin` 期被拒绝（fail-loud，不是"忽略不认识的关键字"——忽略会让作者以为自己的约束生效了）：

| 关键字 | 允许出现在 | 语义 |
|---|---|---|
| `type` | schema 根与每个属性 | `object` / `string` / `number` / `integer` / `boolean` / `array` |
| `properties` | `type: object` | 字段名 → 子 schema |
| `required` | `type: object` | 必填字段名，必须都在 `properties` 里 |
| `additional_properties` | `type: object` | 缺省 `false`：**未声明的字段是错误**（写错一个键名不该悄悄生效） |
| `items` | `type: array` | 元素的子 schema |
| `enum` | 任意标量 | 允许的取值 |
| `description` | 任意 | 只给人看 |

嵌套深度上限 **5**：一个五层嵌套的插件配置是设计问题，而无上限的递归校验器是拿不可信输入喂自己的栈。

## Global Constraints

- Fail-loud 铁律（`CLAUDE.md` §0）：禁止 fallback、禁止静默吞错、禁止零值假装正常。
- 公开标识符必须有 Go doc 注释，且不得与代码矛盾。
- 完成判据：`go build ./... && go vet ./... && go test ./...` 全绿、`gofmt -l .` 为空。
- **每个 task 至少跑一次 `go test ./...`**（不是包子集）。
- 错误路径必须有测试断言「确实返回 error 且点名了字段」，不得只测 happy path。
- 每个 task 做变异验证：把核心机制改坏，确认测试确实 FAIL，输出留在报告里，然后还原并 `git status` 核对。
- **向后兼容是硬要求**：没有 `config_schema` 的插件（今天所有的插件）行为必须一字不变，且要有测试钉住。
- 提交只 stage 本 task 自己的文件（显式路径），**永不 `git add -A`**。
- 不改 `plugin_example/`（G2 那条分支正在动它，避免冲突）。

## 前置事实（已在 master）

```go
// internal/plugin/manifest —— manifest.go
type PluginManifest struct{ Name, Version string; ABI int; SHA256 string;
    Capabilities []string; Limits Limits; Network Network; Filesystem Filesystem;
    Tools []ToolDecl; Requires []string }              // :119
func ParsePlugin(data []byte) (PluginManifest, error)  // :418，DisallowUnknownFields
type Entry struct{ Name, Source, Digest string; Enabled *bool; Grant *GrantDecl;
    Tools []ToolAccept; Config json.RawMessage }        // :523

// internal/plugin/loader —— loader.go
const stepAssembleSpec = "assemble-spec"; stepDependencies = "dependencies"  // :150-151
func (l *Loader) prepare(ctx, entry, root) (*convergePlan, error)            // :996
func (l *Loader) fail(ctx, name, version, step string, err error, prev *instance) error // :1139
// prepare 里已有的顺序：LoadPackage → 身份校验 → AssembleSpec → deps → fingerprint
```

---

### Task 1: 清单接受 `config_schema`，并校验这份 schema 本身

**Files:**
- Create: `internal/plugin/manifest/configschema.go`
- Modify: `internal/plugin/manifest/manifest.go`（`PluginManifest` 加字段，`validatePlugin` 调用新校验）
- Test: `internal/plugin/manifest/configschema_test.go`

**Interfaces:**
- Produces:
  - `PluginManifest.ConfigSchema json.RawMessage` （`json:"config_schema"`）
  - `func ParseConfigSchema(raw json.RawMessage) (*ConfigSchema, error)`
  - `type ConfigSchema struct{ … }`（不导出字段细节，只导出 `Validate`）

一份坏 schema 是**插件作者**的错，必须在包被接受时就拒绝：等到部署方写配置时才发现"这个插件的 schema 本身有毛病"，是把作者的 bug 变成运维的谜题。

- [ ] **Step 1: 写失败测试**

```go
func TestParsePluginAcceptsAConfigSchema(t *testing.T) {
	pm := mustParsePluginWith(t, `"config_schema":{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]},`)
	if len(pm.ConfigSchema) == 0 {
		t.Error("ConfigSchema is empty, want the document from plugin.json")
	}
}

func TestParsePluginRefusesAnUnsupportedSchemaKeyword(t *testing.T) {
	// 忽略不认识的关键字会让作者以为自己的约束生效了。
	_, err := parsePluginWith(`"config_schema":{"type":"object","patternProperties":{"^x":{"type":"string"}}},`)
	if err == nil {
		t.Fatal("ParsePlugin with patternProperties = nil error, want a refusal naming the keyword")
	}
	if !strings.Contains(err.Error(), "patternProperties") {
		t.Errorf("error = %v, want it to name the unsupported keyword", err)
	}
}

func TestParsePluginRefusesARequiredFieldThatIsNotDeclared(t *testing.T) {
	_, err := parsePluginWith(`"config_schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["b"]},`)
	if err == nil {
		t.Fatal("required naming an undeclared property = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("error = %v, want it to name the missing property", err)
	}
}

func TestParsePluginRefusesASchemaNestedTooDeep(t *testing.T) {
	deep := `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"object","properties":{"d":{"type":"object","properties":{"e":{"type":"object","properties":{"f":{"type":"string"}}}}}}}}}}}}`
	_, err := parsePluginWith(`"config_schema":` + deep + `,`)
	if err == nil {
		t.Fatal("a schema nested past the cap = nil error, want a refusal: an unbounded recursive " +
			"validator is fed untrusted input")
	}
}

func TestParsePluginWithoutAConfigSchemaIsUnchanged(t *testing.T) {
	// 向后兼容：今天所有的插件都没有这个字段。
	pm := mustParsePluginWith(t, "")
	if pm.ConfigSchema != nil {
		t.Errorf("ConfigSchema = %s, want nil when plugin.json declares none", pm.ConfigSchema)
	}
}
```

`parsePluginWith` / `mustParsePluginWith` 是本 task 要写的小助手：把一段 JSON 片段插进该文件既有的最小合法 plugin.json 里再 `ParsePlugin`。**先读一遍 `manifest_test.go` 现有的构造方式**，如果那里已经有等价助手就用它的，别新造平行的一套。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/plugin/manifest/ -run TestParsePlugin -v`
Expected: FAIL，`unknown field "config_schema"`（`DisallowUnknownFields` 先拦住）

- [ ] **Step 3: 实现**

`manifest.go`：

```go
	// ConfigSchema optionally describes the shape of the deployment-side
	// configuration this plugin expects (Entry.Config). It is a SUBSET of JSON
	// Schema — see ParseConfigSchema for exactly which keywords — and it is
	// what turns a typo in plugins.json from a runtime surprise inside the
	// guest into a load-time refusal that names the field.
	//
	// Absent means "this plugin makes no claim about its configuration", which
	// is what every plugin written before this field existed says, and the
	// deployment's config is then passed through unchecked exactly as before.
	ConfigSchema json.RawMessage `json:"config_schema"`
```

`configschema.go` 实现 `ParseConfigSchema`（解析 + 校验支持的关键字 + 深度上限 + `required` 必须在 `properties` 里）与 `(*ConfigSchema).Validate(config json.RawMessage) error`（Task 2 用）。`validatePlugin` 里：

```go
	if len(pm.ConfigSchema) > 0 {
		if _, err := ParseConfigSchema(pm.ConfigSchema); err != nil {
			return fmt.Errorf("parse plugin manifest %q: config_schema: %w", pm.Name, err)
		}
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/plugin/manifest/ -run TestParsePlugin -v` → PASS

- [ ] **Step 5: 全量 + 变异**

Run: `go test ./...`

变异：把「`required` 必须在 `properties` 里」那条检查删掉，确认
`TestParsePluginRefusesARequiredFieldThatIsNotDeclared` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/manifest/configschema.go internal/plugin/manifest/configschema_test.go internal/plugin/manifest/manifest.go
git commit -m "feat(plugin): plugin.json 接受 config_schema 并校验其本身"
```

---

### Task 2: 按 schema 校验部署侧的 config

**Files:**
- Modify: `internal/plugin/manifest/configschema.go`（`Validate`）
- Test: `internal/plugin/manifest/configschema_test.go`

**Interfaces:**
- Produces: `func (s *ConfigSchema) Validate(config json.RawMessage) error`
- Produces: `func ValidateEntryConfig(pm PluginManifest, config json.RawMessage) error`（Task 3 调它；`pm` 没声明 schema 时直接返回 nil）

**错误必须点名路径**：`config.retries: want integer, got string "3"` 比 `invalid config` 有用一个数量级——后者会让运维去逐个字段试。

- [ ] **Step 1: 写失败测试**

```go
func TestValidateEntryConfigAcceptsAConformingDocument(t *testing.T) { … }

func TestValidateEntryConfigNamesTheFieldWithTheWrongType(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{"retries":{"type":"integer"}}}`, `{"retries":"3"}`)
	if err == nil {
		t.Fatal("a string where an integer is declared = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "retries") {
		t.Errorf("error = %v, want it to name the field", err)
	}
}

func TestValidateEntryConfigNamesAMissingRequiredField(t *testing.T) { … }

func TestValidateEntryConfigRefusesAnUndeclaredField(t *testing.T) {
	// additional_properties 缺省 false：写错一个键名不该悄悄生效。
	err := validateWith(t, `{"type":"object","properties":{"endpoint":{"type":"string"}}}`, `{"endpiont":"x"}`)
	if err == nil {
		t.Fatal("an undeclared field = nil error, want a refusal: a typo'd key that is silently " +
			"ignored looks exactly like a setting that did not take effect")
	}
	if !strings.Contains(err.Error(), "endpiont") {
		t.Errorf("error = %v, want it to name the offending key", err)
	}
}

func TestValidateEntryConfigAllowsUndeclaredFieldsWhenTheSchemaSaysSo(t *testing.T) {
	err := validateWith(t, `{"type":"object","properties":{},"additional_properties":true}`, `{"anything":1}`)
	if err != nil { t.Errorf("… = %v, want nil", err) }
}

func TestValidateEntryConfigChecksEnums(t *testing.T) { … }

func TestValidateEntryConfigChecksArrayItems(t *testing.T) { … }

func TestValidateEntryConfigWithoutASchemaAcceptsAnything(t *testing.T) {
	// 向后兼容：没声明 schema 的插件，配置照旧原文直传。
	if err := ValidateEntryConfig(PluginManifest{}, json.RawMessage(`{"whatever":[1,2]}`)); err != nil {
		t.Errorf("ValidateEntryConfig with no schema = %v, want nil", err)
	}
}

func TestValidateEntryConfigRefusesAConfigThatIsNotAnObject(t *testing.T) {
	// 根 schema 是 object 时，一个数组配置是配置写错了，不是"没配置"。
	err := validateWith(t, `{"type":"object","properties":{}}`, `[1,2]`)
	if err == nil { t.Fatal("an array config against an object schema = nil error, want a refusal") }
}
```

- [ ] **Step 2-4: 红 → 实现 → 绿**

实现要点：JSON 数字统一按 `json.Number` 读（`encoding/json` 默认把数字读成 `float64`，那样 `integer` 与 `number` 区分不了，而 `retries: 1.5` 正是要拦的东西）。用 `json.Decoder` + `UseNumber()`。

- [ ] **Step 5: 全量 + 变异**

变异：把 `additional_properties` 的缺省从 false 改成 true，确认
`TestValidateEntryConfigRefusesAnUndeclaredField` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/manifest/configschema.go internal/plugin/manifest/configschema_test.go
git commit -m "feat(plugin): 按 config_schema 校验部署侧配置，错误点名字段"
```

---

### Task 3: 加载期拒绝不合 schema 的配置

**Files:**
- Modify: `internal/plugin/loader/loader.go`（`prepare`：新增 step 常量与校验调用）
- Test: `internal/plugin/loader/configschema_test.go`

**Interfaces:**
- Consumes: `manifest.ValidateEntryConfig`（Task 2）
- Produces: `const stepConfigSchema = "config-schema"`

**位置**：`AssembleSpec` 之后、`deps` 之前。之前不行——schema 来自 `plugin.json`，要先 `LoadPackage`；之后也不必——配置错了就不该再去建 host 依赖。

- [ ] **Step 1: 写失败测试**

```go
func TestApplyFailsAnEntryWhoseConfigDoesNotMatchTheSchema(t *testing.T) {
	h := newHarness(t)
	entry := h.writeEchoWithConfigSchema(t,
		`{"type":"object","properties":{"endpoint":{"type":"string"}},"required":["endpoint"]}`,
		`{"endpoint":42}`)
	// Apply 不因为一条 entry 失败而整体失败（既有语义），所以这里看 Status。
	h.applyExpectingFailure(entry)

	statuses := h.loader.Status()
	if len(statuses) != 1 || statuses[0].State != StateFailed {
		t.Fatalf("Status() = %+v, want one failed entry", statuses)
	}
	if !strings.Contains(statuses[0].LastError, "endpoint") {
		t.Errorf("LastError = %q, want it to name the offending field", statuses[0].LastError)
	}
	if h.toolNames() != nil {
		t.Error("a plugin with a bad config contributed tools; it must not be activated at all")
	}
}

func TestApplyMountsAnEntryWhoseConfigMatchesTheSchema(t *testing.T) { … }

func TestApplyIsUnchangedForAPluginWithoutAConfigSchema(t *testing.T) {
	// 向后兼容的回归网：既有夹具插件没有 config_schema，照常挂载。
	h := newHarness(t)
	h.apply(h.writeEcho("1.0.0", "echo_tool"))
	if len(h.toolNames()) != 1 { t.Errorf("tools = %v, want the plugin mounted", h.toolNames()) }
}
```

`writeEchoWithConfigSchema` 与 `applyExpectingFailure` 是本 task 要在 harness 上补的助手；**先读 `loader_test.go` 现有的 `writeEcho` / `apply`**，照它们的形状写，别新造平行夹具。

- [ ] **Step 2-4: 红 → 实现 → 绿**

`prepare` 里，紧跟 `spec, err := manifest.AssembleSpec(...)` 之后：

```go
	// 配置的形状是插件自己声明的（plugin.json 的 config_schema），部署方写的
	// 值必须对得上。放在这里而不是激活时：一条配置写错的 entry 不该先把 host
	// 依赖建起来，更不该等到 guest 里才炸——guest 侧的报错能力最弱。
	if err := manifest.ValidateEntryConfig(pm, entry.Config); err != nil {
		return nil, l.fail(ctx, entry.Name, pm.Version, stepConfigSchema, err, nil)
	}
```

- [ ] **Step 5: 全量 + -race + 变异**

Run: `go test ./... && go test -race ./internal/plugin/...`

变异：把这段校验调用注释掉，确认 `TestApplyFailsAnEntryWhoseConfigDoesNotMatchTheSchema` FAIL；还原。

- [ ] **Step 6: 提交**

```bash
git add internal/plugin/loader/loader.go internal/plugin/loader/configschema_test.go
git commit -m "feat(plugin): 加载期按 config_schema 拒绝写错的配置"
```

---

### Task 4: 文档

**Files:**
- Modify: docs 仓 `agents/reference/reference-legion-agent-plugins-001.md`（§4 清单规范加 `config_schema`；§9 排错加一行）
- Modify: docs 仓 `design/architecture/legion-plugin-system.md`（§9 路线表 G3 标记已交付）
- Modify: `sdk/rust/legion-plugin/README.md` 与 `pkg/legionplugin/README.md` 各加一句「配置形状建议用 `config_schema` 声明」**（仅当 G2 已合入 master；未合则跳过并在报告里说明）**

- [ ] **Step 1-3: 手册字段表 + 排错行 + 路线表**

字段表加一行：`config_schema` / 可选 / 「声明本插件期望的部署侧配置形状（JSON Schema 子集）。声明了就会在**加载期**校验 `plugins.json` 的 `config`，不合则该条 `failed` 并点名字段；不声明则配置原文直传，与今天一致」。

排错表加一行：`detail` 里出现 `config-schema` → 部署写的 `config` 与插件声明的形状不符，错误里有字段名。

§4 后面补一小节列出**支持的关键字表**（就是本计划开头那张）与深度上限，并写明「不认识的关键字是拒绝而不是忽略」。

- [ ] **Step 4: 提交（docs 仓单独分支与 PR）**

---

## 自检

**范围覆盖**：roadmap 的 G3 三件事——清单加字段（Task 1）、加载期校验并点名字段（Task 2+3）、呈现（Task 3 用既有的 `failed` + `detail` 通道，`GET /v1/plugins` 与 GUI 自动带出，不需要新 DTO 字段）——各有任务；文档在 Task 4。

**类型一致性**：`ParseConfigSchema(raw) (*ConfigSchema, error)` 在 Task 1 定义、Task 2 内部使用；`ValidateEntryConfig(pm, config) error` 在 Task 2 定义、Task 3 调用；`stepConfigSchema` 只在 Task 3 出现。

**已知留白**：测试夹具的确切名字（`parsePluginWith` / `writeEchoWithConfigSchema` / `applyExpectingFailure`）以各文件现状为准——本仓每个包都有自己的助手，新造平行的一套会更差。这是**指向现有代码**，不是 placeholder。

**刻意不做**：完整 JSON Schema（要引依赖，见开头）；schema 的默认值填充（"配置里没写就用 schema 的 default" 是另一套语义，会让 `config_get` 拿到的东西与 `plugins.json` 里写的不一致）；跨版本 schema 迁移。
