# 浏览器快照三级降级 + 落盘 + 翻页指针 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 浏览器观测渲染文本超 rune 阈值时按三级降级（任务导向小模型抽取 → 按行截断），全文哈希去重落盘并给出 `read_file` 翻页指针，省 token 且不失能。

**Architecture:** browser 包内新增纯函数 `DegradeObservation` 编排降级梯，LLM 抽取与磁盘落盘经 `SnapshotExtractor`/`SnapshotArchive` 两个本地接口注入（browser 包不 import LLM/port，守平台无关）。任务文本 `domain.Task.Input` 经 ctx value 从 `dispatchToolCall` 下传到 browser 工具 handler；工具根在 `RegisterBrowserTools` 注册闭包捕获（与 `read_file` 同源）。fail-loud：抽取器 nil = 契约可选（降级到截断），配置了却报错 = 硬失败返 error。

**Tech Stack:** Go、go-rod、`port.MaasInferenceClient`、标准库 `crypto/sha256`/`os`/`path/filepath`。

**关联设计：** `docs/superpowers/specs/2026-08-11-browser-snapshot-degradation-design.md`

---

## 文件结构

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/browser/observation.go` | 改 | 加 `SnapshotExtractor`/`SnapshotArchive` 接口、`TruncateByLine`、`DegradeObservation`、footer 渲染 |
| `internal/browser/observation_degrade_test.go` | 建 | 纯函数表驱动测试（fake 接口） |
| `internal/browser/snapshot_archive.go` | 建 | `fileSnapshotArchive`：sha256 去重落盘 + TTL 清理 |
| `internal/browser/snapshot_archive_test.go` | 建 | 真实文件落盘/去重/清理测试 |
| `internal/browser/api.go` | 改 | `OpenReq/ReadReq/ClickReq/TypeReq` 加 `UserTask`/`ToolRoot` |
| `internal/browser/runtime.go` | 改 | `RuntimeConfig` 加降级字段；`observe` 加 `ctx,userTask,toolRoot`；4 处调用点透传；`NewRuntime` 装配默认 Archive |
| `internal/adapter/browser_extractor.go` | 建 | `MaasSnapshotExtractor`：包 `MaasInferenceClient`，拼任务导向 prompt |
| `internal/adapter/browser_extractor_test.go` | 建 | fake maas 验 prompt/错误透传 |
| `internal/tool/ctxtask.go` | 建 | `WithUserTask`/`UserTaskFromContext` ctx 助手 |
| `internal/tool/browser.go` | 改 | `BrowserToolOptions` 加 `ToolRoot`；handler 从 ctx 取任务、填 Req |
| `internal/runtime/lazytools.go` | 改 | `dispatchToolCall` 注入 `tool.WithUserTask(ctx, task.Input)` |
| `internal/config/config.go` | 改 | `BrowserConfig` 加 4 字段 + 默认值 |
| `internal/cli/command.go` | 改 | 装配注入 extractor/archive/阈值/TTL/dir + ToolRoot |

---

## Task 1: 纯函数降级层 + 接口定义

**Files:**
- Modify: `internal/browser/observation.go`
- Test: `internal/browser/observation_degrade_test.go`（建）

- [ ] **Step 1: 写失败测试**

建 `internal/browser/observation_degrade_test.go`：

```go
package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeExtractor / fakeArchive 让纯函数测试不碰 LLM/磁盘。
type fakeExtractor struct {
	out string
	err error
}

func (f fakeExtractor) Extract(_ context.Context, _, _ string) (string, error) {
	return f.out, f.err
}

type fakeArchive struct {
	rel      string
	err      error
	savedArg string
}

func (f *fakeArchive) Save(_ , content string) (string, error) {
	f.savedArg = content
	return f.rel, f.err
}
func (f *fakeArchive) Cleanup(string, time.Duration) error { return nil }

func obsWithText(t string) Observation { return Observation{Text: t} }

func TestDegradeObservation_UnderThreshold_ReturnsAsIs(t *testing.T) {
	obs := obsWithText("short text")
	arch := &fakeArchive{rel: "x"}
	got, err := DegradeObservation(context.Background(), obs, "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Text != "short text" || got.Truncated {
		t.Fatalf("got %+v, want unchanged untruncated", got)
	}
	if arch.savedArg != "" {
		t.Fatalf("archive.Save called under threshold; want not called")
	}
}

