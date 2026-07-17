package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
)

type Model struct {
	result    app.DemoResult
	sanitizer port.OutputSanitizer
	done      bool
}

func NewModel(result app.DemoResult) Model {
	return NewModelWithSanitizer(result, quality.NewOutputSanitizer())
}

func NewModelWithSanitizer(result app.DemoResult, sanitizer port.OutputSanitizer) Model {
	return Model{result: result, sanitizer: sanitizer}
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return doneMsg{} }
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case doneMsg:
		m.done = true
		return m, nil
	case tea.KeyMsg:
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m Model) View() string {
	title := lipgloss.NewStyle().Bold(true).Render("Legion Agent Runtime")
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Task: %s\n", m.clean(m.result.TaskID)))
	b.WriteString(fmt.Sprintf("Result: %s\n\n", m.clean(m.result.Result)))
	b.WriteString("Event Stream:\n")
	for _, event := range m.result.EventStream {
		b.WriteString(fmt.Sprintf("- %s: %s\n", m.clean(event.Type), m.clean(event.Message)))
	}
	b.WriteString("\nAudit:\n")
	for _, action := range m.result.AuditActions {
		b.WriteString(fmt.Sprintf("- %s\n", m.clean(action)))
	}
	if m.done {
		b.WriteString("\nPress any key to exit.\n")
	}
	return b.String()
}

type doneMsg struct{}

func (m Model) clean(text string) string {
	if m.sanitizer == nil {
		return text
	}
	return m.sanitizer.MarkdownInline(text)
}
