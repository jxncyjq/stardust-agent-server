package browser

import (
	"fmt"
	"strings"
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
	Ref   string `json:"ref"`  // 会话内稳定，如 "e1"
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