func TestDegradeObservation_NilExtractor_TruncatesWithPointer(t *testing.T) {
	full := strings.Repeat("[e1] <link> aaaaa\n", 50) // 远超阈值
	arch := &fakeArchive{rel: ".legion/browser/snapshots/deadbeef.txt"}
	got, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	if !strings.Contains(got.Text, "read_file(path=\".legion/browser/snapshots/deadbeef.txt\")") {
		t.Fatalf("footer missing pointer, got:\n%s", got.Text)
	}
	if arch.savedArg != full {
		t.Fatalf("archive got wrong content")
	}
	if len([]rune(got.Text)) > 100+footerMaxRunes {
		t.Fatalf("truncated text still too long: %d runes", len([]rune(got.Text)))
	}
}

func TestDegradeObservation_Extractor_ReducesText(t *testing.T) {
	full := strings.Repeat("[e1] <link> noise\n", 50)
	arch := &fakeArchive{rel: "p.txt"}
	got, err := DegradeObservation(context.Background(), obsWithText(full), "buy milk", "/root",
		DegradeDeps{Extractor: fakeExtractor{out: "[e1] <button> Buy"}, Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !strings.HasPrefix(got.Text, "[e1] <button> Buy") {
		t.Fatalf("reduced text missing, got:\n%s", got.Text)
	}
}

func TestDegradeObservation_ExtractorError_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{rel: "p.txt"}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Extractor: fakeExtractor{err: errors.New("boom")}, Archive: arch}, 100)
	if err == nil || !strings.Contains(err.Error(), "extract") {
		t.Fatalf("err = %v, want wrapped extract error", err)
	}
}

func TestDegradeObservation_ExtractorEmpty_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{rel: "p.txt"}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Extractor: fakeExtractor{out: "   "}, Archive: arch}, 100)
	if err == nil {
		t.Fatalf("err = nil, want error on empty extraction")
	}
}

func TestDegradeObservation_ArchiveError_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{err: errors.New("disk full")}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("err = %v, want wrapped archive error", err)
	}
}

