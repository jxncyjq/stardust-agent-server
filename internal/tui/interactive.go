package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/quality"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/taskledger"
)

type Runner func(ctx context.Context, prompt string) (app.DemoResult, error)
type StreamingRunner func(ctx context.Context, prompt string, emit func(domain.RuntimeEvent)) (app.DemoResult, error)

var tuiAgentMessageIDCounter uint64

type InteractiveConfig struct {
	Runner          Runner
	StreamingRunner StreamingRunner
	Sanitizer       port.OutputSanitizer
	SkillManager    skill.Manager
	SkillManagers   map[string]skill.Manager
	SessionManager  SessionManager
	TaskLedger      *taskledger.Ledger
	MessageStore    AgentMessageStore
	AgentID         string
	AgentNames      []string
	AgentName       string
	ModelName       string
	HidePrompt      bool
	HideThinking    bool
	Renderer        *lipgloss.Renderer
	Theme           ThemeColors
}

type InteractiveModel struct {
	runner         Runner
	streamRunner   StreamingRunner
	sanitizer      port.OutputSanitizer
	skillManager   skill.Manager
	skillManagers  map[string]skill.Manager
	sessionManager SessionManager
	taskLedger     *taskledger.Ledger
	messageStore   AgentMessageStore
	renderer       *lipgloss.Renderer
	styles         interactiveStyles
	theme          ThemeColors
	input          string
	result         app.DemoResult
	skillMsg       string
	err            string
	running        bool
	quitting       bool
	width          int
	height         int
	viewMode       interactiveViewMode
	history        []string
	historyAt      int
	commandAt      int
	agentAt        int
	agentNames     []string
	agentName      string
	modelName      string
	activePrompt   string
	progressFrame  int
	showPrompt     bool
	showThinking   bool
	mainScroll     int
	streamCh       chan domain.RuntimeEvent
	liveEvents     []domain.RuntimeEvent
	copyNotice     string
	sessionID      string
	sessionMsg     string
	taskMsg        string
	messageMsg     string
	agentID        string
	turns          []conversationTurn
}

type interactiveViewMode string

const (
	interactiveViewResult  interactiveViewMode = "result"
	interactiveViewAudit   interactiveViewMode = "audit"
	interactiveViewEvent   interactiveViewMode = "event"
	interactiveViewSkill   interactiveViewMode = "skill"
	interactiveViewHistory interactiveViewMode = "history"
	interactiveViewSession interactiveViewMode = "session"
	interactiveViewTasks   interactiveViewMode = "tasks"
	interactiveViewTask    interactiveViewMode = "task"
	interactiveViewInbox   interactiveViewMode = "inbox"
)

type SessionManager interface {
	CurrentSessionID() string
	NewSession(context.Context) (string, error)
	ListSessions(context.Context) ([]string, error)
	SwitchSession(context.Context, string) error
	ClearSession(context.Context) error
}

type AgentMessageStore interface {
	SaveAgentMessage(context.Context, domain.AgentMessage) error
	ListAgentMessages(context.Context, domain.AgentMessageQuery) ([]domain.AgentMessage, error)
	MarkAgentMessageRead(context.Context, string, time.Time) error
}

// conversationTurn holds one complete prompt→reply exchange.
type conversationTurn struct {
	Prompt string
	Result string
	Err    string
}

const interactiveModelWaitingText = "正在和大模型通讯，等待输出..."

// interactiveClearCopyNoticeMsg clears the "已复制" notification after a brief delay.
type interactiveClearCopyNoticeMsg struct{}

type interactiveCommand struct {
	Name        string
	Description string
}

var interactiveCommands = []interactiveCommand{
	{Name: "/history", Description: "显示完整对话历史"},
	{Name: "/audit", Description: "显示审计动作"},
	{Name: "/event", Description: "显示事件流"},
	{Name: "/tasks", Description: "显示任务看板"},
	{Name: "/task ", Description: "显示任务详情 <task_id>"},
	{Name: "/handoff ", Description: "交接任务 <agent> <task_id> <summary>"},
	{Name: "/send ", Description: "发送消息 <agent> <message>"},
	{Name: "/inbox", Description: "显示未读消息"},
	{Name: "/new", Description: "创建新会话"},
	{Name: "/sessions", Description: "列出会话"},
	{Name: "/switch ", Description: "切换会话 <session_id>"},
	{Name: "/clear-session", Description: "清空当前会话"},
	{Name: "/skill install ", Description: "安装技能 <github:owner/repo | https://...>"},
	{Name: "/skill update ", Description: "更新技能 <name>"},
	{Name: "/skill uninstall ", Description: "卸载技能 <name>"},
}

// ThemeColors defines the color palette for the TUI.
type ThemeColors struct {
	Accent   string
	Accent2  string
	Text     string
	Dim      string
	Error    string
	StatusFg string
	StatusBg string
	ShellBg  string
}

type interactiveStyles struct {
	title lipgloss.Style
	panel lipgloss.Style
	label lipgloss.Style
	help  lipgloss.Style
	err   lipgloss.Style
	dim   lipgloss.Style
	shell lipgloss.Style
}

func defaultThemeColors() ThemeColors {
	return ThemeColors{
		Accent:   "39",
		Accent2:  "33",
		Text:     "250",
		Dim:      "245",
		Error:    "196",
		StatusFg: "230",
		StatusBg: "236",
		ShellBg:  "17",
	}
}

func buildInteractiveStyles(theme ThemeColors, renderer *lipgloss.Renderer) interactiveStyles {
	ns := lipgloss.NewStyle
	if renderer != nil {
		ns = renderer.NewStyle
	}
	return interactiveStyles{
		title: ns().Bold(true).Foreground(lipgloss.Color(theme.Accent)),
		panel: ns().Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(theme.Accent2)).
			Padding(0, 1).MarginBottom(1),
		label: ns().Bold(true).Foreground(lipgloss.Color(theme.Accent)),
		help:  ns().Foreground(lipgloss.Color(theme.Dim)),
		err:   ns().Foreground(lipgloss.Color(theme.Error)).Bold(true),
		dim:   ns().Foreground(lipgloss.Color(theme.Dim)),
		shell: ns().Background(lipgloss.Color(theme.ShellBg)).Foreground(lipgloss.Color(theme.Text)),
	}
}

