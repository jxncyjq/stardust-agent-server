package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/taskledger"
)

func TestInteractiveModelRunsPromptAndStaysOpen(t *testing.T) {
	t.Parallel()

	var gotPrompt string
	model := NewInteractiveModel(InteractiveConfig{
		Runner: func(ctx context.Context, prompt string) (app.DemoResult, error) {
			if err := ctx.Err(); err != nil {
				return app.DemoResult{}, err
			}
			gotPrompt = prompt
			return app.DemoResult{
				TaskID: "task-1",
				Result: "done",
				EventStream: []domain.RuntimeEvent{
					{Type: "task_completed", Message: "done"},
				},
				AuditActions: []string{"task_completed"},
			}, nil
		},
	})

	var cmd tea.Cmd
	for _, r := range "hello" {
		next, nextCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
		cmd = nextCmd
		if cmd != nil {
			t.Fatalf("InteractiveModel.Update(%q) cmd = non-nil, want nil before enter", string(r))
		}
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatalf("InteractiveModel.Update(enter) cmd = nil, want run command")
	}
	model = runInteractiveCommand(t, model, cmd)
	if gotPrompt != "hello" {
		t.Fatalf("InteractiveModel runner prompt = %q, want hello", gotPrompt)
	}
	if model.quitting {
		t.Fatalf("InteractiveModel quitting = true, want false after run")
	}
	view := model.View()
	for _, want := range []string{"Agent", "hello", "thinking done", "● done", "Composer"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelUsesConfiguredAgentAndModelNames(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		AgentName: "planner",
		ModelName: "gpt-4.1",
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	view := model.View()
	for _, want := range []string{"planner", "gpt-4.1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing configured display value %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"cloudy", "deepseek-v4-pro"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("InteractiveModel.View() contains hard-coded value %q:\n%s", unwanted, view)
		}
	}
}

func TestInteractiveModelCentersIdleBranding(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		AgentName: "dev",
		ModelName: "deepseek-v4-pro",
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	main := model.renderMain(76, 28)
	titleLine := findLineContaining(main, "Legion Agent TUI")
	subtitleLine := findLineContaining(main, "dev  ·  deepseek-v4-pro")
	if titleLine == "" {
		t.Fatalf("renderMain() missing centered title:\n%s", main)
	}
	if subtitleLine == "" {
		t.Fatalf("renderMain() missing centered subtitle:\n%s", main)
	}
	if !isRoughlyCentered(titleLine, 76) {
		t.Fatalf("title line is not centered in width 76:\n%q", titleLine)
	}
	if !isRoughlyCentered(subtitleLine, 76) {
		t.Fatalf("subtitle line is not centered in width 76:\n%q", subtitleLine)
	}
}

func TestInteractiveModelIdleBrandingUsesMainBackground(t *testing.T) {
	previousProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(previousProfile)
	})

	model := NewInteractiveModel(InteractiveConfig{
		AgentName: "dev",
		ModelName: "deepseek-v4-pro",
	})
	main := model.renderMain(76, 28)
	for _, want := range []string{"Legion Agent TUI", "dev  ·  deepseek-v4-pro"} {
		line := findLineContaining(main, want)
		if line == "" {
			t.Fatalf("renderMain() missing branding line %q:\n%s", want, main)
		}
		if !strings.Contains(line, "\x1b[48;5;17m") {
			t.Fatalf("renderMain() line %q lacks shell background escape, got %q", want, line)
		}
	}
}

func TestInteractiveModelFormatsResultView(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "line one\nline two",
	}

	view := model.View()
	for _, want := range []string{"thinking done", "● line one", "line two"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing formatted result token %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"RESULT", "Result:", "Output"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("InteractiveModel.View() contains legacy result token %q:\n%s", unwanted, view)
		}
	}
}

func TestInteractiveModelConversationShowsPromptAndThinkingSteps(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.activePrompt = "你是什么模型"
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "我是 Legion Agent。",
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
			{Type: "model_inference_completed", Message: "ok"},
		},
	}

	view := model.renderConversation(80)
	for _, want := range []string{
		"你是什么模型",
		"thinking done",
		"received user prompt",
		"prepared Agent context",
		"event: task_started · started",
		"event: model_inference_completed · ok",
		"● 我是 Legion Agent。",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("renderConversation() missing %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelConversationShowsModelReasoningSummary(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.activePrompt = "分析问题"
	model.result = app.DemoResult{
		TaskID:           "task-1",
		ReasoningSummary: "先识别用户问题，再给出直接回答。",
		Result:           "这是最终回答。",
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
		},
	}

	view := model.renderConversation(80)
	for _, want := range []string{
		"thinking done",
		"│ 先识别用户问题，再给出直接回答。",
		"● 这是最终回答。",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("renderConversation(reasoning summary) missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "event: task_started") {
		t.Fatalf("renderConversation(reasoning summary) should prefer model reasoning over event summary:\n%s", view)
	}
}

func TestInteractiveModelStreamsToolEventsWhileRunning(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		StreamingRunner: func(ctx context.Context, prompt string, emit func(domain.RuntimeEvent)) (app.DemoResult, error) {
			if err := ctx.Err(); err != nil {
				return app.DemoResult{}, err
			}
			emit(domain.RuntimeEvent{Type: "tool_call_requested", Message: "list_files"})
			emit(domain.RuntimeEvent{Type: "tool_result", Message: "internal/observability/\ninternal/server/\ninternal/port/"})
			return app.DemoResult{TaskID: "task-1", Result: "done"}, nil
		},
	})
	model = submitComposerInput(t, model, "列出 internal 目录")

	view := model.View()
	for _, want := range []string{"tool_result", "internal/observability/", "internal/server/", "internal/port/", "● done"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View(streaming tool events) missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "truncated") || strings.Contains(view, "被截断") {
		t.Fatalf("InteractiveModel.View(streaming tool events) contains truncation marker:\n%s", view)
	}
}

