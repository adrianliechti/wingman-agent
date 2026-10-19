package code

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/task"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/schedule"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/shell"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type slashCommand struct {
	Name string
	Desc string
	Hint string

	// Busy marks commands that stay usable while a turn is running.
	Busy    bool
	Run     func(a *App)
	RunArgs func(a *App, args string)
}

func (c slashCommand) Label() string {
	if c.Hint == "" {
		return c.Name
	}
	return c.Name + " " + c.Hint
}

func (a *App) builtinCommands() []slashCommand {
	cmds := []slashCommand{
		{Name: "/queue", Desc: "Resume, edit or remove queued inputs", Busy: true, Run: (*App).showTurnQueue, RunArgs: (*App).runQueueCommand},
		{Name: "/discard", Desc: "Discard the current draft", Busy: true, Run: func(a *App) {
			a.editor.SetText("")
			a.clearPendingContent()
			a.editingQueueID = ""
			a.beforeQueueEdit = nil
			a.saveCurrentDraft()
		}},
		{Name: "/help", Desc: "Open command center", Busy: true, Run: (*App).showHelp},
		{Name: "/model", Desc: "Select AI model and effort", Run: (*App).showModelPicker},
		{Name: "/copy", Desc: "Copy a response, code block, or blockquote", Busy: true, Run: (*App).showCopyPicker},
		{Name: "/export", Desc: "Export conversation as Markdown (optional file path)", Busy: true, Run: (*App).showExportPicker, RunArgs: (*App).exportTranscript},
	}
	modes, _ := a.agent.Modes(a.sessionID)
	for _, mode := range modes {
		description := fmt.Sprintf("Switch to %s mode", mode.Name)
		if mode.Description != "" {
			description += ". " + mode.Description
		}
		cmds = append(cmds, slashCommand{
			Name: "/" + mode.ID,
			Desc: description,
			Busy: true,
			Run:  func(a *App) { a.setMode(mode.ID) },
		})
	}
	if _, ok := a.agent.(code.UnattendedAgent); ok {
		cmds = append(cmds, slashCommand{
			Name: "/unattended", Desc: "Toggle running every action without review and skipping questions", Busy: true,
			Run: (*App).toggleUnattendedMode,
		})
	}
	cmds = append(cmds,
		slashCommand{Name: "/problems", Desc: "Show problems", Busy: true, Run: (*App).showDiagnosticsView},
		slashCommand{Name: "/diff", Desc: "Show or hide working tree changes", Busy: true, Run: (*App).showDiffView},
	)
	if _, ok := a.agent.(contextStatsProvider); ok {
		cmds = append(cmds, slashCommand{Name: "/context", Desc: "Show context window usage", Busy: true, Run: (*App).showContextStats})
	}
	if _, ok := a.agent.(taskProvider); ok {
		cmds = append(cmds, slashCommand{Name: "/agents", Desc: "Watch agents and view scheduled tasks", Busy: true, Run: (*App).showAgents})
	}
	if _, ok := a.agent.(recapProvider); ok {
		cmds = append(cmds, slashCommand{Name: "/recap", Desc: "Summarize the session so far", Run: (*App).showRecap})
	}
	if _, ok := a.agent.(code.Compactor); ok {
		cmds = append(cmds, slashCommand{
			Name: "/compact", Hint: "[focus]", Desc: "Summarize the conversation to free context",
			Run: func(a *App) { a.compactContext("") }, RunArgs: (*App).compactContext,
		})
	}

	if _, ok := a.agent.(code.SessionForker); ok {
		cmds = append(cmds, slashCommand{
			Name: "/fork", Hint: "[title]", Desc: "Continue in a copy of this session; the original is kept",
			Run: func(a *App) { a.forkSession("") }, RunArgs: (*App).forkSession,
		})
	}

	cmds = append(cmds,
		slashCommand{Name: "/resume", Desc: "Resume a previous session", Run: (*App).resumeSession},
		slashCommand{Name: "/clear", Desc: "Clear chat history", Run: (*App).clearChat},
		slashCommand{Name: "/quit", Desc: "Exit application", Busy: true, Run: func(a *App) {
			if a.confirmQuit() {
				a.stop()
			}
		}},
	)

	return cmds
}

