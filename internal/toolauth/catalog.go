// Package toolauth defines which tools a per-agent config may disable.
package toolauth

import "sort"

// GateableTool is one tool a per-agent config may allow or disable, with a
// one-line description for the config UI.
type GateableTool struct {
	Name        string
	Description string
}

// gateable is the canonical set of tools a per-agent disabled_tools list may
// name. It is explicit data rather than a registry enumeration because the tools
// are registered across two packages (internal/tool and internal/runtime); a
// drift-guard test (internal/runtime) asserts every real production tool appears
// here, so a newly added tool that is not listed fails loudly.
//
// Meta-tools (call_tool, load_capabilities) are deliberately absent: they are
// always resident and never gated.
var gateable = []GateableTool{
	{"append_task_message", "向任务追加一条消息"},
	{"claim_task", "认领一个任务"},
	{"create_task", "创建新任务"},
	{"delegate_task", "把子任务委派给其他 agent（仅编排者）"},
	{"fetch_url", "抓取一个 URL 的内容"},
	{"list_files", "列出目录下的文件"},
	{"moa_consult", "向多个模型发起 MoA 咨询（仅编排者）"},
	{"read_file", "读取一个文件的内容"},
	{"read_messages", "读取 agent 间消息"},
	{"read_task", "读取一个任务的详情"},
	{"rebuild_tasks", "重建任务台账索引"},
	{"search_content", "在文件内容中搜索"},
	{"send_message", "向其他 agent 发送消息"},
	{"session_search", "跨会话检索历史（仅编排者）"},
	{"update_task", "更新一个任务的状态"},
	{"write_file", "写入/创建一个文件"},
}

// GateableTools returns the canonical gateable tools, sorted by name.
func GateableTools() []GateableTool {
	out := make([]GateableTool, len(gateable))
	copy(out, gateable)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GateableToolNames returns the gateable tool names as a set, for validating a
// disabled_tools list.
func GateableToolNames() map[string]bool {
	names := make(map[string]bool, len(gateable))
	for _, t := range gateable {
		names[t.Name] = true
	}
	return names
}

// IsGateable reports whether name is a tool a config may disable.
func IsGateable(name string) bool {
	return GateableToolNames()[name]
}