func NewInteractiveModel(cfg InteractiveConfig) InteractiveModel {
	sanitizer := cfg.Sanitizer
	if sanitizer == nil {
		sanitizer = quality.NewOutputSanitizer()
	}
	agentName := strings.TrimSpace(cfg.AgentName)
	if agentName == "" {
		agentName = "agent"
	}
	modelName := strings.TrimSpace(cfg.ModelName)
	if modelName == "" {
		modelName = "model"
	}
	agentID := strings.TrimSpace(cfg.AgentID)
	if agentID == "" {
		agentID = "cli-agent"
	}
	theme := cfg.Theme
	if theme.Accent == "" {
		theme = defaultThemeColors()
	}
	return InteractiveModel{
		runner:         cfg.Runner,
		streamRunner:   cfg.StreamingRunner,
		sanitizer:      sanitizer,
		skillManager:   cfg.SkillManager,
		skillManagers:  copySkillManagers(cfg.SkillManagers),
		sessionManager: cfg.SessionManager,
		taskLedger:     cfg.TaskLedger,
		messageStore:   cfg.MessageStore,
		agentNames:     normalizedAgentNames(cfg.AgentNames),
		renderer:       cfg.Renderer,
		styles:         buildInteractiveStyles(theme, cfg.Renderer),
		theme:          theme,
		historyAt:      0,
		agentAt:        -1,
		agentName:      agentName,
		modelName:      modelName,
		sessionID:      currentSessionID(cfg.SessionManager),
		agentID:        agentID,
		showPrompt:     !cfg.HidePrompt,
		showThinking:   !cfg.HideThinking,
	}
}

func currentSessionID(manager SessionManager) string {
	if manager == nil {
		return ""
	}
	return strings.TrimSpace(manager.CurrentSessionID())
}

func normalizedAgentNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func (m InteractiveModel) Init() tea.Cmd {
	return nil
}

func (m InteractiveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case interactiveRunDoneMsg:
		m.running = false
		m.err = ""
		m.result = msg.result
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		// Record completed turn into conversation history.
		turn := conversationTurn{Prompt: m.activePrompt, Result: m.result.Result, Err: m.err}
		m.turns = append(m.turns, turn)
		// Scroll to bottom so the new result is visible.
		m.mainScroll = m.clampMainScroll(999999)
		return m, nil
	case interactiveSkillDoneMsg:
		m.running = false
		m.err = ""
		m.skillMsg = msg.output
		m.viewMode = interactiveViewSkill
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, nil
	case interactiveClearCopyNoticeMsg:
		m.copyNotice = ""
		return m, nil
	case interactiveProgressTickMsg:
		if !m.running {
			return m, nil
		}
		m.progressFrame++
		return m, interactiveProgressTick()
	case interactiveStreamEventMsg:
		m.liveEvents = append(m.liveEvents, domain.RuntimeEvent(msg))
		if m.streamCh != nil {
			return m, m.waitStream()
		}
		return m, nil
	case interactiveStreamClosedMsg:
		m.streamCh = nil
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.mainScroll = m.clampMainScroll(m.mainScroll)
		return m, nil
	case tea.MouseMsg:
		return m.updateMouse(msg), nil
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyCtrlY:
			text := m.currentCopyableText()
			if text != "" {
				if err := clipboard.WriteAll(text); err == nil {
					m.copyNotice = "已复制到剪贴板"
				} else {
					m.copyNotice = "复制失败: " + err.Error()
				}
			}
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return interactiveClearCopyNoticeMsg{}
			})
		case tea.KeyEnter:
			prompt := strings.TrimSpace(m.input)
			if prompt == "" || m.running {
				return m, nil
			}
			m = m.recordHistory(prompt)
			if strings.EqualFold(prompt, "/audit") {
				m.input = ""
				m.viewMode = interactiveViewAudit
				return m, nil
			}
			if strings.EqualFold(prompt, "/event") {
				m.input = ""
				m.viewMode = interactiveViewEvent
				return m, nil
			}
			if strings.EqualFold(prompt, "/history") {
				m.input = ""
				m.viewMode = interactiveViewHistory
				m.mainScroll = 0
				return m, nil
			}
			if handled, next := m.handleSessionCommand(context.Background(), prompt); handled {
				return next, nil
			}
			if handled, next := m.handleTaskCommand(context.Background(), prompt); handled {
				return next, nil
			}
			if handled, next := m.handleMessageCommand(context.Background(), prompt); handled {
				return next, nil
			}
			if fields := strings.Fields(prompt); len(fields) >= 1 && strings.EqualFold(fields[0], "/skill") {
				m.input = ""
				if len(fields) < 3 {
					m.skillMsg = "用法: /skill install <github:owner/repo | https://...>\n      /skill update <name>\n      /skill uninstall <name>"
					m.viewMode = interactiveViewSkill
					return m, nil
				}
				m.viewMode = interactiveViewSkill
				m.running = true
				m.progressFrame = 0
				return m, tea.Batch(m.runSkill(fields[1], strings.Join(fields[2:], " ")), interactiveProgressTick())
			}
			m.activePrompt = prompt
			m.input = ""
			m.viewMode = interactiveViewResult
			m.running = true
			m.progressFrame = 0
			m.mainScroll = 0
			m.liveEvents = nil
			m.streamCh = make(chan domain.RuntimeEvent, 256)
			return m, tea.Batch(m.run(prompt), m.waitStream(), interactiveProgressTick())
		case tea.KeyBackspace:
			if m.running {
				return m, nil
			}
			if len(m.input) > 0 {
				runes := []rune(m.input)
				m.input = string(runes[:len(runes)-1])
			}
			return m, nil
		case tea.KeyUp:
			if m.running {
				return m, nil
			}
			if m.isCommandSelecting() {
				m = m.previousCommand()
			} else if m.isAgentSelecting() {
				m = m.previousAgent()
			} else {
				m = m.previousHistory()
			}
			return m, nil
		case tea.KeyDown:
			if m.running {
				return m, nil
			}
			if m.isCommandSelecting() {
				m = m.nextCommand()
			} else if m.isAgentSelecting() {
				m = m.nextAgent()
			} else {
				m = m.nextHistory()
			}
			return m, nil
		case tea.KeyRunes:
			if string(msg.Runes) == "q" && strings.TrimSpace(m.input) == "" {
				m.quitting = true
				return m, tea.Quit
			}
			if m.running {
				return m, nil
			}
			m.input += string(msg.Runes)
			m.syncCommandSelection()
			m.syncAgentSelection()
			return m, nil
		default:
			return m, nil
		}
	default:
		return m, nil
	}
}

func (m InteractiveModel) updateMouse(msg tea.MouseMsg) InteractiveModel {
	if !m.mouseInMainArea(msg) {
		return m
	}
	switch msg.Type {
	case tea.MouseWheelUp:
		m.mainScroll = m.clampMainScroll(m.mainScroll - 3)
	case tea.MouseWheelDown:
		m.mainScroll = m.clampMainScroll(m.mainScroll + 3)
	}
	return m
}

func (m InteractiveModel) mouseInMainArea(msg tea.MouseMsg) bool {
	_, _, mainHeight := m.layoutDimensions()
	mainTop := 1
	mainBottom := mainTop + mainHeight
	return msg.Y >= mainTop && msg.Y < mainBottom
}

func (m InteractiveModel) isCommandSelecting() bool {
	return strings.HasPrefix(strings.TrimSpace(m.input), "/")
}

func (m InteractiveModel) isAgentSelecting() bool {
	input := strings.TrimSpace(m.input)
	return strings.HasPrefix(input, "@") && !strings.Contains(input, " ") && len(m.agentNames) > 0
}