func TestInteractiveModelConversationMetadataCanBeHidden(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		HidePrompt:   true,
		HideThinking: true,
	})
	model.activePrompt = "hidden prompt"
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "visible answer",
	}

	view := model.renderConversation(80)
	if strings.Contains(view, "hidden prompt") {
		t.Fatalf("renderConversation(hidden prompt) contains prompt:\n%s", view)
	}
	if strings.Contains(view, "thinking") || strings.Contains(view, "prepared Agent context") {
		t.Fatalf("renderConversation(hidden thinking) contains thinking metadata:\n%s", view)
	}
	if !strings.Contains(view, "● visible answer") {
		t.Fatalf("renderConversation(hidden metadata) missing answer:\n%s", view)
	}
}

func TestInteractiveModelFormatsMarkdownResultLists(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "1. **我是谁，取决于运行环境**: 第一条说明。\n" +
			"2.\n" +
			"   **外表 vs.\n" +
			"内在**: 第二条说明。\n" +
			"- **检查**一下配置",
	}

	got := model.renderResult(60)
	for _, want := range []string{
		"1. 我是谁，取决于运行环境: 第一条说明。",
		"2. 外表 vs. 内在: 第二条说明。",
		"- 检查一下配置",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderResult(markdown list) missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") {
		t.Fatalf("renderResult(markdown list) contains markdown emphasis markers:\n%s", got)
	}
	if strings.Contains(got, "\n2.\n") {
		t.Fatalf("renderResult(markdown list) keeps orphan ordered marker:\n%s", got)
	}
}

func TestInteractiveModelWrapsMarkdownResultWithListIndent(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "1. **这是一个很长的编号列表条目**: 用来验证终端输出会在主面板宽度内换行，并且换行后仍然保持列表缩进。",
	}

	got := model.renderResult(32)
	lines := strings.Split(got, "\n")
	var continuationSeen bool
	for _, line := range lines {
		if lipgloss.Width(line) > 30 {
			t.Fatalf("renderResult(wrapped markdown) line width = %d, want <= 30:\n%s", lipgloss.Width(line), got)
		}
		if strings.HasPrefix(line, "   ") {
			continuationSeen = true
		}
	}
	if !continuationSeen {
		t.Fatalf("renderResult(wrapped markdown) missing hanging indent continuation:\n%s", got)
	}
}

func TestInteractiveModelComposerShowsModelWaitingState(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.running = true

	composer := model.renderComposer(100)
	if !strings.Contains(composer, "正在和大模型通讯，等待输出...") {
		t.Fatalf("renderComposer(running) = %q, want model waiting state", composer)
	}
	if strings.Contains(composer, "编写任务或使用 /。") {
		t.Fatalf("renderComposer(running) contains idle placeholder, want waiting state:\n%s", composer)
	}
}

func TestInteractiveModelFooterShowsWorkingProgressBar(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		AgentName: "agent",
		ModelName: "deepseek-v4-pro",
	})
	model.running = true

	footer := model.renderFooter(100)
	for _, want := range []string{"agent", "deepseek-v4-pro", "工作中 ...", "╯╰"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("renderFooter(running) missing %q:\n%s", want, footer)
		}
	}
	if strings.Contains(footer, "Enter Run") {
		t.Fatalf("renderFooter(running) contains idle help, want working progress only:\n%s", footer)
	}
	if got := lipgloss.Width(footer); got > 100 {
		t.Fatalf("renderFooter(running) width = %d, want <= 100:\n%s", got, footer)
	}
	model.progressFrame = 5
	animated := model.renderFooter(100)
	if footer == animated {
		t.Fatalf("renderFooter(running animated) did not change across frames:\n%s", footer)
	}
}

func TestInteractiveModelProgressTickAnimatesWhileRunning(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.running = true

	next, cmd := model.Update(interactiveProgressTickMsg{})
	model = next.(InteractiveModel)
	if model.progressFrame != 1 {
		t.Fatalf("InteractiveModel progressFrame after tick = %d, want 1", model.progressFrame)
	}
	if cmd == nil {
		t.Fatalf("InteractiveModel.Update(progress tick) cmd = nil, want next tick command")
	}

	model.running = false
	next, cmd = model.Update(interactiveProgressTickMsg{})
	model = next.(InteractiveModel)
	if model.progressFrame != 1 {
		t.Fatalf("InteractiveModel progressFrame after stopped tick = %d, want unchanged 1", model.progressFrame)
	}
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(stopped progress tick) cmd = non-nil, want nil")
	}
}

