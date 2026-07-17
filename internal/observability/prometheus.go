package observability

import (
	"fmt"
	"sort"
	"strings"
)

func PrometheusText(snapshot MetricsSnapshot) string {
	var b strings.Builder
	writeCounterMap(&b, "legion_agent_http_requests_total", "HTTP responses by status code.", "status", snapshot.HTTPStatus)
	writeCounterMap(&b, "legion_agent_tasks_total", "Tasks by status.", "status", snapshot.Tasks)
	writeCounterMap(&b, "legion_agent_model_calls_total", "Model calls by status.", "status", snapshot.ModelCalls)
	writeCounterMap(&b, "legion_agent_approvals_total", "Approvals by status.", "status", snapshot.Approvals)
	writeCounterMap(&b, "legion_agent_workflows_total", "Workflow runs by status.", "status", snapshot.WorkflowRuns)
	return b.String()
}

func writeCounterMap(b *strings.Builder, name string, help string, label string, values map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, sanitizeMetricLabel(key), values[key])
	}
	if len(keys) == 0 {
		fmt.Fprintf(b, "%s 0\n", name)
	}
}

func sanitizeMetricLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}