func (m InteractiveModel) previousCommand() InteractiveModel {
	if len(interactiveCommands) == 0 {
		return m
	}
	if m.commandAt <= 0 {
		m.commandAt = len(interactiveCommands) - 1
	} else {
		m.commandAt--
	}
	m.input = interactiveCommands[m.commandAt].Name
	return m
}

func (m InteractiveModel) nextCommand() InteractiveModel {
	if len(interactiveCommands) == 0 {
		return m
	}
	m.commandAt = (m.commandAt + 1) % len(interactiveCommands)
	m.input = interactiveCommands[m.commandAt].Name
	return m
}

func (m InteractiveModel) previousAgent() InteractiveModel {
	if len(m.agentNames) == 0 {
		return m
	}
	if m.agentAt <= 0 {
		m.agentAt = len(m.agentNames) - 1
	} else {
		m.agentAt--
	}
	m.input = "@" + m.agentNames[m.agentAt] + " "
	return m
}

func (m InteractiveModel) nextAgent() InteractiveModel {
	if len(m.agentNames) == 0 {
		return m
	}
	m.agentAt = (m.agentAt + 1) % len(m.agentNames)
	m.input = "@" + m.agentNames[m.agentAt] + " "
	return m
}

func (m *InteractiveModel) syncCommandSelection() {
	input := strings.TrimSpace(m.input)
	for i, command := range interactiveCommands {
		name := command.Name
		if name == input {
			m.commandAt = i
			return
		}
		// Prefix commands (those with a trailing space) match when the user has
		// started typing an argument after the command keyword.
		if strings.HasSuffix(name, " ") && strings.HasPrefix(input, strings.TrimRight(name, " ")) {
			m.commandAt = i
			return
		}
	}
	if input == "/" {
		m.commandAt = 0
	}
}

func (m *InteractiveModel) syncAgentSelection() {
	input := strings.TrimSpace(m.input)
	if !strings.HasPrefix(input, "@") || strings.Contains(input, " ") {
		return
	}
	typed := strings.TrimPrefix(input, "@")
	if typed == "" {
		m.agentAt = -1
		return
	}
	for i, name := range m.agentNames {
		if strings.HasPrefix(name, typed) {
			m.agentAt = i
			return
		}
	}
}

func (m InteractiveModel) recordHistory(input string) InteractiveModel {
	if input == "" {
		return m
	}
	m.history = append(m.history, input)
	m.historyAt = len(m.history)
	return m
}

func (m InteractiveModel) previousHistory() InteractiveModel {
	if len(m.history) == 0 {
		return m
	}
	if m.historyAt > 0 {
		m.historyAt--
	}
	m.input = m.history[m.historyAt]
	return m
}

func (m InteractiveModel) nextHistory() InteractiveModel {
	if len(m.history) == 0 {
		return m
	}
	if m.historyAt < len(m.history)-1 {
		m.historyAt++
		m.input = m.history[m.historyAt]
		return m
	}
	m.historyAt = len(m.history)
	m.input = ""
	return m
}

func (m InteractiveModel) View() string {
	width, planWidth, mainHeight := m.layoutDimensions()
	mainWidth := width - planWidth - 2
	if mainWidth < 40 {
		mainWidth = 40
	}

	var b strings.Builder
	b.WriteString(m.renderHeader(width))
	b.WriteString("\n")
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, m.renderMain(mainWidth, mainHeight), " ", m.renderPlan(planWidth, mainHeight)))
	b.WriteString("\n")
	b.WriteString(m.renderComposer(width))
	b.WriteString("\n")
	b.WriteString(m.renderFooter(width))
	b.WriteString("\n")
	return m.styles.shell.Width(width).Height(m.normalizedHeight()).Render(b.String())
}

func (m InteractiveModel) layoutDimensions() (int, int, int) {
	width := m.width
	if width < 80 {
		width = 100
	}
	planWidth := 42
	if width < 100 {
		planWidth = 38
	}
	mainHeight := m.normalizedHeight() - 8
	if mainHeight < 10 {
		mainHeight = 10
	}
	return width, planWidth, mainHeight
}

func (m InteractiveModel) normalizedHeight() int {
	height := m.height
	if height < 24 {
		return 30
	}
	return height
}

func (m InteractiveModel) newStyle() lipgloss.Style {
	if m.renderer != nil {
		return m.renderer.NewStyle()
	}
	return lipgloss.NewStyle()
}

func (m InteractiveModel) statusText() string {
	status := "Ready"
	if m.running {
		status = "Running..."
	}
	if m.err != "" {
		status = "Error"
	}
	return status
}

func (m InteractiveModel) renderHeader(width int) string {
	left := m.styles.title.Render("Agent") + "  " + m.clean(m.agentName) + " · " + m.clean(m.modelName)
	right := m.styles.title.Render("max") + "  0%"
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m InteractiveModel) renderMain(width int, height int) string {
	body := m.renderMainBody(width, height)
	body = scrollTextBlock(body, height, m.mainScroll)
	return m.newStyle().
		Width(width).
		Height(height).
		Background(lipgloss.Color(m.theme.ShellBg)).
		Render(body)
}