func TestInteractiveModelIgnoresComposerInputWhileRunning(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	model.running = true

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(rune while running) cmd = non-nil, want nil")
	}
	if model.input != "" {
		t.Fatalf("InteractiveModel input while running = %q, want empty", model.input)
	}
}

func TestInteractiveModelViewShowsBubbleTeaLayout(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model.running = true
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "done",
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
		},
		AuditActions: []string{"audit.recorded"},
	}

	view := model.View()
	for _, want := range []string{
		"Agent",
		"agent",
		"model",
		"Plan",
		"tracks update_plan / /goal / cycles",
		"Composer",
		"正在和大模型通讯，等待输出...",
		"thinking",
		"工作中 ...",
		"agent · model",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "AUDIT") {
		t.Fatalf("InteractiveModel.View() contains AUDIT by default, want hidden:\n%s", view)
	}
	if strings.Contains(view, "EVENT STREAM") {
		t.Fatalf("InteractiveModel.View() contains EVENT STREAM by default, want hidden:\n%s", view)
	}
	if strings.Contains(view, "STATUS") {
		t.Fatalf("InteractiveModel.View() contains STATUS by default, want hidden:\n%s", view)
	}
}

func TestInteractiveModelShowsAuditAfterSlashAuditCommand(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID:       "task-1",
		Result:       "done",
		AuditActions: []string{"model_inference_completed", "task_completed"},
	}
	for _, r := range "/audit" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/audit enter) cmd = non-nil, want local view command only")
	}

	view := model.View()
	for _, want := range []string{"AUDIT", "model_inference_completed", "task_completed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q after /audit:\n%s", want, view)
		}
	}
}

func TestInteractiveModelShowsEventsAfterSlashEventCommand(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "done",
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
			{Type: "task_completed", Message: "done"},
		},
	}
	for _, r := range "/event" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/event enter) cmd = non-nil, want local view command only")
	}

	view := model.View()
	for _, want := range []string{"EVENT", "task_started", "task_completed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q after /event:\n%s", want, view)
		}
	}
}

func TestInteractiveModelSessionCommandsUseSessionManager(t *testing.T) {
	t.Parallel()

	manager := &fakeSessionManager{
		current:  "session-1",
		sessions: []string{"session-1", "session-2"},
	}
	model := NewInteractiveModel(InteractiveConfig{SessionManager: manager})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model = typeInteractiveText(t, model, "/sessions")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/sessions) cmd = non-nil, want local command")
	}
	view := model.View()
	for _, want := range []string{"SESSION", "session-1", "session-2"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q after /sessions:\n%s", want, view)
		}
	}

	model.input = ""
	model = typeInteractiveText(t, model, "/switch session-2")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if manager.current != "session-2" || model.sessionID != "session-2" {
		t.Fatalf("/switch current = %q model.sessionID = %q, want session-2", manager.current, model.sessionID)
	}
}

func TestInteractiveModelModeCommandSetsMode(t *testing.T) {
	t.Parallel()

	manager := &fakeSessionManager{current: "session-1"}
	model := NewInteractiveModel(InteractiveConfig{SessionManager: manager})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	model = typeInteractiveText(t, model, "/mode manual")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/mode manual) cmd = non-nil, want local command")
	}
	if manager.mode != "manual" {
		t.Fatalf("fakeSessionManager.mode = %q, want manual", manager.mode)
	}
	if model.mode != "manual" {
		t.Fatalf("InteractiveModel.mode = %q, want manual", model.mode)
	}
	if model.err != "" {
		t.Fatalf("InteractiveModel.err = %q, want empty after valid /mode", model.err)
	}
}

func TestInteractiveModelModeCommandRejectsInvalid(t *testing.T) {
	t.Parallel()

	manager := &fakeSessionManager{current: "session-1"}
	model := NewInteractiveModel(InteractiveConfig{SessionManager: manager})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	model = typeInteractiveText(t, model, "/mode bogus")
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if model.err == "" {
		t.Fatalf("InteractiveModel.err = empty, want non-empty for invalid /mode")
	}
	if manager.mode != "" {
		t.Fatalf("fakeSessionManager.mode = %q, want unchanged empty after invalid /mode", manager.mode)
	}
	if model.mode != domain.ModeAuto {
		t.Fatalf("InteractiveModel.mode = %q, want unchanged %q after invalid /mode", model.mode, domain.ModeAuto)
	}
}