func TestTruncateByLine_KeepsWholeLines(t *testing.T) {
	in := "line-one\nline-two\nline-three\n"
	out := TruncateByLine(in, 12) // 只够 "line-one\n"（9 runes），第二行会超
	if strings.Contains(out, "line-two") {
		t.Fatalf("cut mid-content, got %q", out)
	}
	if !strings.Contains(out, "line-one") {
		t.Fatalf("dropped first line, got %q", out)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/browser/ -run 'DegradeObservation|TruncateByLine' -v`
Expected: FAIL（`DegradeObservation`/`DegradeDeps`/`TruncateByLine`/`footerMaxRunes` 未定义；`time` 未导入）

- [ ] **Step 3: 实现纯函数与接口**

在 `internal/browser/observation.go` 顶部 import 加 `context`、`time`，并追加：

```go
// SnapshotExtractor 用当前任务从全量观测文本抽取相关内容。实现在 browser 包之外
// （adapter 层包 MaasInferenceClient），使 browser 核心不依赖 LLM，守平台无关。
type SnapshotExtractor interface {
	Extract(ctx context.Context, task, snapshot string) (string, error)
}

// SnapshotArchive 把全量观测文本落盘到工具根下，返回相对根的路径供 read_file 翻页。
type SnapshotArchive interface {
	// Save 幂等：同内容返回同路径且不重复写（内容哈希去重）。
	Save(root, content string) (relPath string, err error)
	// Cleanup 删除 root 下超过 ttl 的旧快照，best-effort。
	Cleanup(root string, ttl time.Duration) error
}

// DegradeDeps 注入降级所需的外部能力。Extractor 可为 nil（契约声明的可选槽：
// 未配抽取器时降级直接走截断）；Archive 在阈值>0 时必需。
type DegradeDeps struct {
	Extractor SnapshotExtractor
	Archive   SnapshotArchive
}

// footerMaxRunes 是翻页指针 footer 的 rune 上限估算，供测试给截断预留余量。
const footerMaxRunes = 120

// DegradeObservation 在渲染文本超阈值时按三级降级：
//  ① 全量落盘（哈希去重）拿到翻页路径；
//  ② 有抽取器且有任务 → 小模型按任务抽取（失败/空 → 硬失败返 error）；
//  ③ 仍超阈 → 按行截断；
// 末尾附 read_file 翻页 footer。threshold<=0 或未超阈则原样返回、不落盘。
// obs.Elements 与 ref 表不变（截断只作用于给模型看的 Text），故 ref 仍可解析。
func DegradeObservation(ctx context.Context, obs Observation, userTask, toolRoot string, deps DegradeDeps, threshold int) (Observation, error) {
	full := obs.Text
	if threshold <= 0 || len([]rune(full)) <= threshold {
		return obs, nil
	}
	if deps.Archive == nil {
		return Observation{}, fmt.Errorf("browser snapshot degrade: archive required above threshold")
	}
	relPath, err := deps.Archive.Save(toolRoot, full)
	if err != nil {
		return Observation{}, fmt.Errorf("browser snapshot archive: %w", err)
	}

	text := full
	if deps.Extractor != nil && strings.TrimSpace(userTask) != "" {
		reduced, err := deps.Extractor.Extract(ctx, userTask, full)
		if err != nil {
			return Observation{}, fmt.Errorf("browser snapshot extract: %w", err)
		}
		if strings.TrimSpace(reduced) == "" {
			return Observation{}, fmt.Errorf("browser snapshot extract: empty result")
		}
		text = reduced
	}

	total := len([]rune(full))
	if len([]rune(text)) > threshold {
		text = TruncateByLine(text, threshold)
	}
	shown := len([]rune(text))
	obs.Text = text + snapshotFooter(relPath, shown, total)
	obs.Truncated = true
	return obs, nil
}

// snapshotFooter 渲染翻页指针。路径原样给 read_file（相对工具根，模型可翻页取回全文）。
func snapshotFooter(relPath string, shown, total int) string {
	return fmt.Sprintf("\n[已裁剪：显示 %d/%d 字；全文见 read_file(path=%q)]\n", shown, total, relPath)
}

// TruncateByLine 按 \n 边界把文本截到 budget 个 rune 内，不切碎单行。
// 若首行本身超 budget，则对该行按 rune 硬截（避免返回空）。
func TruncateByLine(text string, budget int) string {
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	used := 0
	for _, ln := range lines {
		r := len([]rune(ln))
		if used+r > budget {
			if used == 0 { // 首行即超：硬截该行
				return string([]rune(ln)[:budget])
			}
			break
		}
		b.WriteString(ln)
		used += r
	}
	return b.String()
}
```

（`observation.go` 已 import `fmt`、`strings`；只需补 `context`、`time`。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/browser/ -run 'DegradeObservation|TruncateByLine' -v`
Expected: PASS（全部子测试）

- [ ] **Step 5: 提交**

```bash
git add internal/browser/observation.go internal/browser/observation_degrade_test.go
git commit -m "feat(browser): add snapshot degradation pure funcs + extractor/archive interfaces"
```

---

## Task 2: 文件落盘实现 fileSnapshotArchive

**Files:**
- Create: `internal/browser/snapshot_archive.go`
- Test: `internal/browser/snapshot_archive_test.go`

- [ ] **Step 1: 写失败测试**

建 `internal/browser/snapshot_archive_test.go`：

```go
package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileArchive_SaveWritesAndReturnsRelPath(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel, err := a.Save(root, "hello world")
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	if !strings.HasPrefix(rel, ".legion/browser/snapshots/") || !strings.HasSuffix(rel, ".txt") {
		t.Fatalf("rel = %q, unexpected shape", rel)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read back err = %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("content = %q, want hello world", data)
	}
}

func TestFileArchive_DedupSameContent(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel1, _ := a.Save(root, "same")
	rel2, _ := a.Save(root, "same")
	if rel1 != rel2 {
		t.Fatalf("dedup failed: %q != %q", rel1, rel2)
	}
}

func TestFileArchive_CleanupRemovesExpired(t *testing.T) {
	root := t.TempDir()
	a := newFileSnapshotArchive(".legion/browser/snapshots")
	rel, _ := a.Save(root, "old")
	p := filepath.Join(root, filepath.FromSlash(rel))
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes err = %v", err)
	}
	if err := a.Cleanup(root, 24*time.Hour); err != nil {
		t.Fatalf("Cleanup err = %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expired file still present, stat err = %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/browser/ -run FileArchive -v`
Expected: FAIL（`newFileSnapshotArchive` 未定义）

- [ ] **Step 3: 实现**

建 `internal/browser/snapshot_archive.go`：

```go
package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// fileSnapshotArchive 把全量观测文本按内容哈希落盘到 <root>/<dir>/<sha>.txt。
// 幂等去重（同内容命同名文件，存在即跳过写）。dir 用斜杠相对路径，返回值同样用斜杠，
// 供 read_file（接受相对工具根路径）翻页。
type fileSnapshotArchive struct {
	dir string // 相对工具根，如 ".legion/browser/snapshots"
}

func newFileSnapshotArchive(dir string) *fileSnapshotArchive {
	if dir == "" {
		dir = ".legion/browser/snapshots"
	}
	return &fileSnapshotArchive{dir: dir}
}

func (a *fileSnapshotArchive) Save(root, content string) (string, error) {
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:]) + ".txt"
	rel := a.dir + "/" + name // 斜杠相对路径（read_file 契约）
	abs := filepath.Join(root, filepath.FromSlash(a.dir), name)
	if _, err := os.Stat(abs); err == nil {
		return rel, nil // 去重：同内容已存在
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write snapshot %s: %w", name, err)
	}
	return rel, nil
}

func (a *fileSnapshotArchive) Cleanup(root string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	base := filepath.Join(root, filepath.FromSlash(a.dir))
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录还没建，无需清理
		}
		return fmt.Errorf("read snapshot dir: %w", err)
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(base, e.Name())) // best-effort
		}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/browser/ -run FileArchive -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/browser/snapshot_archive.go internal/browser/snapshot_archive_test.go
git commit -m "feat(browser): add fileSnapshotArchive with hash dedup and TTL cleanup"
```

---

## Task 3: 抽取器适配 MaasSnapshotExtractor

**Files:**
- Create: `internal/adapter/browser_extractor.go`
- Test: `internal/adapter/browser_extractor_test.go`

- [ ] **Step 1: 写失败测试**

建 `internal/adapter/browser_extractor_test.go`：

```go
package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/port"
)

type stubMaas struct {
	gotPrompt string
	resp      port.InferenceResponse
	err       error
}

func (s *stubMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	s.gotPrompt = req.Prompt
	return s.resp, s.err
}

func TestMaasSnapshotExtractor_BuildsTaskPromptAndTrims(t *testing.T) {
	m := &stubMaas{resp: port.InferenceResponse{Text: "  [e1] <button> Buy  "}}
	e := NewMaasSnapshotExtractor(m)
	out, err := e.Extract(context.Background(), "buy milk", "[e1] <button> Buy\n[e2] <link> Ads")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != "[e1] <button> Buy" {
		t.Fatalf("out = %q, want trimmed", out)
	}
	if !strings.Contains(m.gotPrompt, "buy milk") || !strings.Contains(m.gotPrompt, "[e2] <link> Ads") {
		t.Fatalf("prompt missing task or snapshot: %q", m.gotPrompt)
	}
	if !strings.Contains(m.gotPrompt, "[eN]") {
		t.Fatalf("prompt missing ref-preservation instruction")
	}
}

func TestMaasSnapshotExtractor_PropagatesError(t *testing.T) {
	m := &stubMaas{err: errors.New("upstream down")}
	e := NewMaasSnapshotExtractor(m)
	_, err := e.Extract(context.Background(), "t", "s")
	if err == nil {
		t.Fatalf("err = nil, want propagated")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/adapter/ -run MaasSnapshotExtractor -v`
Expected: FAIL（`NewMaasSnapshotExtractor` 未定义）

- [ ] **Step 3: 实现**

建 `internal/adapter/browser_extractor.go`：

```go
package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/port"
)

// MaasSnapshotExtractor 用注入的推理客户端把全量 a11y 观测按当前任务抽取相关行。
// 满足 browser.SnapshotExtractor（结构化实现，不反向 import browser 避免环）。
type MaasSnapshotExtractor struct {
	client port.MaasInferenceClient
}

func NewMaasSnapshotExtractor(client port.MaasInferenceClient) *MaasSnapshotExtractor {
	return &MaasSnapshotExtractor{client: client}
}

func (e *MaasSnapshotExtractor) Extract(ctx context.Context, task, snapshot string) (string, error) {
	resp, err := e.client.Generate(ctx, port.InferenceRequest{
		RequestID: "browser-snapshot-extract",
		Prompt:    buildExtractPrompt(task, snapshot),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text), nil
}

func buildExtractPrompt(task, snapshot string) string {
	return fmt.Sprintf(`你在为一个浏览器自动化 agent 精简页面可访问性快照。
当前任务：%s

下面是完整快照，每行形如 [eN] <role> name。只保留与"当前任务"相关的可交互元素行，
删除无关行（广告、页脚、无关导航等）。**必须原样保留每行开头的 [eN] 引用标记**，
agent 后续用它定位元素。只输出保留的行，不要解释、不要改写标记。

完整快照：
%s`, task, snapshot)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/adapter/ -run MaasSnapshotExtractor -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/adapter/browser_extractor.go internal/adapter/browser_extractor_test.go
git commit -m "feat(adapter): add MaasSnapshotExtractor for task-directed snapshot reduction"
```

---

## Task 4: 接入 Runtime（配置字段 + observe 透传）

**Files:**
- Modify: `internal/browser/api.go`（`*Req` 加字段）
- Modify: `internal/browser/runtime.go`（`RuntimeConfig`、`NewRuntime`、`observe`、4 处调用点）
- Test: `internal/browser/runtime.go` 现有测试 + 新增 `internal/browser/observe_degrade_wiring_test.go`

- [ ] **Step 1: 加 Req 字段**

`internal/browser/api.go` 给 `OpenReq/ReadReq/ClickReq/TypeReq` 各加两字段（示例 `OpenReq`，其余同样加）：

```go
type OpenReq struct {
	URL       string
	SessionID string
	TaskID    string
	UserTask  string // 当前 agent 任务文本，供超阈快照按任务抽取；空则跳过抽取
	ToolRoot  string // 与 read_file 同源的工具根，落盘全文使其可被翻页
}
```

`ReadReq`/`ClickReq`/`TypeReq` 同样追加：

```go
	UserTask string
	ToolRoot string
```

- [ ] **Step 2: 扩 RuntimeConfig + NewRuntime 装配默认 Archive**

`internal/browser/runtime.go` 的 `RuntimeConfig` 追加：

```go
	// 快照降级（Task: browser-snapshot-degradation）。
	Extractor             SnapshotExtractor // 可空：nil 则超阈快照只截断不抽取
	Archive               SnapshotArchive   // 可空：nil 时 NewRuntime 装配 fileSnapshotArchive
	SnapshotRuneThreshold int               // 渲染文本超此 rune 数触发降级；<=0 关闭降级
	SnapshotTTL           time.Duration     // 落盘全文保留时长；<=0 不清理
	SnapshotArchiveDir    string            // 相对工具根的落盘子目录；空=默认
```

在 `NewRuntime`（`r := &Runtime{...}` 之后、起 reaper 之前）加装配：

```go
	if cfg.SnapshotRuneThreshold > 0 && r.cfg.Archive == nil {
		r.cfg.Archive = newFileSnapshotArchive(cfg.SnapshotArchiveDir)
	}
```

- [ ] **Step 3: 改 observe 签名 + 末尾降级**

`internal/browser/runtime.go` 把 `observe` 改为携带 ctx/任务/根，并在返回前降级：

```go
func (r *Runtime) observe(ctx context.Context, page *rod.Page, sess *Session, userTask, toolRoot string) (Observation, error) {
	tree, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	// ...（原有抽取 + BuildObservation + sess.Refs 重建逻辑不变）...
	// 原本 `return obs, nil` 改为：
	return DegradeObservation(ctx, obs, userTask, toolRoot,
		DegradeDeps{Extractor: r.cfg.Extractor, Archive: r.cfg.Archive},
		r.cfg.SnapshotRuneThreshold)
}
```

（`sess.Refs` 在 `DegradeObservation` 前已按全量 `obs.Elements` 建好，降级只改 `obs.Text`，ref 仍可解析。）

- [ ] **Step 4: 改 4 处调用点**

`Open`（runtime.go:341）、`Read`（:401）、`Click`（:434）、`Type`（:475）里的 `r.observe(page, sess)` 改为透传 ctx 与 Req 字段：

- Open（在 `sess.WithLock` 内，ctx 为 `Open` 入参）：
```go
		obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot)
```
- Read：
```go
	sess.WithLock(func() { obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot) })
```
- Click / Type（各自 `sess.WithLock` 内的 `r.observe(page, sess)`）：
```go
		obs, opErr = r.observe(ctx, page, sess, req.UserTask, req.ToolRoot)
```

- [ ] **Step 5: 写接线测试**

建 `internal/browser/observe_degrade_wiring_test.go`——验 `RuntimeConfig` 阈值为 0 时 `DegradeObservation` 不改文本（保证默认行为不变）：

```go
package browser

import (
	"context"
	"strings"
	"testing"
)

func TestDegrade_ThresholdZero_NoChange(t *testing.T) {
	full := strings.Repeat("[e1] <link> x\n", 500)
	got, err := DegradeObservation(context.Background(), Observation{Text: full}, "t", "/root",
		DegradeDeps{}, 0) // 阈值 0 = 关闭
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Text != full || got.Truncated {
		t.Fatalf("threshold 0 should not degrade")
	}
}
```

- [ ] **Step 6: 跑测试 + 全包编译**

Run: `go test ./internal/browser/ -v && go build ./...`
Expected: PASS；编译通过（现有 `observe` 调用点若测试里有直接调用需同步改签名——按编译报错逐一修）

- [ ] **Step 7: 提交**

```bash
git add internal/browser/api.go internal/browser/runtime.go internal/browser/observe_degrade_wiring_test.go
git commit -m "feat(browser): thread task/root into observe and apply snapshot degradation"
```

---

## Task 5: 任务文本 ctx 管道 + 工具根捕获 + handler 接线

**Files:**
- Create: `internal/tool/ctxtask.go`
- Modify: `internal/tool/browser.go`（`BrowserToolOptions.ToolRoot` + handler 填 Req）
- Modify: `internal/runtime/lazytools.go`（`dispatchToolCall` 注入 ctx）
- Test: `internal/tool/ctxtask_test.go`、`internal/tool/browser_events_test.go`（扩）

- [ ] **Step 1: 写 ctx 助手失败测试**

建 `internal/tool/ctxtask_test.go`：

```go
package tool

import (
	"context"
	"testing"
)

func TestUserTaskContextRoundTrip(t *testing.T) {
	ctx := WithUserTask(context.Background(), "buy milk")
	if got := UserTaskFromContext(ctx); got != "buy milk" {
		t.Fatalf("got %q, want buy milk", got)
	}
}

func TestUserTaskFromContext_Absent(t *testing.T) {
	if got := UserTaskFromContext(context.Background()); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tool/ -run UserTask -v`
Expected: FAIL（未定义）

- [ ] **Step 3: 实现 ctx 助手**

建 `internal/tool/ctxtask.go`：

```go
package tool

import "context"

type userTaskKey struct{}

// WithUserTask 把当前 agent 任务文本放进 ctx，供工具（如 browser）按任务定制行为。
func WithUserTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, userTaskKey{}, task)
}

// UserTaskFromContext 取任务文本；不存在返回空串（契约允许缺省——非 browser 场景不注入）。
func UserTaskFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userTaskKey{}).(string); ok {
		return v
	}
	return ""
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tool/ -run UserTask -v`
Expected: PASS

- [ ] **Step 5: BrowserToolOptions 加 ToolRoot + handler 填 Req**

`internal/tool/browser.go`：给 `BrowserToolOptions` 加字段：

```go
type BrowserToolOptions struct {
	Enabled  bool
	Runtime  browser.RuntimeAPI
	Events   BrowserEventSink
	ToolRoot string // 与 read_file 同源的工具根；落盘全文快照使其可被 read_file 翻页
}
```

四个 handler 各自把 ctx 任务与 `opts.ToolRoot` 填进 Req。改后：

```go
	registry.RegisterDescriptor(browserOpenDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		url := call.Arguments["url"]
		if url == "" {
			return failure(call.ID, "url is required"), nil
		}
		out, err := rt.Open(ctx, browser.OpenReq{
			URL: url, SessionID: call.Arguments["session_id"],
			UserTask: UserTaskFromContext(ctx), ToolRoot: opts.ToolRoot,
		})
		// ...（其余不变）
```

`browser_read`：
```go
		obs, err := rt.Read(ctx, browser.ReadReq{
			SessionID: call.Arguments["session_id"], Mode: call.Arguments["mode"],
			UserTask: UserTaskFromContext(ctx), ToolRoot: opts.ToolRoot,
		})
```

`browser_click`：
```go
		obs, err := rt.Click(ctx, browser.ClickReq{
			SessionID: call.Arguments["session_id"], Ref: call.Arguments["ref"],
			UserTask: UserTaskFromContext(ctx), ToolRoot: opts.ToolRoot,
		})
```

`browser_type`：
```go
		obs, err := rt.Type(ctx, browser.TypeReq{
			SessionID: call.Arguments["session_id"], Ref: call.Arguments["ref"],
			Text: call.Arguments["text"], Submit: submit,
			UserTask: UserTaskFromContext(ctx), ToolRoot: opts.ToolRoot,
		})
```

- [ ] **Step 6: 分发处注入任务文本**

`internal/runtime/lazytools.go` 的 `dispatchToolCall`（:135）在函数体最前注入（`task.Input` 已在 scope，`tool` 包已被 runtime 引用）：

```go
func (r *Runtime) dispatchToolCall(ctx context.Context, agent domain.Agent, task domain.Task, call domain.ToolCall, st *loopState) (domain.ToolResult, error) {
	ctx = tool.WithUserTask(ctx, task.Input)
	tools := st.tools
	// ...（其余不变）
```

（确认 `internal/runtime` 已 import `internal/tool`；若无则加 import `"github.com/stardust/legion-agent/internal/tool"`。）

- [ ] **Step 7: 集成测试 handler 透传**

在 `internal/tool/browser_events_test.go` 追加：用一个记录 `OpenReq` 的 fake `browser.RuntimeAPI`，断言 handler 把 ctx 任务与 ToolRoot 填进 Req：

```go
func TestBrowserOpen_PassesTaskAndRoot(t *testing.T) {
	fake := &recordingRuntime{} // 实现 browser.RuntimeAPI，记录最后一次 OpenReq
	reg := NewRegistry()
	RegisterBrowserTools(reg, BrowserToolOptions{Enabled: true, Runtime: fake, ToolRoot: "/ws"})
	ctx := WithUserTask(context.Background(), "find login")
	_, err := reg.Execute(ctx, domain.Agent{ID: "a1"}, domain.ToolCall{
		ID: "c1", Name: "browser_open", Arguments: map[string]string{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if fake.lastOpen.UserTask != "find login" || fake.lastOpen.ToolRoot != "/ws" {
		t.Fatalf("Req not populated: %+v", fake.lastOpen)
	}
}
```

（`recordingRuntime` 需实现 `browser.RuntimeAPI` 全部方法；`Open` 记录 `req` 到 `lastOpen` 并返回零值 `OpenObservation`。若测试文件已有类似 fake 则复用。）

- [ ] **Step 8: 跑测试 + 编译**

Run: `go test ./internal/tool/ ./internal/runtime/ -v && go build ./...`
Expected: PASS

- [ ] **Step 9: 提交**

```bash
git add internal/tool/ctxtask.go internal/tool/ctxtask_test.go internal/tool/browser.go internal/tool/browser_events_test.go internal/runtime/lazytools.go
git commit -m "feat: plumb task text via ctx and tool root into browser tool requests"
```

---

## Task 6: 配置透出 + 装配注入 + 回归

**Files:**
- Modify: `internal/config/config.go`（`BrowserConfig` + 默认值）
- Modify: `internal/cli/command.go`（装配注入）
- Test: `internal/config/config_test.go`（扩）

- [ ] **Step 1: 写配置默认值失败测试**

在 `internal/config/config_test.go` 追加：

```go
func TestBrowserSnapshotDefaults(t *testing.T) {
	cfg := Default() // 若默认构造函数名不同，按现有测试用法调整
	if cfg.Browser.SnapshotRuneThreshold != 15000 {
		t.Fatalf("SnapshotRuneThreshold = %d, want 15000", cfg.Browser.SnapshotRuneThreshold)
	}
	if cfg.Browser.MaxElements != 100 {
		t.Fatalf("MaxElements = %d, want 100", cfg.Browser.MaxElements)
	}
	if cfg.Browser.SnapshotTTLHours != 24 {
		t.Fatalf("SnapshotTTLHours = %d, want 24", cfg.Browser.SnapshotTTLHours)
	}
}
```

（`Default()` 的确切名字/用法照 `config_test.go` 现有默认值测试；如现有测试用 `Load(defaultPath)` 则同样处理。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run BrowserSnapshotDefaults -v`
Expected: FAIL（字段未定义）

- [ ] **Step 3: 加配置字段 + 默认值**

`internal/config/config.go` 的 `BrowserConfig`（:66）追加：

```go
	// MaxElements 是 a11y 观测保留的最大可交互元素数（次级硬上限）。0 = 默认 100。
	MaxElements int `json:"max_elements"`
	// SnapshotRuneThreshold 是渲染文本触发降级的 rune 阈值。0 = 关闭降级。
	SnapshotRuneThreshold int `json:"snapshot_rune_threshold"`
	// SnapshotTTLHours 是落盘全文快照保留时长（小时）。0 = 不清理。
	SnapshotTTLHours int `json:"snapshot_ttl_hours"`
	// SnapshotArchiveDir 是相对工具根的落盘子目录。空 = 默认 .legion/browser/snapshots。
	SnapshotArchiveDir string `json:"snapshot_archive_dir"`
```

在 `Default()` 里 `Browser` 结构默认值处（config.go 默认构造，Browser 段）补：

```go
		Browser: BrowserConfig{
			// ...（现有 Enabled/Headless 等保持）
			MaxElements:           100,
			SnapshotRuneThreshold: 15000,
			SnapshotTTLHours:      24,
			SnapshotArchiveDir:    ".legion/browser/snapshots",
		},
```

（若 `Default()` 未显式列 `Browser`，按现有其他段的默认赋值风格补一段。）

- [ ] **Step 4: 装配注入**

`internal/cli/command.go` 的 `runtimeCfg := browser.RuntimeConfig{...}`（:2348）追加字段，并构造 extractor/archive。`defaultMaas` 在此 scope 内已可用（:2336 已用）：

```go
		runtimeCfg := browser.RuntimeConfig{
			Headless:              cfg.Browser.Headless,
			BinPath:               cfg.Browser.BinPath,
			SessionTTL:            time.Duration(cfg.Browser.SessionTTLSeconds) * time.Second,
			ReapInterval:          time.Duration(cfg.Browser.ReapIntervalSeconds) * time.Second,
			MaxElements:           cfg.Browser.MaxElements,
			SnapshotRuneThreshold: cfg.Browser.SnapshotRuneThreshold,
			SnapshotTTL:           time.Duration(cfg.Browser.SnapshotTTLHours) * time.Hour,
			SnapshotArchiveDir:    cfg.Browser.SnapshotArchiveDir,
			Extractor:             adapter.NewMaasSnapshotExtractor(defaultMaas),
		}
```

（确认 `command.go` 已 import `internal/adapter`；若无则加。）

- [ ] **Step 5: 装配 ToolRoot 到 RegisterBrowserTools**

找到 `RegisterBrowserTools(...)` 的调用处（tool 注册装配，通常在 agentruntime resolver 或 command.go 工具注册段，与 `read_file` 注册用同一个工具根变量），给 `BrowserToolOptions` 传 `ToolRoot`：

```go
	RegisterBrowserTools(registry, BrowserToolOptions{
		Enabled:  cfg.Browser.Enabled,
		Runtime:  sharedBrowser,
		Events:   browserEventSink,
		ToolRoot: toolRoot, // 与该 registry 的 read_file/write_file 同源的根
	})
```

（用当前注册上下文里解析 `read_file` 用的那个根变量——即 `contextCfg.ContextFiles.Root` / `runtimeCfg.ToolRoot`，见 command.go:612/2026。确保是**同一个值**。）

- [ ] **Step 6: 跑配置测试 + 全量回归**

Run:
```bash
go test ./internal/config/ -run BrowserSnapshotDefaults -v
go build ./... && go vet ./... && go test ./... && gofmt -l .
```
Expected: 配置测试 PASS；build/vet 通过；`go test ./...` 全绿；`gofmt -l .` 输出为空

- [ ] **Step 7: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go internal/cli/command.go
git commit -m "feat(browser): expose snapshot degradation config and wire extractor/archive/root"
```

---

## Self-Review 记录

- **Spec 覆盖**：三级降级（Task 1 `DegradeObservation`）✓；rune 阈值（Task 1/4）✓；MaxElements 次级上限透出（Task 6）✓；任务导向抽取方案 C（Task 3 + Task 5 ctx 管道）✓；哈希去重落盘（Task 2）✓；TTL 清理（Task 2 `Cleanup`）✓；翻页指针 read_file（Task 1 `snapshotFooter` + Task 2 相对路径）✓；fail-loud（Task 1 硬失败分支 + nil 可选降级）✓；配置透出（Task 6）✓。
- **占位符扫描**：无 TBD/TODO；每步含真实代码与命令。
- **类型一致**：`SnapshotExtractor.Extract(ctx,task,snapshot)`、`SnapshotArchive.Save(root,content)`/`Cleanup(root,ttl)`、`DegradeDeps{Extractor,Archive}`、`DegradeObservation(ctx,obs,userTask,toolRoot,deps,threshold)`、`*Req{UserTask,ToolRoot}`、`RuntimeConfig.{Extractor,Archive,SnapshotRuneThreshold,SnapshotTTL,SnapshotArchiveDir}`、`BrowserToolOptions.ToolRoot`、`tool.WithUserTask/UserTaskFromContext` 跨任务一致。
- **待实现期确认点**（非阻塞，编译/现有测试会暴露）：① `Default()` 构造函数确切名/用法照 `config_test.go`；② `RegisterBrowserTools` 调用处的工具根变量名；③ `recordingRuntime` fake 是否已存在可复用；④ `internal/runtime` 是否已 import `internal/tool`（Registry 类型来源，极可能已 import）。

## Cleanup 顺带项（不在本计划范围，另行处理）

Task 6 落盘目录 `.legion/browser/` 建议加入工具根 `.gitignore`（避免污染用户仓库）——实现时若 `RegisterBrowserTools` 装配处便利可顺手加一行注释提示，否则记 chip 另办。