func (m InteractiveModel) renderMainBody(width int, height int) string {
	var parts []string
	if m.err != "" {
		parts = append(parts, m.styles.err.Render("ERROR")+" "+m.clean(m.err))
	}
	if !m.running && m.result.TaskID == "" && m.result.Result == "" && m.err == "" && m.skillMsg == "" && m.sessionMsg == "" && m.taskMsg == "" && m.messageMsg == "" {
		topPad := height / 3
		if topPad < 2 {
			topPad = 2
		}
		title := centerLine(width, m.styles.title.Render("Legion Agent TUI"))
		subtitle := centerLine(width, m.styles.title.Render(m.clean(m.agentName)+"  ·  "+m.clean(m.modelName)))
		parts = append(parts,
			strings.Repeat("\n", topPad)+
				title+"\n"+
				subtitle,
		)
	} else {
		if m.viewMode == interactiveViewAudit {
			parts = append(parts, m.styles.label.Render("AUDIT")+"\n"+m.renderAudit())
		} else if m.viewMode == interactiveViewEvent {
			parts = append(parts, m.styles.label.Render("EVENT")+"\n"+m.renderEvents())
		} else if m.viewMode == interactiveViewSkill {
			parts = append(parts, m.styles.label.Render("SKILL")+"\n"+m.clean(m.skillMsg))
		} else if m.viewMode == interactiveViewHistory {
			parts = append(parts, m.styles.label.Render("HISTORY")+"\n"+m.renderHistory(width))
		} else if m.viewMode == interactiveViewSession {
			parts = append(parts, m.styles.label.Render("SESSION")+"\n"+m.clean(m.sessionMsg))
		} else if m.viewMode == interactiveViewTasks {
			parts = append(parts, m.styles.label.Render("TASKS")+"\n"+m.formatMarkdownBlock(m.taskMsg, width))
		} else if m.viewMode == interactiveViewTask {
			parts = append(parts, m.styles.label.Render("TASK")+"\n"+m.formatMarkdownBlock(m.taskMsg, width))
		} else if m.viewMode == interactiveViewInbox {
			parts = append(parts, m.styles.label.Render("INBOX")+"\n"+m.formatMarkdownBlock(m.messageMsg, width))
		} else {
			parts = append(parts,
				m.renderConversation(width),
			)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m InteractiveModel) clampMainScroll(offset int) int {
	_, planWidth, mainHeight := m.layoutDimensions()
	mainWidth := m.width - planWidth - 2
	if mainWidth < 40 {
		mainWidth = 40
	}
	body := m.renderMainBody(mainWidth, mainHeight)
	maxOffset := max(0, len(strings.Split(body, "\n"))-mainHeight)
	if offset < 0 {
		return 0
	}
	if offset > maxOffset {
		return maxOffset
	}
	return offset
}

func scrollTextBlock(text string, height int, offset int) string {
	lines := strings.Split(text, "\n")
	if height <= 0 || len(lines) <= height {
		return text
	}
	maxOffset := len(lines) - height
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	return strings.Join(lines[offset:offset+height], "\n")
}

func (m InteractiveModel) renderPlan(width int, height int) string {
	body := m.styles.label.Render("Plan") + "\n" +
		m.styles.dim.Italic(true).Render("tracks update_plan / /goal / cycles")
	return m.newStyle().
		Width(frameContentWidth(width)).
		Height(height).
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(m.theme.Accent2)).
		Padding(0, 1).
		Render(body)
}

func (m InteractiveModel) renderComposer(width int) string {
	text := m.clean(m.input)
	if m.running {
		text = m.styles.title.Render(interactiveModelWaitingText)
	} else if strings.TrimSpace(text) == "" {
		text = m.styles.dim.Italic(true).Render("编写任务或使用 /。")
	} else {
		text = "> " + text
	}
	body := "Composer\n" + text
	if !m.running && strings.HasPrefix(strings.TrimSpace(m.input), "/") {
		body += "\n" + m.renderCommandSuggestions()
	} else if !m.running && strings.HasPrefix(strings.TrimSpace(m.input), "@") {
		body += "\n" + m.renderAgentSuggestions()
	}
	return m.newStyle().
		Width(width - 2).
		Height(4).
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(m.theme.Accent2)).
		Render(body)
}

func (m InteractiveModel) renderAgentSuggestions() string {
	if len(m.agentNames) == 0 {
		return m.styles.dim.Render("  未配置可选 Agent")
	}
	parts := make([]string, 0, len(m.agentNames))
	for i, name := range m.agentNames {
		label := "@" + name
		if i == m.agentAt {
			parts = append(parts, m.styles.title.Render("> "+label))
		} else {
			parts = append(parts, m.styles.dim.Render("  "+label))
		}
	}
	return strings.Join(parts, "   ")
}

func (m InteractiveModel) renderCommandSuggestions() string {
	parts := make([]string, 0, len(interactiveCommands))
	for i, command := range interactiveCommands {
		label := command.Name + " - " + command.Description
		if i == m.commandAt {
			parts = append(parts, m.styles.title.Render("> "+label))
		} else {
			parts = append(parts, m.styles.dim.Render("  "+label))
		}
	}
	return strings.Join(parts, "   ")
}

func frameContentWidth(outerWidth int) int {
	contentWidth := outerWidth - 2
	if contentWidth < 1 {
		return 1
	}
	return contentWidth
}

func centerLine(width int, text string) string {
	textWidth := lipgloss.Width(text)
	if textWidth >= width {
		return text
	}
	return strings.Repeat(" ", (width-textWidth)/2) + text
}

func (m InteractiveModel) renderFooter(width int) string {
	left := m.styles.title.Render(m.clean(m.agentName)) + m.styles.dim.Render(" · "+m.clean(m.modelName))
	if m.sessionID != "" {
		left += m.styles.dim.Render(" · " + m.clean(m.sessionID))
	}
	if m.copyNotice != "" {
		notice := m.styles.title.Render(" ✓ " + m.copyNotice)
		gap := width - lipgloss.Width(left) - lipgloss.Width(notice)
		if gap < 1 {
			gap = 1
		}
		return left + strings.Repeat(" ", gap) + notice
	}
	if m.running {
		status := m.styles.title.Render(" · 工作中 ...")
		prefix := left + status + " "
		barWidth := width - lipgloss.Width(prefix)
		if barWidth < 4 {
			return prefix
		}
		return prefix + m.renderWorkingProgressBar(barWidth, m.progressFrame)
	}
	right := m.styles.help.Render("Enter Run | Ctrl+Y 复制 | Shift+拖拽 选择 | Esc/Ctrl+C Quit")
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m InteractiveModel) handleSessionCommand(ctx context.Context, prompt string) (bool, InteractiveModel) {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false, m
	}
	switch strings.ToLower(fields[0]) {
	case "/new", "/sessions", "/switch", "/clear-session":
	default:
		return false, m
	}
	m.input = ""
	m.viewMode = interactiveViewSession
	if m.sessionManager == nil {
		m.err = "session manager unavailable"
		m.sessionMsg = ""
		return true, m
	}
	switch strings.ToLower(fields[0]) {
	case "/new":
		id, err := m.sessionManager.NewSession(ctx)
		if err != nil {
			m.err = err.Error()
			return true, m
		}
		m.err = ""
		m.sessionID = id
		m.turns = nil
		m.sessionMsg = "created " + id
	case "/sessions":
		ids, err := m.sessionManager.ListSessions(ctx)
		if err != nil {
			m.err = err.Error()
			return true, m
		}
		m.err = ""
		if len(ids) == 0 {
			m.sessionMsg = "no sessions"
		} else {
			m.sessionMsg = strings.Join(ids, "\n")
		}
	case "/switch":
		if len(fields) < 2 {
			m.err = "usage: /switch <session_id>"
			return true, m
		}
		if err := m.sessionManager.SwitchSession(ctx, fields[1]); err != nil {
			m.err = err.Error()
			return true, m
		}
		m.err = ""
		m.sessionID = fields[1]
		m.turns = nil
		m.sessionMsg = "switched to " + fields[1]
	case "/clear-session":
		if err := m.sessionManager.ClearSession(ctx); err != nil {
			m.err = err.Error()
			return true, m
		}
		m.err = ""
		m.sessionID = currentSessionID(m.sessionManager)
		m.turns = nil
		m.sessionMsg = "session cleared"
	}
	return true, m
}