func TestInteractiveModelCwdCommandSetsWorkingDir(t *testing.T) {
	t.Parallel()

	manager := &fakeSessionManager{current: "session-1"}
	model := NewInteractiveModel(InteractiveConfig{SessionManager: manager})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	dir := t.TempDir()
	model = typeInteractiveText(t, model, "/cwd "+dir)
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/cwd) cmd = non-nil, want local command")
	}
	if manager.workingDir != dir {
		t.Fatalf("fakeSessionManager.workingDir = %q, want %q", manager.workingDir, dir)
	}
	if model.workingDir != dir {
		t.Fatalf("InteractiveModel.workingDir = %q, want %q", model.workingDir, dir)
	}
	if model.err != "" {
		t.Fatalf("InteractiveModel.err = %q, want empty after valid /cwd", model.err)
	}

	model.input = ""
	missing := dir + string(os.PathSeparator) + "does-not-exist"
	model = typeInteractiveText(t, model, "/cwd "+missing)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if model.err == "" {
		t.Fatalf("InteractiveModel.err = empty, want non-empty for nonexistent /cwd path")
	}
	if manager.workingDir != dir {
		t.Fatalf("fakeSessionManager.workingDir = %q, want unchanged %q after invalid /cwd", manager.workingDir, dir)
	}
	if model.workingDir != dir {
		t.Fatalf("InteractiveModel.workingDir = %q, want unchanged %q after invalid /cwd", model.workingDir, dir)
	}
}

func TestInteractiveModelShowsSlashCommandSuggestions(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = next.(InteractiveModel)

	view := model.View()
	for _, want := range []string{"/audit", "/event"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing command suggestion %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelTaskCommandsShowTaskIndexAndDetail(t *testing.T) {
	t.Parallel()

	ledger := newInteractiveTaskLedger(t)
	appendInteractiveTaskEvent(t, ledger, taskledger.Event{
		TaskID:       "TASK-20260522-001",
		Type:         taskledger.EventTaskCreated,
		Title:        "调研 tasks TUI",
		Status:       "planned",
		Owner:        "researcher",
		ActorAgentID: "researcher",
		Summary:      "把任务看板放进 TUI",
	})
	model := NewInteractiveModel(InteractiveConfig{TaskLedger: ledger, AgentID: "cli-agent"})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	model = typeInteractiveText(t, model, "/tasks")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/tasks) cmd = non-nil, want local command")
	}
	for _, want := range []string{"TASKS", "TASK-20260522-001", "把任务看板放进"} {
		if !strings.Contains(model.View(), want) {
			t.Fatalf("InteractiveModel.View() missing %q after /tasks:\n%s", want, model.View())
		}
	}

	model.input = ""
	model = typeInteractiveText(t, model, "/task TASK-20260522-001")
	next, cmd = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/task) cmd = non-nil, want local command")
	}
	for _, want := range []string{"TASK", "TASK-20260522-001", "Owner", "researcher"} {
		if !strings.Contains(model.View(), want) {
			t.Fatalf("InteractiveModel.View() missing %q after /task:\n%s", want, model.View())
		}
	}
}

func TestInteractiveModelHandoffAppendsTaskLedgerEvent(t *testing.T) {
	t.Parallel()

	ledger := newInteractiveTaskLedger(t)
	appendInteractiveTaskEvent(t, ledger, taskledger.Event{
		TaskID:       "TASK-20260522-002",
		Type:         taskledger.EventTaskCreated,
		Title:        "writer handoff",
		Status:       "in_progress",
		ActorAgentID: "cli-agent",
	})
	model := NewInteractiveModel(InteractiveConfig{TaskLedger: ledger, AgentID: "cli-agent"})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model = typeInteractiveText(t, model, "/handoff writer TASK-20260522-002 请整理成说明")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/handoff) cmd = non-nil, want local command")
	}
	for _, want := range []string{"TASK", "handoff.appended", "cli-agent", "writer", "请整理成说明"} {
		if !strings.Contains(model.View(), want) {
			t.Fatalf("InteractiveModel.View() missing %q after /handoff:\n%s", want, model.View())
		}
	}
}

func TestInteractiveModelSendCommandStoresAgentMessage(t *testing.T) {
	t.Parallel()

	store := &fakeAgentMessageStore{}
	model := NewInteractiveModel(InteractiveConfig{
		MessageStore: store,
		AgentID:      "cli-agent",
		AgentName:    "dev",
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model = typeInteractiveText(t, model, "/send writer 请整理当前实现")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/send) cmd = non-nil, want local command")
	}
	if len(store.messages) != 1 {
		t.Fatalf("/send stored %d messages, want 1", len(store.messages))
	}
	message := store.messages[0]
	if message.FromAgentID != "cli-agent" || message.ToAgentID != "writer" {
		t.Fatalf("/send from/to = %q/%q, want cli-agent/writer", message.FromAgentID, message.ToAgentID)
	}
	if message.Type != domain.AgentMessageTypeMessage || message.Status != domain.AgentMessageUnread {
		t.Fatalf("/send type/status = %q/%q, want message/unread", message.Type, message.Status)
	}
	if message.Summary != "请整理当前实现" {
		t.Fatalf("/send summary = %q, want 请整理当前实现", message.Summary)
	}
	for _, want := range []string{"INBOX", "sent", "writer", "请整理当前实现"} {
		if !strings.Contains(model.View(), want) {
			t.Fatalf("InteractiveModel.View() missing %q after /send:\n%s", want, model.View())
		}
	}
}

