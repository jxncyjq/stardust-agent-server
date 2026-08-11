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
