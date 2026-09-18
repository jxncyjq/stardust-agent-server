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

type taskIDKey struct{}

// WithTaskID 把当前正在跑的那条任务的 id 放进 ctx。工具只从这里知道「是哪条任务在调
// 用我」：domain.ToolCall 上没有这个字段，工具调用 id 也不是任务 id。
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskIDFromContext 取任务 id；不存在返回空串。
//
// 空串是「没人注入」，不是「顶层任务」——两者在 domain.Task.ParentTaskID 的契约里是
// 完全不同的两件事。把任务 id 写进落盘状态的调用方必须把空串当成接线缺口硬失败，不
// 许就地编一个。
func TaskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(taskIDKey{}).(string); ok {
		return v
	}
	return ""
}