func TestInteractiveModelInboxCommandShowsUnreadMessages(t *testing.T) {
	t.Parallel()

	store := &fakeAgentMessageStore{messages: []domain.AgentMessage{
		{
			ID:          "msg-1",
			FromAgentID: "researcher",
			ToAgentID:   "cli-agent",
			Type:        domain.AgentMessageTypeResult,
			Status:      domain.AgentMessageUnread,
			Summary:     "调研结果已完成",
			CreatedAt:   time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
		},
		{
			ID:          "msg-2",
			FromAgentID: "writer",
			ToAgentID:   "cli-agent",
			Type:        domain.AgentMessageTypeMessage,
			Status:      domain.AgentMessageRead,
			Summary:     "已读消息不默认展示",
			CreatedAt:   time.Date(2026, 5, 25, 11, 0, 0, 0, time.UTC),
		},
	}}
	model := NewInteractiveModel(InteractiveConfig{
		MessageStore: store,
		AgentID:      "cli-agent",
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model = typeInteractiveText(t, model, "/inbox")
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(/inbox) cmd = non-nil, want local command")
	}
	view := model.View()
	for _, want := range []string{"INBOX", "msg-1", "researcher", "调研结果已完成"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q after /inbox:\n%s", want, view)
		}
	}
	if strings.Contains(view, "已读消息不默认展示") {
		t.Fatalf("InteractiveModel.View() contains read message by default:\n%s", view)
	}
}

func typeInteractiveText(t *testing.T, model InteractiveModel, text string) InteractiveModel {
	t.Helper()
	for _, r := range text {
		next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	return model
}

type fakeAgentMessageStore struct {
	messages []domain.AgentMessage
}

func (s *fakeAgentMessageStore) SaveAgentMessage(_ context.Context, message domain.AgentMessage) error {
	s.messages = append(s.messages, message)
	return nil
}

func (s *fakeAgentMessageStore) ListAgentMessages(_ context.Context, query domain.AgentMessageQuery) ([]domain.AgentMessage, error) {
	var out []domain.AgentMessage
	for _, message := range s.messages {
		if query.ToAgentID != "" && message.ToAgentID != query.ToAgentID {
			continue
		}
		if query.FromAgentID != "" && message.FromAgentID != query.FromAgentID {
			continue
		}
		if query.Status != "" && message.Status != query.Status {
			continue
		}
		out = append(out, message)
		if query.Limit > 0 && len(out) >= query.Limit {
			break
		}
	}
	return out, nil
}

func (s *fakeAgentMessageStore) MarkAgentMessageRead(_ context.Context, id string, readAt time.Time) error {
	for i := range s.messages {
		if s.messages[i].ID == id {
			s.messages[i].Status = domain.AgentMessageRead
			s.messages[i].ReadAt = readAt
			return nil
		}
	}
	return nil
}

func newInteractiveTaskLedger(t *testing.T) *taskledger.Ledger {
	t.Helper()
	ledger, err := taskledger.New(taskledger.Config{
		WorkspaceRoot:   t.TempDir(),
		AllowedAgentIDs: []string{"cli-agent", "researcher", "writer"},
	})
	if err != nil {
		t.Fatalf("taskledger.New() error = %v, want nil", err)
	}
	return ledger
}

func appendInteractiveTaskEvent(t *testing.T, ledger *taskledger.Ledger, event taskledger.Event) {
	t.Helper()
	if _, err := ledger.Append(context.Background(), event); err != nil {
		t.Fatalf("Ledger.Append(%s) error = %v, want nil", event.Type, err)
	}
}

func TestInteractiveModelSlashCommandsCanBeSelectedWithArrows(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID:       "task-1",
		Result:       "done",
		AuditActions: []string{"task_completed"},
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
		},
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(InteractiveModel)
	if model.input != "/audit" {
		t.Fatalf("InteractiveModel input after slash down = %q, want /audit", model.input)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(InteractiveModel)
	if model.input != "/history" {
		t.Fatalf("InteractiveModel input after slash up = %q, want /history", model.input)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(selected /history enter) cmd = non-nil, want local command")
	}
	if model.viewMode != interactiveViewHistory {
		t.Fatalf("InteractiveModel viewMode = %q, want history", model.viewMode)
	}
}

func TestInteractiveModelShowsAgentMentionSuggestions(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		AgentNames: []string{"researcher", "writer"},
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'@'}})
	model = next.(InteractiveModel)

	view := model.View()
	for _, want := range []string{"@researcher", "@writer"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing agent suggestion %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelAgentMentionsCanBeSelectedWithArrows(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		AgentNames: []string{"researcher", "writer"},
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'@'}})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(InteractiveModel)
	if model.input != "@researcher " {
		t.Fatalf("InteractiveModel input after @ down = %q, want @researcher with trailing space", model.input)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(InteractiveModel)
	if model.input != "@writer " {
		t.Fatalf("InteractiveModel input after @ up = %q, want @writer with trailing space", model.input)
	}
}

