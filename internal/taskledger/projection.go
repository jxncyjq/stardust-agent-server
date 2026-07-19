package taskledger

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type Projection struct {
	Tasks         map[string]Task
	IndexMarkdown string
	TaskMarkdown  map[string]string
	Diagnostics   []string
}

func BuildProjection(events []Event, cfg Config) Projection {
	sortEvents(events)
	events = dedupeEvents(events)
	tasks := make(map[string]Task)
	var diagnostics []string
	for _, event := range events {
		task := tasks[event.TaskID]
		if task.ID == "" {
			task = Task{
				ID:           event.TaskID,
				Status:       "planned",
				Participants: make(map[string]bool),
				CreatedAt:    event.CreatedAt,
			}
		}
		if event.CreatedAt.Before(task.CreatedAt) {
			task.CreatedAt = event.CreatedAt
		}
		task.UpdatedAt = event.CreatedAt
		addParticipant(task.Participants, event.From)
		addParticipant(task.Participants, event.To)
		addParticipant(task.Participants, event.ActorAgentID)
		switch event.Type {
		case EventTaskCreated:
			if event.Title != "" {
				task.Title = event.Title
			}
			if event.Status != "" {
				task.Status = event.Status
			}
			if event.Owner != "" {
				task.Owner = event.Owner
			}
			if event.Summary != "" {
				task.Summary = event.Summary
			}
			if event.Artifact != "" {
				task.Artifact = event.Artifact
			}
		case EventTaskClaimed:
			if task.Owner != "" && task.Owner != event.ActorAgentID && task.Owner != event.Owner {
				conflict := event
				conflict.Type = "conflict.owner_claim"
				task.Conflicts = append(task.Conflicts, conflict)
				diagnostics = append(diagnostics, fmt.Sprintf("%s owner claim conflict from %s", event.TaskID, event.ActorAgentID))
				break
			}
			if event.Owner != "" {
				task.Owner = event.Owner
			} else {
				task.Owner = event.ActorAgentID
			}
		case EventTaskStatusChanged:
			if isTerminal(task.Status, cfg.DoneStatuses) && event.Status != "" && !isTerminal(event.Status, cfg.DoneStatuses) {
				conflict := event
				conflict.Type = "conflict.late_event"
				task.Conflicts = append(task.Conflicts, conflict)
				diagnostics = append(diagnostics, fmt.Sprintf("%s late status %s after terminal %s", event.TaskID, event.Status, task.Status))
				break
			}
			if event.Status != "" {
				task.Status = event.Status
			}
			if event.Summary != "" {
				task.Summary = event.Summary
			}
		case EventMessageAppended, EventResultAppended, EventHandoffAppended, EventReviewAppended:
			if event.Summary != "" {
				task.Summary = event.Summary
			}
			if event.Artifact != "" {
				task.Artifact = event.Artifact
			}
			task.Messages = append(task.Messages, event)
		default:
			if strings.HasPrefix(event.Type, EventConflictPrefix) {
				task.Conflicts = append(task.Conflicts, event)
			}
		}
		if task.Title == "" {
			task.Title = event.TaskID
		}
		tasks[event.TaskID] = task
	}
	return Projection{
		Tasks:         tasks,
		IndexMarkdown: renderIndex(tasks, cfg),
		TaskMarkdown:  renderTasks(tasks, cfg),
		Diagnostics:   diagnostics,
	}
}

func renderIndex(tasks map[string]Task, cfg Config) string {
	var b strings.Builder
	b.WriteString("# tasks\n\n")
	b.WriteString("本文件由 TaskLedger 根据 `tasks/events/*.jsonl` 生成，不手工并发改写。\n\n")
	b.WriteString("| ID | Status | Owner | Participants | Summary | Link |\n")
	b.WriteString("|----|--------|-------|--------------|---------|------|\n")
	for _, task := range sortedTasks(tasks) {
		if isTerminal(task.Status, cfg.DoneStatuses) {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | [[tasks/%s]] |\n",
			escapeCell(task.ID),
			escapeCell(task.Status),
			escapeCell(task.Owner),
			escapeCell(strings.Join(sortedParticipants(task.Participants), ", ")),
			escapeCell(task.Summary),
			escapeCell(task.ID),
		)
	}
	return enforceIndexLineLimit(b.String(), cfg.MaxIndexLines)
}

