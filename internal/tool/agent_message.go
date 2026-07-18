package tool

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

var agentMessageToolIDCounter uint64

type AgentMessageStore interface {
	SaveAgentMessage(context.Context, domain.AgentMessage) error
	ListAgentMessages(context.Context, domain.AgentMessageQuery) ([]domain.AgentMessage, error)
	MarkAgentMessageRead(context.Context, string, time.Time) error
}

func RegisterAgentMessageTools(registry *Registry, store AgentMessageStore) {
	if registry == nil || store == nil {
		return
	}
	registry.RegisterDescriptor(sendMessageDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		message := domain.AgentMessage{
			ID:            firstMessageArgument(call.Arguments["message_id"], nextAgentMessageID()),
			CompanyID:     call.Arguments["company_id"],
			TaskID:        call.Arguments["task_id"],
			SourceEventID: call.Arguments["source_event_id"],
			ThreadID:      firstMessageArgument(call.Arguments["thread_id"], call.Arguments["task_id"]),
			FromAgentID:   firstMessageArgument(call.Arguments["from"], call.Arguments["from_agent_id"]),
			ToAgentID:     firstMessageArgument(call.Arguments["to"], call.Arguments["to_agent_id"]),
			Type:          agentMessageType(call.Arguments["type"]),
			Status:        domain.AgentMessageUnread,
			Summary:       call.Arguments["summary"],
			Artifact:      call.Arguments["artifact"],
			CreatedAt:     time.Now().UTC(),
		}
		if message.FromAgentID == "" {
			message.FromAgentID = strings.TrimSpace(call.Arguments["actor_agent_id"])
		}
		if message.FromAgentID == "" {
			message.FromAgentID = "agent"
		}
		if err := store.SaveAgentMessage(ctx, message); err != nil {
			return domain.ToolResult{}, fmt.Errorf("send message: %w", err)
		}
		return messageToolResult(call.ID, fmt.Sprintf("sent message %s from %s to %s", message.ID, message.FromAgentID, message.ToAgentID)), nil
	}))
	registry.RegisterDescriptor(readMessagesDescriptor(), HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		query := domain.AgentMessageQuery{
			CompanyID:     call.Arguments["company_id"],
			TaskID:        call.Arguments["task_id"],
			ThreadID:      call.Arguments["thread_id"],
			FromAgentID:   firstMessageArgument(call.Arguments["from"], call.Arguments["from_agent_id"]),
			ToAgentID:     firstMessageArgument(call.Arguments["to"], call.Arguments["to_agent_id"]),
			Status:        domain.AgentMessageStatus(strings.TrimSpace(call.Arguments["status"])),
			SourceEventID: call.Arguments["source_event_id"],
			Limit:         parseMessageLimit(call.Arguments["limit"]),
		}
		messages, err := store.ListAgentMessages(ctx, query)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("read messages: %w", err)
		}
		if parseMessageBool(call.Arguments["mark_read"]) {
			for _, message := range messages {
				if message.Status == domain.AgentMessageUnread {
					if err := store.MarkAgentMessageRead(ctx, message.ID, time.Now().UTC()); err != nil {
						return domain.ToolResult{}, fmt.Errorf("mark message %q read: %w", message.ID, err)
					}
				}
			}
		}
		return messageToolResult(call.ID, renderAgentMessages(messages)), nil
	}))
}

func sendMessageDescriptor() Descriptor {
	return messageDescriptor("send_message", "Send an AgentMessage to another agent inbox.", "medium", true, // sends a message to another agent
		[]string{"to", "summary"}, map[string]any{
			"message_id":      messageString("Optional stable message id."),
			"company_id":      messageString("Company id. Defaults to runtime company when omitted by caller."),
			"task_id":         messageString("Related task id."),
			"thread_id":       messageString("Conversation or task thread id. Defaults to task_id."),
			"source_event_id": messageString("Optional TaskLedger event id for traceability."),
			"from":            messageString("Sender agent id."),
			"to":              messageString("Recipient agent id."),
			"type":            messageString("message, result, handoff, or review. Defaults to message."),
			"summary":         messageString("Short message body or summary."),
			"artifact":        messageString("Optional artifact path or URI."),
		})
}

func readMessagesDescriptor() Descriptor {
	return messageDescriptor("read_messages", "Read AgentMessage inbox/outbox messages.", "low", false,
		nil, map[string]any{
			"company_id":      messageString("Company id filter."),
			"task_id":         messageString("Task id filter."),
			"thread_id":       messageString("Thread id filter."),
			"from":            messageString("Sender agent id filter."),
			"to":              messageString("Recipient agent id filter."),
			"status":          messageString("Message status filter: unread or read."),
			"source_event_id": messageString("TaskLedger event id filter."),
			"limit":           messageString("Maximum messages to return."),
			"mark_read":       messageString("When true, mark returned unread messages as read."),
		})
}

func messageDescriptor(name, description, risk string, sensitive bool, required []string, properties map[string]any) Descriptor {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return Descriptor{
		Name:        name,
		Description: description,
		RiskLevel:   risk,
		Timeout:     5 * time.Second,
		InputSchema: schema,
		Sensitive:   sensitive,
	}
}

func messageString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func agentMessageType(kind string) domain.AgentMessageType {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "result":
		return domain.AgentMessageTypeResult
	case "handoff":
		return domain.AgentMessageTypeHandoff
	case "review":
		return domain.AgentMessageTypeReview
	default:
		return domain.AgentMessageTypeMessage
	}
}

func renderAgentMessages(messages []domain.AgentMessage) string {
	if len(messages) == 0 {
		return "no messages"
	}
	var b strings.Builder
	for _, message := range messages {
		b.WriteString(fmt.Sprintf("- `%s` `%s` `%s` %s -> %s: %s",
			message.ID,
			message.Type,
			message.Status,
			message.FromAgentID,
			message.ToAgentID,
			message.Summary,
		))
		if message.TaskID != "" {
			b.WriteString(" task=" + message.TaskID)
		}
		if message.Artifact != "" {
			b.WriteString(" artifact=" + message.Artifact)
		}
		if message.SourceEventID != "" {
			b.WriteString(" source_event=" + message.SourceEventID)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func messageToolResult(callID, output string) domain.ToolResult {
	return domain.ToolResult{CallID: callID, Success: true, Output: output}
}

func firstMessageArgument(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseMessageLimit(value string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || limit < 0 {
		return 0
	}
	return limit
}

func parseMessageBool(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func nextAgentMessageID() string {
	seq := atomic.AddUint64(&agentMessageToolIDCounter, 1)
	return fmt.Sprintf("msg-%s-%06d", time.Now().UTC().Format("20060102-150405"), seq)
}
