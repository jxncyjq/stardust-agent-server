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

// NewMaasSnapshotExtractor 构造一个由 client 驱动的快照抽取器。
func NewMaasSnapshotExtractor(client port.MaasInferenceClient) *MaasSnapshotExtractor {
	return &MaasSnapshotExtractor{client: client}
}

// Extract 把 snapshot 按 task 精简为相关行，保留每行的 [eN] 引用标记。
// 上游推理错误以原始错误向上传播，供调用方降级处理。
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