func TestInteractiveModelAgentMentionRunsRawPrompt(t *testing.T) {
	t.Parallel()

	var gotPrompt string
	model := NewInteractiveModel(InteractiveConfig{
		AgentNames: []string{"researcher"},
		Runner: func(ctx context.Context, prompt string) (app.DemoResult, error) {
			if err := ctx.Err(); err != nil {
				return app.DemoResult{}, err
			}
			gotPrompt = prompt
			return app.DemoResult{TaskID: "task-1", Result: "done"}, nil
		},
	})
	model = submitComposerInput(t, model, "@researcher 调研一下当前实现")
	if gotPrompt != "@researcher 调研一下当前实现" {
		t.Fatalf("InteractiveModel runner prompt = %q, want raw @researcher prompt", gotPrompt)
	}
}

func TestInteractiveModelComposerHistoryUsesUpAndDown(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		Runner: func(context.Context, string) (app.DemoResult, error) {
			return app.DemoResult{TaskID: "task-1", Result: "done"}, nil
		},
	})
	model = submitComposerInput(t, model, "first command")
	model = submitComposerInput(t, model, "second command")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(up) cmd = non-nil, want nil")
	}
	if model.input != "second command" {
		t.Fatalf("InteractiveModel input after first up = %q, want second command", model.input)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(InteractiveModel)
	if model.input != "first command" {
		t.Fatalf("InteractiveModel input after second up = %q, want first command", model.input)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(InteractiveModel)
	if model.input != "second command" {
		t.Fatalf("InteractiveModel input after down = %q, want second command", model.input)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(InteractiveModel)
	if model.input != "" {
		t.Fatalf("InteractiveModel input after final down = %q, want empty composer", model.input)
	}
}

func TestInteractiveModelWorkbenchShowsComposerPlaceholder(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = next.(InteractiveModel)

	view := model.View()
	for _, want := range []string{
		"Composer",
		"编写任务或使用 /。",
		"Plan",
		"Legion Agent TUI",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelWorkbenchFitsWindowWidth(t *testing.T) {
	t.Parallel()

	const width = 120
	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 36})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: "done",
		EventStream: []domain.RuntimeEvent{
			{Type: "task_started", Message: "started"},
		},
		AuditActions: []string{"audit.recorded"},
	}

	for lineNumber, line := range strings.Split(model.View(), "\n") {
		if got := visibleWidth(line); got > width {
			t.Fatalf("line %d width = %d, want <= %d:\n%s", lineNumber+1, got, width, line)
		}
	}
}

func TestInteractiveModelMouseWheelScrollsMainOutput(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		HidePrompt:   true,
		HideThinking: true,
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: strings.Join([]string{
			"line-01", "line-02", "line-03", "line-04", "line-05", "line-06",
			"line-07", "line-08", "line-09", "line-10", "line-11", "line-12",
			"line-13", "line-14", "line-15", "line-16", "line-17", "line-18",
		}, "\n"),
	}

	initial := model.View()
	if !strings.Contains(initial, "line-01") {
		t.Fatalf("InteractiveModel.View(initial scroll) missing first output line:\n%s", initial)
	}

	next, cmd := model.Update(tea.MouseMsg{Type: tea.MouseWheelDown, Button: tea.MouseButtonWheelDown, Y: 2})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(mouse wheel down) cmd = non-nil, want nil")
	}
	scrolled := model.View()
	if model.mainScroll == 0 {
		t.Fatalf("InteractiveModel mainScroll after wheel down = 0, want > 0")
	}
	if strings.Contains(scrolled, "line-01") {
		t.Fatalf("InteractiveModel.View(after wheel down) still contains first output line:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "line-04") {
		t.Fatalf("InteractiveModel.View(after wheel down) missing scrolled line:\n%s", scrolled)
	}

	next, cmd = model.Update(tea.MouseMsg{Type: tea.MouseWheelUp, Button: tea.MouseButtonWheelUp, Y: 2})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(mouse wheel up) cmd = non-nil, want nil")
	}
	if model.mainScroll != 0 {
		t.Fatalf("InteractiveModel mainScroll after wheel up = %d, want 0", model.mainScroll)
	}
}

func TestInteractiveModelMouseWheelOutsideMainDoesNotScrollOutput(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{
		HidePrompt:   true,
		HideThinking: true,
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	model = next.(InteractiveModel)
	model.result = app.DemoResult{
		TaskID: "task-1",
		Result: strings.Join([]string{
			"line-01", "line-02", "line-03", "line-04", "line-05", "line-06",
			"line-07", "line-08", "line-09", "line-10", "line-11", "line-12",
			"line-13", "line-14", "line-15", "line-16", "line-17", "line-18",
		}, "\n"),
	}

	next, cmd := model.Update(tea.MouseMsg{Type: tea.MouseWheelDown, Button: tea.MouseButtonWheelDown, Y: 22})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("InteractiveModel.Update(mouse wheel outside main) cmd = non-nil, want nil")
	}
	if model.mainScroll != 0 {
		t.Fatalf("InteractiveModel mainScroll after outside wheel = %d, want 0", model.mainScroll)
	}
}