func (m InteractiveModel) handleTaskCommand(ctx context.Context, prompt string) (bool, InteractiveModel) {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false, m
	}
	command := strings.ToLower(fields[0])
	switch command {
	case "/tasks", "/task", "/handoff":
	default:
		return false, m
	}
	m.input = ""
	if m.taskLedger == nil {
		m.err = "task ledger unavailable"
		m.taskMsg = ""
		m.viewMode = interactiveViewTasks
		return true, m
	}
	switch command {
	case "/tasks":
		projection, err := m.taskLedger.Snapshot(ctx)
		if err != nil {
			m.err = err.Error()
			return true, m
		}
		m.err = ""
		m.taskMsg = projection.IndexMarkdown
		m.viewMode = interactiveViewTasks
	case "/task":
		if len(fields) < 2 {
			m.err = "usage: /task <task_id>"
			m.viewMode = interactiveViewTask
			return true, m
		}
		return m.showTask(ctx, fields[1])
	case "/handoff":
		if len(fields) < 4 {
			m.err = "usage: /handoff <agent> <task_id> <summary>"
			m.viewMode = interactiveViewTask
			return true, m
		}
		to := fields[1]
		taskID := fields[2]
		summary := strings.TrimSpace(strings.TrimPrefix(prompt, strings.Join(fields[:3], " ")))
		if _, err := m.taskLedger.Append(ctx, taskledger.Event{
			TaskID:        taskID,
			Type:          taskledger.EventHandoffAppended,
			From:          m.agentID,
			To:            to,
			ActorAgentID:  m.agentID,
			Summary:       summary,
			CorrelationID: "tui-handoff-" + taskID,
		}); err != nil {
			m.err = err.Error()
			m.viewMode = interactiveViewTask
			return true, m
		}
		if _, err := m.taskLedger.Rebuild(ctx); err != nil {
			m.err = err.Error()
			m.viewMode = interactiveViewTask
			return true, m
		}
		return m.showTask(ctx, taskID)
	}
	return true, m
}

func (m InteractiveModel) showTask(ctx context.Context, taskID string) (bool, InteractiveModel) {
	projection, err := m.taskLedger.Snapshot(ctx)
	if err != nil {
		m.err = err.Error()
		m.viewMode = interactiveViewTask
		return true, m
	}
	detail, ok := projection.TaskMarkdown[taskID]
	if !ok {
		m.err = "task not found: " + taskID
		m.taskMsg = ""
		m.viewMode = interactiveViewTask
		return true, m
	}
	m.err = ""
	m.taskMsg = detail
	m.viewMode = interactiveViewTask
	return true, m
}

func (m InteractiveModel) handleMessageCommand(ctx context.Context, prompt string) (bool, InteractiveModel) {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false, m
	}
	command := strings.ToLower(fields[0])
	switch command {
	case "/send", "/inbox":
	default:
		return false, m
	}
	m.input = ""
	m.viewMode = interactiveViewInbox
	if m.messageStore == nil {
		m.err = "message store unavailable"
		m.messageMsg = ""
		return true, m
	}
	switch command {
	case "/send":
		if len(fields) < 3 {
			m.err = "usage: /send <agent> <message>"
			return true, m
		}
		to := strings.TrimSpace(fields[1])
		if !m.isKnownAgent(to) {
			m.err = "unknown agent: " + to
			return true, m
		}
		summary := strings.TrimSpace(strings.TrimPrefix(prompt, strings.Join(fields[:2], " ")))
		message := domain.AgentMessage{
			ID:          nextTUIAgentMessageID(),
			FromAgentID: m.agentID,
			ToAgentID:   to,
			Type:        domain.AgentMessageTypeMessage,
			Status:      domain.AgentMessageUnread,
			Summary:     summary,
			CreatedAt:   time.Now().UTC(),
		}
		if err := m.messageStore.SaveAgentMessage(ctx, message); err != nil {
			m.err = fmt.Errorf("send message: %w", err).Error()
			return true, m
		}
		m.err = ""
		m.messageMsg = fmt.Sprintf("sent `%s` %s -> %s: %s", message.ID, message.FromAgentID, message.ToAgentID, message.Summary)
	case "/inbox":
		status := domain.AgentMessageUnread
		if len(fields) > 1 && strings.EqualFold(fields[1], "--all") {
			status = ""
		}
		messages, err := m.messageStore.ListAgentMessages(ctx, domain.AgentMessageQuery{
			ToAgentID: m.agentID,
			Status:    status,
			Limit:     50,
		})
		if err != nil {
			m.err = fmt.Errorf("read inbox: %w", err).Error()
			return true, m
		}
		m.err = ""
		m.messageMsg = renderInteractiveMessages(messages)
	}
	return true, m
}

func nextTUIAgentMessageID() string {
	seq := atomic.AddUint64(&tuiAgentMessageIDCounter, 1)
	return fmt.Sprintf("tui-msg-%s-%06d", time.Now().UTC().Format("20060102-150405"), seq)
}

func (m InteractiveModel) isKnownAgent(agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	if len(m.agentNames) == 0 {
		return true
	}
	for _, name := range m.agentNames {
		if name == agentID {
			return true
		}
	}
	return false
}

