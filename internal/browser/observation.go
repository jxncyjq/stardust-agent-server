package browser

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RawA11yNode 是从 CDP Accessibility 域抽出、尚未裁剪的可访问性节点。
type RawA11yNode struct {
	Role        string
	Name        string
	Value       string
	Interactive bool
	Visible     bool
}

// Element 是裁剪后、分配了会话内稳定 ref 的可交互元素。
type Element struct {
	Ref   string `json:"ref"` // 会话内稳定，如 "e1"
	Role  string `json:"role"`
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// Observation 是回给 Agent 的页面表示（默认 a11y 树）。
type Observation struct {
	Elements  []Element `json:"elements"`
	Text      string    `json:"text"`      // 渲染成 [e1] <button> 搜索 的可读文本
	Truncated bool      `json:"truncated"` // 是否因预算裁剪
}

// ObservationBudget 控制裁剪预算。
type ObservationBudget struct {
	MaxElements int
}

const defaultMaxElements = 100

// BuildObservation 把原始 a11y 节点裁剪为 token 可控的观测：
// 只保留可交互且可见的节点，按顺序分配稳定 ref（e1、e2…），超预算截断。
// 纯函数，不碰 Chromium——ref 的会话内稳定性由调用方保证输入顺序稳定。
func BuildObservation(raw []RawA11yNode, budget ObservationBudget) Observation {
	max := budget.MaxElements
	if max <= 0 {
		max = defaultMaxElements
	}
	var obs Observation
	for _, n := range raw {
		if !n.Interactive || !n.Visible {
			continue
		}
		if len(obs.Elements) >= max {
			obs.Truncated = true
			break
		}
		obs.Elements = append(obs.Elements, Element{
			Ref:   fmt.Sprintf("e%d", len(obs.Elements)+1),
			Role:  n.Role,
			Name:  n.Name,
			Value: n.Value,
		})
	}
	obs.Text = renderObservation(obs.Elements)
	return obs
}

func renderObservation(elems []Element) string {
	var b strings.Builder
	for _, e := range elems {
		fmt.Fprintf(&b, "[%s] <%s> %s", e.Ref, e.Role, e.Name)
		if e.Value != "" {
			fmt.Fprintf(&b, " (value=%q)", e.Value)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

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
//
//	① 全量落盘（哈希去重）拿到翻页路径；
//	② 有抽取器且有任务 → 小模型按任务抽取（失败/空 → 硬失败返 error）；
//	③ 仍超阈 → 按行截断；
//
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
			if used == 0 {
				return string([]rune(ln)[:budget])
			}
			break
		}
		b.WriteString(ln)
		used += r
	}
	return b.String()
}
