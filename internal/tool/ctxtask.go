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

type chatSessionKey struct{}

// WithChatSession 把当前 chat/对话 session id 放进 ctx，供 browser 工具据此复用同一对话内
// 的浏览器会话（会话 id 不随每条新消息自增、接管态延续）。
func WithChatSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, chatSessionKey{}, id)
}

// ChatSessionFromContext 取 chat session id；不存在返回空串（契约允许缺省——退回每次新建）。
func ChatSessionFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(chatSessionKey{}).(string); ok {
		return v
	}
	return ""
}