func TestInteractiveModelPanelsUseRequestedOuterWidth(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	for name, rendered := range map[string]string{
		"plan":     model.renderPlan(42, 12),
		"composer": model.renderComposer(100),
	} {
		want := 42
		if name == "composer" {
			want = 100
		}
		for lineNumber, line := range strings.Split(rendered, "\n") {
			if line == "" {
				continue
			}
			if got := visibleWidth(line); got != want {
				t.Fatalf("%s line %d width = %d, want %d:\n%s", name, lineNumber+1, got, want, line)
			}
		}
	}
}

func TestInteractiveModelQuitsOnQWhenInputEmpty(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	model = next.(InteractiveModel)
	if !model.quitting {
		t.Fatalf("InteractiveModel.Update(q).quitting = false, want true")
	}
	if cmd == nil {
		t.Fatalf("InteractiveModel.Update(q) cmd = nil, want tea.Quit")
	}
}

func visibleWidth(line string) int {
	return lipgloss.Width(line)
}

func findLineContaining(text string, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func isRoughlyCentered(line string, width int) bool {
	trimmed := strings.TrimSpace(line)
	left := lipgloss.Width(line[:len(line)-len(strings.TrimLeft(line, " "))])
	right := width - left - lipgloss.Width(trimmed)
	if right < 0 {
		return false
	}
	diff := left - right
	if diff < 0 {
		diff = -diff
	}
	return diff <= 1
}

func submitComposerInput(t *testing.T, model InteractiveModel, input string) InteractiveModel {
	t.Helper()

	for _, r := range input {
		next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
		if cmd != nil {
			t.Fatalf("InteractiveModel.Update(%q) cmd = non-nil before enter, want nil", string(r))
		}
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatalf("InteractiveModel.Update(enter) cmd = nil, want run command")
	}
	return runInteractiveCommand(t, model, cmd)
}

func runInteractiveCommand(t *testing.T, model InteractiveModel, cmd tea.Cmd) InteractiveModel {
	t.Helper()

	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, batchCmd := range batch {
			if batchCmd == nil {
				continue
			}
			msg := batchCmd()
			if _, ok := msg.(interactiveProgressTickMsg); ok {
				continue
			}
			model = applyInteractiveMessage(t, model, msg)
		}
		return model
	}
	return applyInteractiveMessage(t, model, msg)
}

func applyInteractiveMessage(t *testing.T, model InteractiveModel, msg tea.Msg) InteractiveModel {
	t.Helper()

	for {
		next, nextCmd := model.Update(msg)
		model = next.(InteractiveModel)
		if nextCmd == nil {
			return model
		}
		msg = nextCmd()
		if _, ok := msg.(interactiveProgressTickMsg); ok {
			return model
		}
	}
}

// --- /skill command tests ---

// fakeSkillManager is a skill.Manager stub for TUI tests.
type fakeSkillManager struct {
	installErr   error
	installSkill skill.Skill
	installed    []string
	updateErr    error
	updateSkill  skill.Skill
	updated      []string
	uninstallErr error
	uninstalled  []string
}

func (f *fakeSkillManager) Install(_ context.Context, source string) (skill.Skill, error) {
	f.installed = append(f.installed, source)
	return f.installSkill, f.installErr
}
func (f *fakeSkillManager) Update(_ context.Context, name string) (skill.Skill, error) {
	f.updated = append(f.updated, name)
	return f.updateSkill, f.updateErr
}
func (f *fakeSkillManager) Uninstall(_ context.Context, name string) error {
	f.uninstalled = append(f.uninstalled, name)
	return f.uninstallErr
}

type fakeSessionManager struct {
	current    string
	sessions   []string
	mode       string
	workingDir string
}

func (f *fakeSessionManager) CurrentSessionID() string {
	return f.current
}

func (f *fakeSessionManager) NewSession(context.Context) (string, error) {
	f.current = "session-new"
	f.sessions = append([]string{f.current}, f.sessions...)
	return f.current, nil
}

func (f *fakeSessionManager) ListSessions(context.Context) ([]string, error) {
	return append([]string(nil), f.sessions...), nil
}

func (f *fakeSessionManager) SwitchSession(_ context.Context, id string) error {
	f.current = id
	return nil
}

func (f *fakeSessionManager) ClearSession(context.Context) error {
	f.current = ""
	return nil
}

func (f *fakeSessionManager) CurrentMode() string {
	if f.mode == "" {
		return domain.ModeAuto
	}
	return f.mode
}

func (f *fakeSessionManager) SetMode(_ context.Context, mode string) error {
	normalized, ok := domain.NormalizeMode(mode)
	if !ok {
		return fmt.Errorf("invalid mode %q", mode)
	}
	f.mode = normalized
	return nil
}