func renderInteractiveMessages(messages []domain.AgentMessage) string {
	if len(messages) == 0 {
		return "no unread messages"
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
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m InteractiveModel) renderWorkingProgressBar(width int, frame int) string {
	if width <= 0 {
		return ""
	}
	if width < 4 {
		return m.styles.title.Render(strings.Repeat("─", width))
	}
	marker := "╯╰"
	markerWidth := lipgloss.Width(marker)
	span := width - markerWidth + 1
	if span < 1 {
		span = 1
	}
	leftWidth := frame % span
	rightWidth := width - markerWidth - leftWidth
	bar := strings.Repeat("─", leftWidth) + marker + strings.Repeat("─", rightWidth)
	return m.styles.title.Render(bar)
}

func (m InteractiveModel) renderPanel(label string, body string) string {
	if strings.TrimSpace(body) == "" {
		body = "No data yet."
	}
	content := m.styles.label.Render(label) + "\n" + body
	return m.styles.panel.Render(content) + "\n"
}

func (m InteractiveModel) renderResult(width int) string {
	if m.result.TaskID == "" && m.result.Result == "" {
		return "No result yet."
	}
	bodyWidth := width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	lines := []string{
		m.styles.dim.Render("Task " + m.clean(m.result.TaskID)),
		"",
		m.styles.label.Render("Output"),
		m.formatMarkdownBlock(m.result.Result, bodyWidth),
	}
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderConversation(width int) string {
	bodyWidth := width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	sep := m.styles.dim.Render(strings.Repeat("─", min(bodyWidth, 60)))
	var lines []string

	// Turns before the most recent are rendered compactly (no thinking block).
	if len(m.turns) > 1 {
		for _, t := range m.turns[:len(m.turns)-1] {
			if m.showPrompt && strings.TrimSpace(t.Prompt) != "" {
				lines = append(lines, m.styles.title.Render("▌")+" "+m.clean(t.Prompt), "")
			}
			if t.Result != "" {
				lines = append(lines, prefixFirstLine("● ", m.formatMarkdownBlock(t.Result, bodyWidth)))
			}
			if t.Err != "" {
				lines = append(lines, m.styles.err.Render("● "+m.clean(t.Err)))
			}
			lines = append(lines, "", sep, "")
		}
	}

	// Current context: the in-flight run, the most recently completed turn
	// (with thinking block), or a result set directly on m.result (tests / initial state).
	if m.running {
		prompt := strings.TrimSpace(m.activePrompt)
		if m.showPrompt && prompt != "" {
			lines = append(lines, m.styles.title.Render("▌")+" "+m.clean(prompt), "")
		}
		if m.showThinking {
			lines = append(lines, m.renderThinkingBlock(bodyWidth))
		}
	} else {
		// Resolve the most-recently-completed result.  Prefer the last turn (ensures
		// separator between prior turns and current), fall back to m.result directly
		// (backwards-compatible with tests and cases where turns is still empty).
		var prompt, result, errText string
		if len(m.turns) > 0 {
			last := m.turns[len(m.turns)-1]
			prompt = last.Prompt
			result = last.Result
			errText = last.Err
		} else {
			prompt = m.activePrompt
			result = m.result.Result
			errText = m.err
		}
		if result != "" || errText != "" {
			if m.showPrompt && strings.TrimSpace(prompt) != "" {
				lines = append(lines, m.styles.title.Render("▌")+" "+m.clean(prompt), "")
			}
			if m.showThinking {
				lines = append(lines, m.renderThinkingBlock(bodyWidth))
			}
			if result != "" {
				lines = append(lines, "")
				lines = append(lines, prefixFirstLine("● ", m.formatMarkdownBlock(result, bodyWidth)))
			}
			if errText != "" {
				lines = append(lines, "")
				lines = append(lines, m.styles.err.Render("● "+m.clean(errText)))
			}
		}
	}

	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderThinkingBlock(width int) string {
	lines := []string{m.renderThinkingLine()}
	if len(m.liveEvents) > 0 {
		for _, event := range m.liveEvents {
			lines = append(lines, m.renderLiveEvent(event, width)...)
		}
		return strings.Join(lines, "\n")
	}
	if strings.TrimSpace(m.result.ReasoningSummary) != "" {
		reasoning := m.formatMarkdownBlock(m.result.ReasoningSummary, width-2)
		for _, line := range strings.Split(reasoning, "\n") {
			if strings.TrimSpace(line) == "" {
				lines = append(lines, m.styles.dim.Render("│"))
				continue
			}
			lines = append(lines, m.styles.dim.Italic(true).Render("│ "+line))
		}
		return strings.Join(lines, "\n")
	}
	for _, step := range m.thinkingSteps() {
		lines = append(lines, m.styles.dim.Italic(true).Render("│ "+step))
	}
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderLiveEvent(event domain.RuntimeEvent, width int) []string {
	eventType := strings.TrimSpace(m.clean(event.Type))
	message := strings.TrimSpace(m.cleanBlock(event.Message))
	if eventType == "" && message == "" {
		return nil
	}
	if eventType == "tool_result" {
		lines := []string{m.styles.dim.Italic(true).Render("│ tool_result")}
		for _, line := range strings.Split(m.formatMarkdownBlock(message, width-2), "\n") {
			if strings.TrimSpace(line) == "" {
				lines = append(lines, m.styles.dim.Render("│"))
				continue
			}
			lines = append(lines, m.styles.dim.Italic(true).Render("│ "+line))
		}
		return lines
	}
	if message == "" {
		return []string{m.styles.dim.Italic(true).Render("│ event: " + eventType)}
	}
	// Take only the first non-empty line of the message as a brief summary.
	// Multi-line messages (e.g. task_completed carrying the full result) should
	// not be re-displayed in full here — the result section already shows them.
	summary := firstNonEmptyLine(message)
	if summary != message {
		summary = stripMarkdownMarkers(stripInlineItalic(summary))
		if len([]rune(summary)) > 80 {
			summary = string([]rune(summary)[:80]) + "…"
		}
		return []string{m.styles.dim.Italic(true).Render("│ event: " + eventType + " · " + summary + " …")}
	}
	summary = stripMarkdownMarkers(stripInlineItalic(summary))
	return []string{m.styles.dim.Italic(true).Render("│ event: " + eventType + " · " + summary)}
}

// firstNonEmptyLine returns the first non-empty line of text.
func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return text
}

func (m InteractiveModel) renderThinkingLine() string {
	if m.running {
		return m.styles.dim.Render("… thinking ... · preparing / calling / waiting")
	}
	elapsed := float64(m.progressFrame) * 0.12
	if elapsed <= 0 {
		return m.styles.dim.Render("… thinking done")
	}
	return m.styles.dim.Render(fmt.Sprintf("… thinking done · %.1fs", elapsed))
}

func (m InteractiveModel) thinkingSteps() []string {
	steps := make([]string, 0, 6)
	if strings.TrimSpace(m.activePrompt) != "" {
		steps = append(steps, "received user prompt")
	}
	steps = append(steps, "prepared Agent context")
	if m.running {
		steps = append(steps, "calling model through C70 MaaS port")
		steps = append(steps, "waiting for model output")
		return steps
	}
	if len(m.result.EventStream) > 0 {
		limit := len(m.result.EventStream)
		if limit > 3 {
			limit = 3
		}
		for _, event := range m.result.EventStream[:limit] {
			eventType := strings.TrimSpace(m.clean(event.Type))
			message := strings.TrimSpace(m.clean(event.Message))
			if eventType == "" && message == "" {
				continue
			}
			if eventType == "" {
				steps = append(steps, "event: "+message)
				continue
			}
			if message == "" {
				steps = append(steps, "event: "+eventType)
				continue
			}
			steps = append(steps, "event: "+eventType+" · "+message)
		}
		if len(m.result.EventStream) > limit {
			steps = append(steps, fmt.Sprintf("event: +%d more", len(m.result.EventStream)-limit))
		}
	} else if m.result.Result != "" {
		steps = append(steps, "model output received")
	}
	if m.err != "" {
		steps = append(steps, "run failed before successful output")
	}
	return steps
}

func prefixFirstLine(prefix string, text string) string {
	if text == "" {
		return prefix
	}
	lines := strings.Split(text, "\n")
	lines[0] = prefix + lines[0]
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderEvents() string {
	if len(m.result.EventStream) == 0 {
		return "No events yet."
	}
	lines := make([]string, 0, len(m.result.EventStream))
	for _, event := range m.result.EventStream {
		lines = append(lines, fmt.Sprintf("- %s: %s", m.clean(event.Type), m.clean(event.Message)))
	}
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderHistory(width int) string {
	if len(m.turns) == 0 {
		return m.styles.dim.Italic(true).Render("暂无对话历史。")
	}
	bodyWidth := width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	var lines []string
	for i, t := range m.turns {
		lines = append(lines, m.styles.title.Render(fmt.Sprintf("▌ [%d] %s", i+1, m.clean(t.Prompt))))
		lines = append(lines, "")
		if t.Result != "" {
			lines = append(lines, prefixFirstLine("● ", m.formatMarkdownBlock(t.Result, bodyWidth)))
		}
		if t.Err != "" {
			lines = append(lines, m.styles.err.Render("● "+m.clean(t.Err)))
		}
		if i < len(m.turns)-1 {
			lines = append(lines, m.styles.dim.Render(strings.Repeat("─", min(bodyWidth, 40))))
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) renderAudit() string {
	if len(m.result.AuditActions) == 0 {
		return "No audit actions yet."
	}
	lines := make([]string, 0, len(m.result.AuditActions))
	for _, action := range m.result.AuditActions {
		lines = append(lines, "- "+m.clean(action))
	}
	return strings.Join(lines, "\n")
}

type interactiveRunDoneMsg struct {
	result app.DemoResult
	err    error
}

type interactiveSkillDoneMsg struct {
	output string
	err    error
}

type interactiveProgressTickMsg struct{}
type interactiveStreamEventMsg domain.RuntimeEvent
type interactiveStreamClosedMsg struct{}

func interactiveProgressTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return interactiveProgressTickMsg{}
	})
}

func (m InteractiveModel) run(prompt string) tea.Cmd {
	streamCh := m.streamCh
	return func() tea.Msg {
		if streamCh != nil {
			defer close(streamCh)
		}
		if m.streamRunner != nil {
			emit := func(event domain.RuntimeEvent) {
				if streamCh == nil {
					return
				}
				streamCh <- event
			}
			result, err := m.streamRunner(context.Background(), prompt, emit)
			return interactiveRunDoneMsg{result: result, err: err}
		}
		if m.runner == nil {
			return interactiveRunDoneMsg{err: fmt.Errorf("interactive runner is not configured")}
		}
		result, err := m.runner(context.Background(), prompt)
		return interactiveRunDoneMsg{result: result, err: err}
	}
}

func (m InteractiveModel) waitStream() tea.Cmd {
	streamCh := m.streamCh
	return func() tea.Msg {
		if streamCh == nil {
			return interactiveStreamClosedMsg{}
		}
		event, ok := <-streamCh
		if !ok {
			return interactiveStreamClosedMsg{}
		}
		return interactiveStreamEventMsg(event)
	}
}

func (m InteractiveModel) runSkill(sub, arg string) tea.Cmd {
	agentName, cleanArg := parseSkillAgentArg(arg)
	mgr := m.skillManager
	if agentName != "" {
		mgr = m.skillManagers[agentName]
	}
	return func() tea.Msg {
		if mgr == nil {
			if agentName != "" {
				return interactiveSkillDoneMsg{err: fmt.Errorf("skill manager not configured for agent %q", agentName)}
			}
			return interactiveSkillDoneMsg{err: fmt.Errorf("skill manager not configured; start the agent with a skills install root")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		switch strings.ToLower(sub) {
		case "install":
			if cleanArg == "" {
				return interactiveSkillDoneMsg{err: fmt.Errorf("install requires a source (github:owner/repo or https://...)")}
			}
			sk, err := mgr.Install(ctx, cleanArg)
			if err != nil {
				return interactiveSkillDoneMsg{err: fmt.Errorf("install %q: %w", cleanArg, err)}
			}
			return interactiveSkillDoneMsg{output: fmt.Sprintf("✓ 已安装: %s  (%s)", sk.Name, sk.Version)}
		case "update":
			if cleanArg == "" {
				return interactiveSkillDoneMsg{err: fmt.Errorf("update requires a skill name")}
			}
			sk, err := mgr.Update(ctx, cleanArg)
			if err != nil {
				return interactiveSkillDoneMsg{err: fmt.Errorf("update %q: %w", cleanArg, err)}
			}
			return interactiveSkillDoneMsg{output: fmt.Sprintf("✓ 已更新: %s  (%s)", sk.Name, sk.Version)}
		case "uninstall":
			if cleanArg == "" {
				return interactiveSkillDoneMsg{err: fmt.Errorf("uninstall requires a skill name")}
			}
			if err := mgr.Uninstall(ctx, cleanArg); err != nil {
				return interactiveSkillDoneMsg{err: fmt.Errorf("uninstall %q: %w", cleanArg, err)}
			}
			return interactiveSkillDoneMsg{output: fmt.Sprintf("✓ 已卸载: %s", cleanArg)}
		default:
			return interactiveSkillDoneMsg{err: fmt.Errorf("unknown subcommand %q; use install, update, or uninstall", sub)}
		}
	}
}

func copySkillManagers(in map[string]skill.Manager) map[string]skill.Manager {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]skill.Manager, len(in))
	for name, manager := range in {
		out[name] = manager
	}
	return out
}

func parseSkillAgentArg(arg string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(arg))
	if len(fields) == 0 {
		return "", ""
	}
	cleaned := make([]string, 0, len(fields))
	agentName := ""
	for idx := 0; idx < len(fields); idx++ {
		field := fields[idx]
		if field == "--agent" && idx+1 < len(fields) {
			agentName = fields[idx+1]
			idx++
			continue
		}
		if strings.HasPrefix(field, "--agent=") {
			agentName = strings.TrimPrefix(field, "--agent=")
			continue
		}
		cleaned = append(cleaned, field)
	}
	return agentName, strings.Join(cleaned, " ")
}

func (m InteractiveModel) clean(text string) string {
	if m.sanitizer == nil {
		return text
	}
	return m.sanitizer.MarkdownInline(text)
}

func (m InteractiveModel) cleanBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = m.clean(line)
	}
	return strings.Join(lines, "\n")
}

func (m InteractiveModel) formatMarkdownBlock(text string, width int) string {
	rawLines := normalizeMarkdownLines(strings.Split(text, "\n"))
	out := make([]string, 0, len(rawLines))
	i := 0
	for i < len(rawLines) {
		line := rawLines[i]
		trimmedRaw := strings.TrimSpace(line)

		// Horizontal rule: ---, ***, ___ (must check before stripMarkdownMarkers which would eat ***)
		if isHorizontalRule(trimmedRaw) {
			ruleWidth := width
			if ruleWidth > 60 {
				ruleWidth = 60
			}
			out = append(out, m.styles.dim.Render(strings.Repeat("─", ruleWidth)))
			i++
			continue
		}

		// Heading: # Title, ## Subtitle, ### etc.
		if strings.HasPrefix(trimmedRaw, "#") {
			content := strings.TrimLeft(trimmedRaw, "#")
			content = stripMarkdownMarkers(stripInlineItalic(m.clean(strings.TrimSpace(content))))
			if content != "" {
				out = append(out, m.styles.title.Render(content))
			}
			i++
			continue
		}

		// Detect start of a markdown table block.
		if isMarkdownTableLine(line) {
			var tableLines []string
			for i < len(rawLines) && isMarkdownTableLine(rawLines[i]) {
				tableLines = append(tableLines, rawLines[i])
				i++
			}
			out = append(out, renderMarkdownTable(tableLines, m, width)...)
			continue
		}

		cleaned := stripMarkdownMarkers(m.clean(line))
		if strings.TrimSpace(cleaned) == "" {
			out = append(out, "")
			i++
			continue
		}
		if marker, content, ok := parseOrderedListLine(cleaned); ok {
			content = stripInlineItalic(content)
			out = append(out, wrapHangingLine(marker+" "+content, strings.Repeat(" ", lipgloss.Width(marker)+1), width)...)
			i++
			continue
		}
		if marker, content, ok := parseBulletListLine(cleaned); ok {
			content = stripInlineItalic(content)
			out = append(out, wrapHangingLine(marker+" "+content, "  ", width)...)
			i++
			continue
		}
		text := stripInlineItalic(strings.TrimSpace(cleaned))
		out = append(out, wrapHangingLine(text, "", width)...)
		i++
	}
	return strings.Join(out, "\n")
}

// isHorizontalRule reports whether line is a Markdown horizontal rule (---, ***, ___).
// All non-space characters must be the same and there must be at least 3 of them.
func isHorizontalRule(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	var char byte
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == ' ' {
			continue
		}
		if char == 0 {
			if c != '-' && c != '*' && c != '_' {
				return false
			}
			char = c
		} else if c != char {
			return false
		}
	}
	count := 0
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == char {
			count++
		}
	}
	return count >= 3
}