func renderTasks(tasks map[string]Task, cfg Config) map[string]string {
	rendered := make(map[string]string, len(tasks))
	for _, task := range sortedTasks(tasks) {
		var b strings.Builder
		b.WriteString("# ")
		b.WriteString(task.ID)
		b.WriteString("\n\n")
		b.WriteString("| Field | Value |\n")
		b.WriteString("|-------|-------|\n")
		fmt.Fprintf(&b, "| Title | %s |\n", escapeCell(task.Title))
		fmt.Fprintf(&b, "| Status | %s |\n", escapeCell(task.Status))
		fmt.Fprintf(&b, "| Owner | %s |\n", escapeCell(task.Owner))
		fmt.Fprintf(&b, "| Participants | %s |\n", escapeCell(strings.Join(sortedParticipants(task.Participants), ", ")))
		fmt.Fprintf(&b, "| Summary | %s |\n", escapeCell(task.Summary))
		if task.Artifact != "" {
			fmt.Fprintf(&b, "| Artifact | %s |\n", escapeCell(task.Artifact))
		}
		b.WriteString("\n## Messages\n\n")
		if len(task.Messages) == 0 {
			b.WriteString("- No messages yet.\n")
		}
		for _, message := range task.Messages {
			fmt.Fprintf(&b, "- `%s` `%s` %s -> %s: %s",
				message.CreatedAt.Format(time.RFC3339),
				message.Type,
				emptyDash(message.From),
				emptyDash(message.To),
				message.Summary,
			)
			if message.Artifact != "" {
				b.WriteString(" (")
				b.WriteString(message.Artifact)
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
		if len(task.Conflicts) > 0 {
			b.WriteString("\n## Conflicts\n\n")
			for _, conflict := range task.Conflicts {
				fmt.Fprintf(&b, "- `%s` `%s` %s: %s\n",
					conflict.CreatedAt.Format(time.RFC3339),
					conflict.Type,
					conflictMetadata(conflict),
					conflict.Summary,
				)
			}
		}
		rendered[task.ID] = enforceLineLimit(b.String(), cfg.MaxTaskLines)
	}
	return rendered
}

func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].EventID < events[j].EventID
		}
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
}

func dedupeEvents(events []Event) []Event {
	seenID := make(map[string]bool)
	seenKey := make(map[string]bool)
	deduped := make([]Event, 0, len(events))
	for _, event := range events {
		if event.EventID != "" {
			if seenID[event.EventID] {
				continue
			}
			seenID[event.EventID] = true
		}
		if event.IdempotencyKey != "" {
			if seenKey[event.IdempotencyKey] {
				continue
			}
			seenKey[event.IdempotencyKey] = true
		}
		deduped = append(deduped, event)
	}
	return deduped
}

func sortedTasks(tasks map[string]Task) []Task {
	values := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		values = append(values, task)
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].ID < values[j].ID
	})
	return values
}

func sortedParticipants(participants map[string]bool) []string {
	values := make([]string, 0, len(participants))
	for participant := range participants {
		if participant != "" {
			values = append(values, participant)
		}
	}
	sort.Strings(values)
	return values
}

func addParticipant(participants map[string]bool, participant string) {
	if participant != "" {
		participants[participant] = true
	}
}

func isTerminal(status string, doneStatuses []string) bool {
	return slices.Contains(doneStatuses, status)
}

func escapeCell(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	return value
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func enforceLineLimit(content string, maxLines int) string {
	if maxLines <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= maxLines {
		return content
	}
	kept := append([]string{}, lines[:maxLines]...)
	kept = append(kept, fmt.Sprintf("[truncated: projection exceeded %d lines]", maxLines))
	kept = append(kept, "建议拆分任务或归档长内容到 docs/ / memory/，TaskLedger 只保留摘要和链接。")
	return strings.Join(kept, "\n")
}

func enforceIndexLineLimit(content string, maxLines int) string {
	if maxLines <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= maxLines {
		return content
	}
	kept := append([]string{}, lines[:maxLines]...)
	kept = append(kept, fmt.Sprintf("[truncated: tasks.md exceeded %d lines]", maxLines))
	kept = append(kept, "建议归档 done/cancelled 任务或拆分活跃任务摘要，保持 tasks.md 只承载当前看板。")
	return strings.Join(kept, "\n")
}

func conflictMetadata(conflict Event) string {
	var parts []string
	if conflict.ActorAgentID != "" {
		parts = append(parts, "actor="+conflict.ActorAgentID)
	}
	if conflict.Owner != "" {
		parts = append(parts, "owner="+conflict.Owner)
	}
	if conflict.From != "" {
		parts = append(parts, "from="+conflict.From)
	}
	if conflict.To != "" {
		parts = append(parts, "to="+conflict.To)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}