func (a *App) findBuiltin(query string) *slashCommand {
	cmds := a.builtinCommands()
	name, _, _ := strings.Cut(query, " ")
	for i := range cmds {
		if cmds[i].Name == query || cmds[i].RunArgs != nil && cmds[i].Name == name {
			return &cmds[i]
		}
	}
	return nil
}

func (a *App) skillCommands() []slashCommand {
	var cmds []slashCommand
	workspace := a.agent.Workspace()
	a.requestSkillRefresh()
	skills := workspace.Skills()
	for i := range skills {
		s := &skills[i]

		// A plugin skill whose bare name lost to another source is only
		// reachable qualified, so offer the name that actually works.
		name := s.Name
		if s.Plugin != "" && skill.FindSkill(s.Name, skills) != s {
			name = s.Qualified()
		}

		cmds = append(cmds, slashCommand{Name: "/" + name, Desc: s.Description, Hint: s.InvocationHint()})
	}
	slices.SortStableFunc(cmds, func(a, b slashCommand) int {
		if a.Name == "/init" {
			return -1
		}
		if b.Name == "/init" {
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return cmds
}

func (a *App) agentCommands() []slashCommand {
	provider, ok := a.agent.(code.CommandProvider)
	if !ok {
		return nil
	}
	commands := provider.Commands(a.sessionID)
	result := make([]slashCommand, 0, len(commands))
	for _, command := range commands {
		name := "/" + strings.TrimPrefix(command.Name, "/")
		description := command.Description
		if description == "" {
			description = command.InputHint
		}
		result = append(result, slashCommand{Name: name, Desc: description})
	}
	return result
}

func (a *App) availableCommands() []slashCommand {
	groups := [][]slashCommand{a.builtinCommands(), a.skillCommands(), a.agentCommands()}
	seen := map[string]bool{}
	var result []slashCommand
	for _, group := range groups {
		for _, command := range group {
			if seen[command.Name] {
				continue
			}
			seen[command.Name] = true
			result = append(result, command)
		}
	}
	return result
}

func (a *App) commandHint(name string) string {
	for _, command := range a.availableCommands() {
		if command.Name == name {
			return command.Hint
		}
	}
	return ""
}

// slashToken returns the /command token the cursor sits in: the rune index
// of its leading slash and the token text up to the cursor. A token starts at
// the beginning of the buffer or after whitespace and contains no whitespace,
// so paths and URLs never form one.
func slashToken(value []rune, cursor int) (int, string, bool) {
	for i := cursor; i > 0; i-- {
		r := value[i-1]
		if r == ' ' || r == '\t' || r == '\n' {
			return 0, "", false
		}
		if r != '/' {
			continue
		}
		if i >= 2 {
			prev := value[i-2]
			if prev != ' ' && prev != '\t' && prev != '\n' {
				return 0, "", false
			}
		}
		return i - 1, string(value[i-1 : cursor]), true
	}
	return 0, "", false
}

// syncCommandPopup opens/updates/closes the slash-command popup based on the
// token at the cursor. At the start of the buffer it completes commands and
// skills; mid-prompt it completes skills only.
func (a *App) syncCommandPopup() {
	start, token, ok := slashToken(a.editor.value, a.editor.cursor)

	if !ok || a.promptActive || a.askActive {
		if a.popup != nil && a.popup.kind == popupCommands {
			a.closePopup()
		}
		return
	}

	if a.popup != nil && a.popup.kind != popupCommands {
		return
	}

	inline := start > 0
	if a.popup == nil || a.cmdPopupInline != inline {
		a.closePopup()

		cmds := a.availableCommands()
		if inline {
			cmds = a.skillCommands()
		}

		var items []PopupItem
		for _, cmd := range cmds {
			items = append(items, PopupItem{ID: cmd.Name, Label: cmd.Label(), Detail: cmd.Desc})
		}
		a.popup = newPopup(popupCommands, "", items, nil)
		a.cmdPopupInline = inline
	}

	a.cmdTokenStart = start
	a.popup.SetQuery(token)
}

// completeCommand replaces the slash token at the cursor with the selected
// command and reports whether anything changed. The replacement spans the
// whole word so completing with the cursor mid-token leaves no tail behind;
// mid-prompt completions end past a trailing space so typing continues
// naturally.
func (a *App) completeCommand(id string) bool {
	start, end := a.cmdTokenStart, a.editor.cursor
	for end < len(a.editor.value) {
		r := a.editor.value[end]
		if r == ' ' || r == '\t' || r == '\n' {
			break
		}
		end++
	}

	insert := id
	advance := 0
	if start > 0 {
		if end < len(a.editor.value) && a.editor.value[end] == ' ' {
			advance = 1
		} else {
			insert += " "
		}
	} else if a.commandHint(id) != "" {
		insert += " "
	}

	changed := advance > 0 || insert != string(a.editor.value[start:end])
	a.editor.ReplaceRange(start, end, insert)
	a.editor.cursor += advance
	a.syncCommandPopup()
	return changed
}

func (a *App) submitInput() { a.submitInputIntent(code.TurnInputSteer) }

func (a *App) submitInputIntent(intent code.TurnInputIntent) {
	if a.operation != operationNone {
		a.showToast("Wait for the current operation or press Esc to cancel", theme.Default.Yellow)
		return
	}
	query := strings.TrimSpace(a.editor.Text())
	imageCount := a.countPendingImages()

	if query == "" && imageCount == 0 && len(a.pendingFiles) == 0 {
		return
	}

	if rest, ok := strings.CutPrefix(query, "!"); ok {
		if a.popup != nil && a.popup.kind == popupCommands {
			a.closePopup()
		}
		a.runUserShellCommand(query, strings.TrimSpace(rest))
		return
	}

	if query == "/models" {
		query = "/model"
	}

	cmd := a.findBuiltin(query)
	if cmd != nil && a.getPhase() != PhaseIdle && !cmd.Busy {
		return
	}

	if a.popup != nil && a.popup.kind == popupCommands {
		a.closePopup()
	}

	if cmd != nil {
		a.editor.SetText("")
		if cmd.RunArgs != nil {
			cmd.RunArgs(a, strings.TrimSpace(strings.TrimPrefix(query, cmd.Name)))
		} else {
			cmd.Run(a)
		}
		return
	}

	workspace := a.agent.Workspace()
	a.requestSkillRefresh()
	skills := workspace.Skills()

	if name, ok := skill.ParseSlashCommand(query); ok && skill.FindSkill(name, skills) == nil && !a.hasAgentCommand(name) {
		a.appendChat(cellNotice(fmt.Sprintf("Unknown command: /%s", name), theme.Default.Yellow, a.width()))
		return
	}

	displayText := query

	if imageCount > 0 || len(a.pendingFiles) > 0 {
		var attachments []string
		if imageCount == 1 {
			attachments = append(attachments, "1 image")
		} else if imageCount > 1 {
			attachments = append(attachments, fmt.Sprintf("%d images", imageCount))
		}
		for _, f := range a.pendingFiles {
			attachments = append(attachments, f)
		}
		if query != "" {
			displayText = fmt.Sprintf("%s\n[%s]", query, strings.Join(attachments, ", "))
		} else {
			displayText = fmt.Sprintf("[%s]", strings.Join(attachments, ", "))
		}
	}

	input := []harness.Content{{Text: displayText}}
	input = append(input, a.pendingContent...)

	if len(a.pendingFiles) > 0 {
		var rootPath string
		if workspace != nil {
			rootPath = workspace.RootPath
		}
		var sb strings.Builder
		fmt.Fprint(&sb, "\n[Attached files - use the read tool to access their content]\n")
		for _, f := range a.pendingFiles {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		input = append(input, harness.Content{Text: sb.String()})
		for _, f := range a.pendingFiles {
			if dataURL := readAttachedImageFile(f, rootPath); dataURL != "" {
				input = append(input, harness.Content{File: &harness.File{Name: f, Data: dataURL}})
			}
		}
	}

	for _, inv := range skill.Invocations(query, skills) {
		block, err := inv.Instructions(a.agent.Workspace().RootPath)
		if err != nil {
			a.appendChat(cellNotice(fmt.Sprintf("Failed to load skill %q: %v", inv.Skill.Name, err), theme.Default.Red, a.width()))
			continue
		}
		input = append(input, harness.Content{Text: block, Hidden: true})
	}

	editing := a.editingQueueID != ""
	if a.submitAgentInputIntent(input, displayText, intent) {
		if query != "" {
			a.editor.AddHistory(query)
		}
		a.editor.SetText("")
		a.clearPendingContent()
		a.showWelcome = false
		if editing {
			a.endQueueEdit()
		}
		a.saveCurrentDraft()
	}
}

func (a *App) hasAgentCommand(name string) bool {
	for _, command := range a.agentCommands() {
		if strings.TrimPrefix(command.Name, "/") == name {
			return true
		}
	}
	return false
}

func (a *App) runUserShellCommand(raw, command string) {
	if command == "" {
		a.editor.SetText("")
		a.saveCurrentDraft()
		if a.flow.gap() {
			a.appendChat([]string{""})
		}
		a.appendChat(cellNotice("Run a command with !<cmd>", theme.Default.BrBlack, a.width()))
		a.invalidate()
		return
	}

	a.editor.AddHistory(raw)
	a.editor.SetText("")
	a.showWelcome = false
	a.saveCurrentDraft()

	workDir := ""
	if ws := a.agent.Workspace(); ws != nil && ws.RootPath != "" {
		if info, err := os.Stat(ws.RootPath); err == nil && info.IsDir() {
			workDir = ws.RootPath
		}
	}

	id, epoch := a.sessionID, a.sessionEpoch
	ctx, cancel := context.WithCancel(code.WithSessionID(a.ctx, id))
	a.operation = operationShell
	a.operationCancel = cancel
	a.invalidate()

	go func() {
		start := time.Now()
		cmd := shell.Command(ctx, command, workDir)
		rawOut, err := cmd.CombinedOutput()
		duration := time.Since(start)
		cancel()

		exitCode := 0
		if err != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}

		// Windows shells end lines with CRLF; record the output the agent
		// sees with plain newlines, as the shell tool does.
		output := strings.ReplaceAll(string(rawOut), "\r\n", "\n")
		if output == "" && err != nil {
			if _, ok := errors.AsType[*exec.ExitError](err); !ok {
				output = err.Error()
			}
		}

		var recordErr error
		_, canRecord := a.agent.(userShellRecorder)
		if recorder, ok := a.agent.(userShellRecorder); ok {
			recordErr = recorder.RecordUserShellCommand(id, command, output, exitCode, duration)
		}

		a.post(func() {
			a.operation = operationNone
			a.operationCancel = nil
			if a.sessionID != id || a.sessionEpoch != epoch {
				return
			}
			if recordErr != nil {
				a.appendChat(cellNotice(fmt.Sprintf("Could not record shell command: %v", recordErr), theme.Default.Red, a.width()))
			} else if canRecord {
				before := a.printed
				a.syncMessages()
				if a.printed == before {
					a.appendAnnotation(func(w int) []string {
						return cellUserShell(command, output, exitCode, w, false)
					})
				}
			} else {
				a.appendAnnotation(func(w int) []string {
					return cellUserShell(command, output, exitCode, w, false)
				})
			}
			a.pollDiffPanel(false)
			a.invalidate()
		})
	}()
}

func (a *App) submitAgentInput(input []harness.Content, echo string) bool {
	return a.submitAgentInputIntent(input, echo, code.TurnInputSteer)
}

func (a *App) submitAgentInputIntent(input []harness.Content, echo string, intent code.TurnInputIntent) bool {
	a.syncQueuedInputs()
	displayText := echo
	if a.editor != nil && strings.TrimSpace(a.editor.Text()) != "" {
		displayText = strings.TrimSpace(a.editor.Text())
	}
	display := &code.TurnInputDisplay{Text: displayText, Files: slices.Clone(a.pendingFiles)}
	if a.editingQueueID != "" {
		if err := a.turns.ReplaceQueued(a.sessionID, a.editingQueueID, code.TurnInput{Content: input, Intent: code.TurnInputFollowUp, Display: display}); err != nil {
			a.showToast("Could not update queued input: "+err.Error(), theme.Default.Red)
			return false
		}
		a.syncQueuedInputs()
		return true
	}
	id := uuid.NewString()
	// Active and Steered can arrive before Submit returns. Install the preview
	// first so those events move it ahead of the turn's first streamed output.
	if echo != "" {
		a.pendingEchoMu.Lock()
		a.pendingEcho = append(a.pendingEcho, pendingEchoItem{ID: id, Text: echo, State: code.TurnInputQueued})
		a.pendingEchoMu.Unlock()
	}

	_, err := a.turns.Submit(a.ctx, a.sessionID, code.TurnInput{
		ID: id, Content: input, Intent: intent, Display: display,
	})
	if err != nil {
		a.removePendingEcho(id)
		a.appendChat(cellNotice(fmt.Sprintf("Could not submit turn: %v", err), theme.Default.Red, a.width()))
		return false
	}

	a.syncQueuedInputs()
	a.syncMessages()
	a.invalidate()
	return true
}

func (a *App) showHelp() {
	a.showCommandCenter()
}

func (a *App) showAgents() {
	t := theme.Default

	provider, ok := a.agent.(taskProvider)
	if !ok {
		a.appendChat(cellNotice("Agent transcripts are unavailable for this agent", t.Yellow, a.width()))
		return
	}

	reg := provider.Tasks(a.sessionID)

	var tasks []*task.Task
	if reg != nil {
		tasks = reg.List()
	}

	var jobs []schedule.Task
	if sp, ok := a.agent.(scheduleProvider); ok {
		if store := sp.Schedules(a.sessionID); store != nil {
			jobs, _ = store.List()
		}
	}

	if len(tasks) == 0 && len(jobs) == 0 {
		a.appendChat(cellNotice("No agents or scheduled tasks in this session", t.Yellow, a.width()))
		return
	}

	items := make([]PopupItem, 0, len(tasks)+len(jobs))
	for _, tk := range tasks {
		detail := tk.Description
		if tk.Status() == task.StatusRunning {
			if activity := tk.Activity(); activity != "" {
				detail += " — " + activity
			}
		}
		items = append(items, PopupItem{
			ID:     tk.ID,
			Label:  fmt.Sprintf("%s  %-7s  %-14s  %s", tk.ID, tk.Status(), tk.AgentType, tk.Elapsed().Round(time.Second)),
			Detail: detail,
		})
	}

	now := time.Now()
	for _, job := range jobs {
		timing := schedule.Relative(schedule.NextAttempt(job, now), now)
		if job.Script != "" {
			timing += ", pre-check"
		}
		items = append(items, PopupItem{
			ID:     schedulePopupPrefix + job.ID,
			Label:  fmt.Sprintf("%s  %-7s  %-14s  %s", job.ID, job.Status, job.Schedule, timing),
			Detail: job.Prompt,
		})
	}

	a.popup = newPopup(popupList, "agents & scheduled tasks (enter to view)", items, func(ids []string) {
		if id, ok := strings.CutPrefix(ids[0], schedulePopupPrefix); ok {
			a.showSchedule(id)
			return
		}
		if reg == nil {
			return
		}
		if tk := reg.Get(ids[0]); tk != nil {
			a.showTaskPeek(tk)
		}
	})
}

const schedulePopupPrefix = "schedule:"

func (a *App) activeScheduleCount() int {
	if a.scheduleCountSession != a.sessionID {
		a.scheduleCountSession = a.sessionID
		a.scheduleCount = 0
		a.scheduleCountAt = time.Time{}
	}
	if time.Since(a.scheduleCountAt) < time.Second || a.scheduleCountPending {
		return a.scheduleCount
	}
	sp, ok := a.agent.(scheduleProvider)
	if !ok {
		return 0
	}
	store := sp.Schedules(a.sessionID)
	if store == nil {
		return 0
	}

	a.scheduleCountAt = time.Now()
	a.scheduleCountPending = true
	id, epoch := a.sessionID, a.sessionEpoch
	go func() {
		jobs, err := store.List()
		count := 0
		for _, job := range jobs {
			if job.Status == schedule.StatusActive {
				count++
			}
		}
		a.post(func() {
			a.scheduleCountPending = false
			if err == nil && a.sessionID == id && a.sessionEpoch == epoch && a.scheduleCount != count {
				a.scheduleCount = count
				a.invalidate()
			}
		})
	}()
	return a.scheduleCount
}

func (a *App) showSchedule(id string) {
	sp, ok := a.agent.(scheduleProvider)
	if !ok {
		return
	}
	store := sp.Schedules(a.sessionID)
	if store == nil {
		return
	}

	jobs, err := store.List()
	if err != nil {
		return
	}
	i, err := schedule.Find(jobs, id)
	if err != nil {
		a.appendChat(cellNotice(err.Error(), theme.Default.Yellow, a.width()))
		return
	}

	job := jobs[i]
	now := time.Now()

	lines := []string{
		fmt.Sprintf("Scheduled task %s (%s, %s)", job.ID, job.Schedule, job.Status),
		fmt.Sprintf("Next run: %s", schedule.Relative(schedule.NextAttempt(job, now), now)),
	}
	if job.LastRun != nil {
		lines = append(lines, fmt.Sprintf("Last run: %s", job.LastRun.Local().Format(time.RFC3339)))
	}
	if job.Script != "" {
		lines = append(lines, "Pre-check script: "+job.Script)
	}
	lines = append(lines, "", job.Prompt)

	a.appendChat(cellNotice(strings.Join(lines, "\n"), theme.Default.Cyan, a.width()))
}

func (a *App) showModelPicker() {
	a.showModelPickerLevel(false)
}

func (a *App) showModelPickerLevel(back bool) {
	available, current := a.agent.Models(a.sessionID)
	if len(available) == 0 {
		return
	}

	var items []PopupItem
	for _, m := range available {
		items = append(items, PopupItem{ID: m.ID, Label: m.Name, Detail: m.Description})
	}

	kind := popupList
	title := "model"
	if back {
		kind = popupPalette
		title = "commands › model"
	}
	popup := newPopup(kind, title, items, func(ids []string) {
		modelID := ids[0]
		a.runSessionOperation("Could not change model", func(ctx context.Context, id string) error { return a.agent.SetModel(ctx, id, modelID) }, func() { a.showEffortPickerLevel(back) })
	})
	if back {
		popup.onCancel = a.showCommandCenter
	}
	for i := range popup.items {
		popup.items[i].Checked = popup.items[i].ID == current
	}
	popup.SelectID(current)
	a.popup = popup
}

func (a *App) showEffortPickerLevel(back bool) {
	current, options := a.agent.Effort(a.sessionID)
	if len(options) == 0 {
		return
	}
	selected := current
	if !slices.Contains(options, selected) {
		selected = options[0]
		for _, fallback := range []string{"default", "auto"} {
			if slices.Contains(options, fallback) {
				selected = fallback
				break
			}
		}
	}

	items := make([]PopupItem, 0, len(options))
	for _, v := range options {
		items = append(items, PopupItem{ID: v, Label: v})
	}

	kind := popupList
	title := "effort"
	if back {
		kind = popupPalette
		title = "commands › model › effort"
	}
	popup := newPopup(kind, title, items, func(ids []string) {
		effort := ids[0]
		a.runSessionOperation("Could not change effort", func(ctx context.Context, id string) error { return a.agent.SetEffort(ctx, id, effort) }, nil)
	})
	popup.onCancel = func() { a.showModelPickerLevel(back) }
	for i := range popup.items {
		popup.items[i].Checked = popup.items[i].ID == selected
	}
	popup.SelectID(selected)
	a.popup = popup
}

func (a *App) showFilePicker() {
	go func() {
		files := a.collectFiles()

		a.post(func() {
			if a.popup != nil {
				return
			}

			var images []harness.Content
			for _, content := range a.pendingContent {
				if content.File != nil {
					images = append(images, content)
				}
			}

			items := make([]PopupItem, 0, len(images)+len(files))
			for i := range images {
				items = append(items, PopupItem{ID: fmt.Sprintf("image:%d", i), Label: fmt.Sprintf("image %d", i+1), Detail: "pasted image"})
			}
			for _, f := range files {
				items = append(items, PopupItem{ID: contextFileID(f.Path), Label: f.Path, Detail: "workspace file"})
			}

			popup := newPopup(popupList, "context (space to mark, enter to apply)", items, func(ids []string) {
				a.applyContextSelection(images, files, ids)
				count := a.countPendingImages() + len(a.pendingFiles)
				a.showToast(fmt.Sprintf("Context: %d attachment(s)", count), theme.Default.Cyan)
			})
			popup.multi = true
			popup.acceptEmpty = true
			popup.labelOnly = true
			for i := range images {
				popup.SetSelected(fmt.Sprintf("image:%d", i), true)
			}
			for _, path := range a.pendingFiles {
				popup.SetSelected(contextFileID(path), true)
			}
			a.popup = popup
			a.invalidate()
		})
	}()
}