var (
	italicStarRe  = regexp.MustCompile(`\*([^*\n]+)\*`)
	italicUnderRe = regexp.MustCompile(`_([^_\n]+)_`)
)

// stripInlineItalic removes *text* and _text_ italic/emphasis markers from a single line.
// It is called after stripMarkdownMarkers has removed ** and __ markers, so only
// single-delimited patterns remain.
func stripInlineItalic(text string) string {
	text = italicStarRe.ReplaceAllString(text, "$1")
	text = italicUnderRe.ReplaceAllString(text, "$1")
	return text
}

// isMarkdownTableLine returns true for lines that are part of a markdown table
// (start with optional spaces then `|`).
func isMarkdownTableLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

// isMarkdownTableSeparatorLine returns true for lines like |---|---|.
func isMarkdownTableSeparatorLine(line string) bool {
	stripped := strings.TrimSpace(line)
	for _, r := range stripped {
		if r != '|' && r != '-' && r != ':' && r != ' ' {
			return false
		}
	}
	return strings.ContainsRune(stripped, '-')
}

// parseTableRow splits a markdown table row into trimmed cell strings.
func parseTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// renderMarkdownTable converts raw table lines into aligned plain-text rows.
func renderMarkdownTable(lines []string, m InteractiveModel, width int) []string {
	// Parse all non-separator rows.
	var rows [][]string
	for _, line := range lines {
		if isMarkdownTableSeparatorLine(line) {
			continue
		}
		cells := parseTableRow(line)
		// Strip markdown inside each cell.
		for j, c := range cells {
			cells[j] = stripMarkdownMarkers(m.clean(c))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return nil
	}
	// Find max column count.
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	// Compute column widths.
	colWidths := make([]int, cols)
	for _, r := range rows {
		for j, cell := range r {
			if w := lipgloss.Width(cell); w > colWidths[j] {
				colWidths[j] = w
			}
		}
	}
	// Clamp total width.
	totalWidth := cols + 1 // pipes
	for _, w := range colWidths {
		totalWidth += w + 2 // padding
	}
	_ = totalWidth // width clamping reserved for future use

	// Build output lines.
	var out []string
	for rowIdx, r := range rows {
		// Pad cells.
		padded := make([]string, cols)
		for j := 0; j < cols; j++ {
			cell := ""
			if j < len(r) {
				cell = r[j]
			}
			cw := colWidths[j]
			padded[j] = cell + strings.Repeat(" ", cw-lipgloss.Width(cell))
		}
		out = append(out, "│ "+strings.Join(padded, " │ ")+" │")
		// Separator line after header row.
		if rowIdx == 0 {
			sepParts := make([]string, cols)
			for j := 0; j < cols; j++ {
				sepParts[j] = strings.Repeat("─", colWidths[j]+2)
			}
			out = append(out, "├─"+strings.Join(sepParts, "─┼─")+"─┤")
		}
	}
	return out
}

func normalizeMarkdownLines(lines []string) []string {
	normalized := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if marker, content, ok := parseOrderedListLine(line); ok && strings.TrimSpace(content) == "" {
			var parts []string
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next == "" {
					break
				}
				if _, _, ok := parseOrderedListLine(next); ok {
					break
				}
				if _, _, ok := parseBulletListLine(next); ok {
					break
				}
				parts = append(parts, next)
				i++
				if strings.Contains(next, ":") || strings.HasSuffix(next, "。") {
					break
				}
			}
			normalized = append(normalized, marker+" "+strings.Join(parts, " "))
			continue
		}
		normalized = append(normalized, line)
	}
	return normalized
}