func (f *fakeSessionManager) CurrentWorkingDir() string {
	return f.workingDir
}

func (f *fakeSessionManager) SetWorkingDir(_ context.Context, dir string) error {
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat working dir %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("working dir %q is not a directory", dir)
		}
	}
	f.workingDir = dir
	return nil
}

func TestInteractiveModelSkillCommandsAppearInSuggestions(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	model = next.(InteractiveModel)

	view := model.View()
	for _, want := range []string{"/skill install", "/skill update", "/skill uninstall"} {
		if !strings.Contains(view, want) {
			t.Fatalf("InteractiveModel.View() missing command suggestion %q:\n%s", want, view)
		}
	}
}

func TestInteractiveModelSlashSkillWithNoSubcommandShowsUsage(t *testing.T) {
	t.Parallel()

	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)
	for _, r := range "/skill" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd != nil {
		t.Fatalf("Update(/skill enter) cmd = non-nil, want local view only")
	}
	view := model.View()
	if !strings.Contains(view, "SKILL") {
		t.Fatalf("View() missing SKILL label after /skill:\n%s", view)
	}
	if !strings.Contains(view, "install") {
		t.Fatalf("View() missing usage hint after /skill:\n%s", view)
	}
}

func TestInteractiveModelSkillInstallSuccessShowsResult(t *testing.T) {
	t.Parallel()

	mgr := &fakeSkillManager{
		installSkill: skill.Skill{ID: "my-skill", Name: "My Skill", Version: "1.0.0"},
	}
	model := NewInteractiveModel(InteractiveConfig{SkillManager: mgr})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	for _, r := range "/skill install github:owner/my-skill" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatal("Update(/skill install ... enter) cmd = nil, want async skill command")
	}
	model = runInteractiveCommand(t, model, cmd)

	view := model.View()
	if !strings.Contains(view, "SKILL") {
		t.Fatalf("View() missing SKILL label:\n%s", view)
	}
	if !strings.Contains(view, "My Skill") {
		t.Fatalf("View() missing installed skill name:\n%s", view)
	}
}

func TestInteractiveModelSkillInstallSupportsAgentManager(t *testing.T) {
	t.Parallel()

	global := &fakeSkillManager{installSkill: skill.Skill{ID: "global", Name: "Global", Version: "1.0.0"}}
	writer := &fakeSkillManager{installSkill: skill.Skill{ID: "writer-style", Name: "Writer Style", Version: "1.0.0"}}
	model := NewInteractiveModel(InteractiveConfig{
		SkillManager:  global,
		SkillManagers: map[string]skill.Manager{"writer": writer},
	})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	for _, r := range "/skill install --agent writer github:owner/writer-style" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatal("Update(/skill install --agent writer ...) cmd = nil")
	}
	model = runInteractiveCommand(t, model, cmd)
	if len(writer.installed) != 1 || writer.installed[0] != "github:owner/writer-style" {
		t.Fatalf("writer.installed = %#v, want writer source", writer.installed)
	}
	if len(global.installed) != 0 {
		t.Fatalf("global.installed = %#v, want untouched", global.installed)
	}
	if !strings.Contains(model.View(), "Writer Style") {
		t.Fatalf("View() missing writer skill result:\n%s", model.View())
	}
}

func TestInteractiveModelSkillInstallErrorShowsError(t *testing.T) {
	t.Parallel()

	mgr := &fakeSkillManager{installErr: fmt.Errorf("network timeout")}
	model := NewInteractiveModel(InteractiveConfig{SkillManager: mgr})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	for _, r := range "/skill install https://bad.example.com/SKILL.md" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatal("Update(/skill install enter) cmd = nil")
	}
	model = runInteractiveCommand(t, model, cmd)

	if model.err == "" {
		t.Fatal("InteractiveModel.err is empty after failed install, want error message")
	}
}

func TestInteractiveModelSkillUninstallSuccessShowsResult(t *testing.T) {
	t.Parallel()

	mgr := &fakeSkillManager{}
	model := NewInteractiveModel(InteractiveConfig{SkillManager: mgr})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	for _, r := range "/skill uninstall my-skill" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatal("Update(/skill uninstall enter) cmd = nil")
	}
	model = runInteractiveCommand(t, model, cmd)

	view := model.View()
	if !strings.Contains(view, "SKILL") {
		t.Fatalf("View() missing SKILL label after uninstall:\n%s", view)
	}
}

func TestInteractiveModelSkillManagerNilShowsError(t *testing.T) {
	t.Parallel()

	// No SkillManager configured.
	model := NewInteractiveModel(InteractiveConfig{})
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model = next.(InteractiveModel)

	for _, r := range "/skill install github:owner/skill" {
		next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		model = next.(InteractiveModel)
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(InteractiveModel)
	if cmd == nil {
		t.Fatal("Update(/skill install enter) cmd = nil")
	}
	model = runInteractiveCommand(t, model, cmd)
	if model.err == "" {
		t.Fatal("InteractiveModel.err is empty when skill manager is nil, want error")
	}
}