func stripMarkdownMarkers(text string) string {
	replacer := strings.NewReplacer(
		"***", "",
		"**", "",
		"__", "",
		"~~", "",
		"`", "",
	)
	return strings.TrimSpace(replacer.Replace(text))
}

func parseOrderedListLine(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", false
	}
	dot := -1
	for i, r := range trimmed {
		if r == '.' || r == '．' {
			dot = i
			break
		}
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	if dot <= 0 {
		return "", "", false
	}
	return trimmed[:dot+1], strings.TrimSpace(trimmed[dot+1:]), true
}

func parseBulletListLine(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return "", "", false
	}
	if trimmed[1] != ' ' && trimmed[1] != '\t' {
		return "", "", false
	}
	switch trimmed[0] {
	case '-', '*':
		return string(trimmed[0]), strings.TrimSpace(trimmed[2:]), true
	default:
		return "", "", false
	}
}

func wrapHangingLine(text string, continuationIndent string, width int) []string {
	if width <= 0 || lipgloss.Width(text) <= width {
		return []string{text}
	}
	var lines []string
	current := ""
	currentWidth := 0
	currentLimit := width
	for _, r := range text {
		rw := lipgloss.Width(string(r))
		if current != "" && currentWidth+rw > currentLimit {
			lines = append(lines, current)
			current = continuationIndent
			currentWidth = lipgloss.Width(continuationIndent)
			currentLimit = width
		}
		current += string(r)
		currentWidth += rw
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// currentCopyableText returns the plain text content that should be written to
// the clipboard when the user presses Ctrl+Y. It returns the most useful text
// for the current view mode.
func (m InteractiveModel) currentCopyableText() string {
	switch m.viewMode {
	case interactiveViewAudit:
		return m.renderAudit()
	case interactiveViewEvent:
		return m.renderEvents()
	case interactiveViewSkill:
		return m.skillMsg
	case interactiveViewHistory:
		// Plain-text full history (no ANSI styles)
		var parts []string
		for i, t := range m.turns {
			parts = append(parts, fmt.Sprintf("[%d] Q: %s", i+1, t.Prompt))
			if t.Result != "" {
				parts = append(parts, "A: "+t.Result)
			}
			if t.Err != "" {
				parts = append(parts, "ERR: "+t.Err)
			}
			parts = append(parts, "")
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case interactiveViewTasks, interactiveViewTask:
		return m.taskMsg
	case interactiveViewInbox:
		return m.messageMsg
	default:
		// result / conversation view: prefer the agent reply, fall back to error
		if m.result.Result != "" {
			var parts []string
			if m.activePrompt != "" {
				parts = append(parts, "Q: "+m.activePrompt)
				parts = append(parts, "")
			}
			parts = append(parts, m.result.Result)
			return strings.Join(parts, "\n")
		}
		if m.err != "" {
			return m.err
		}
	}
	return ""
}
