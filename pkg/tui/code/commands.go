package code

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

type slashCommand struct {
	Name string
	Desc string
}

func (a *App) builtinCommands() []slashCommand {
	cmds := []slashCommand{
		{"/help", "Show help"},
		{"/model", "Select AI model"},
		{"/effort", "Set reasoning effort"},
		{"/plan", "Enter planning mode"},
		{"/agent", "Return to execution mode"},
		{"/problems", "Show problems"},
	}

	if a.agent.Workspace().HasRewind() {
		cmds = append(cmds,
			slashCommand{"/diff", "Show changes from baseline"},
			slashCommand{"/rewind", "Restore to previous checkpoint"},
		)
	}

	cmds = append(cmds,
		slashCommand{"/copy", "Copy last response to clipboard"},
		slashCommand{"/paste", "Paste from clipboard"},
		slashCommand{"/resume", "Resume last session"},
		slashCommand{"/clear", "Clear chat history"},
		slashCommand{"/quit", "Exit application"},
	)

	return cmds
}

func (a *App) skillCommands() []slashCommand {
	var cmds []slashCommand
	for _, s := range a.agent.Workspace().Skills {
		cmds = append(cmds, slashCommand{"/" + s.Name, s.Description})
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

func (a *App) availableCommands() []slashCommand {
	return append(a.builtinCommands(), a.skillCommands()...)
}

func isBuiltinCommand(query string) bool {
	switch query {
	case "/quit", "/clear", "/resume", "/help",
		"/models", "/model", "/effort",
		"/plan", "/agent",
		"/rewind", "/diff", "/problems",
		"/copy", "/paste":
		return true
	}
	return false
}

// syncCommandPopup opens/updates/closes the slash-command popup based on the
// editor content.
func (a *App) syncCommandPopup() {
	text := a.editor.Text()

	isCommand := strings.HasPrefix(text, "/") && !strings.Contains(text, " ") && !strings.Contains(text, "\n")

	if !isCommand || a.promptActive || a.askActive {
		if a.popup != nil && a.popup.kind == popupCommands {
			a.closePopup()
		}
		return
	}

	if a.popup != nil && a.popup.kind != popupCommands {
		return
	}

	if a.popup == nil {
		var items []PopupItem
		for _, cmd := range a.availableCommands() {
			items = append(items, PopupItem{ID: cmd.Name, Label: cmd.Name, Detail: cmd.Desc})
		}
		a.popup = newPopup(popupCommands, "", items, nil)
	}

	a.popup.SetQuery(text)

	if a.popup.Empty() {
		a.closePopup()
	}
}

func (a *App) submitInput() {
	query := strings.TrimSpace(a.editor.Text())

	if query == "" {
		return
	}

	if a.getPhase() != PhaseIdle && isBuiltinCommand(query) && query != "/quit" {
		return
	}

	if a.popup != nil && a.popup.kind == popupCommands {
		a.closePopup()
	}

	switch query {
	case "/quit":
		a.stop()
		return

	case "/clear":
		a.clearChat()
		a.editor.SetText("")
		return

	case "/resume":
		a.resumeSession()
		a.editor.SetText("")
		return

	case "/help":
		a.editor.SetText("")
		a.showHelp()
		return

	case "/models", "/model":
		a.editor.SetText("")
		a.showModelPicker()
		return

	case "/effort":
		a.editor.SetText("")
		a.showEffortPicker()
		return

	case "/plan":
		a.editor.SetText("")
		a.enterPlanMode()
		return

	case "/agent":
		a.editor.SetText("")
		a.exitPlanMode()
		return

	case "/rewind":
		a.editor.SetText("")
		a.showRewindPicker()
		return

	case "/diff":
		a.editor.SetText("")
		a.showDiffView()
		return

	case "/problems":
		a.editor.SetText("")
		a.showDiagnosticsView()
		return

	case "/copy":
		a.editor.SetText("")
		a.copyLastResponse()
		return

	case "/paste":
		a.editor.SetText("")
		a.pasteFromClipboard()
		return
	}

	if strings.HasPrefix(query, "/") {
		parts := strings.SplitN(query[1:], " ", 2)
		skillName := parts[0]
		skillArgs := ""
		if len(parts) > 1 {
			skillArgs = parts[1]
		}

		if s := skill.FindSkill(skillName, a.agent.Workspace().Skills); s != nil {
			a.editor.SetText("")
			a.invokeSkill(s, skillArgs)
			return
		}

		a.editor.SetText("")
		a.appendChat(cellNotice(fmt.Sprintf("Unknown command: /%s", skillName), theme.Default.Yellow, a.width()))
		return
	}

	a.editor.AddHistory(query)
	a.editor.SetText("")

	displayText := query

	imageCount := a.countPendingImages()
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
		displayText = fmt.Sprintf("%s\n[%s]", query, strings.Join(attachments, ", "))
	}

	input := []agent.Content{{Text: displayText}}
	input = append(input, a.pendingContent...)

	if len(a.pendingFiles) > 0 {
		var sb strings.Builder
		fmt.Fprint(&sb, "\n[Attached files - use the read tool to access their content]\n")
		for _, f := range a.pendingFiles {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		input = append(input, agent.Content{Text: sb.String()})
	}

	a.clearPendingContent()
	a.showWelcome = false

	a.submitAgentInput(input, displayText)
}

func (a *App) invokeSkill(s *skill.Skill, args string) {
	content, err := s.GetContent(a.agent.Workspace().RootPath)
	if err != nil {
		a.appendChat(cellNotice(fmt.Sprintf("Failed to load skill %q: %v", s.Name, err), theme.Default.Red, a.width()))
		return
	}

	if s.Bundled {
		_, _ = skill.MaterializeBundled(s)
	}

	content = s.ApplyArguments(content, args, s.AbsoluteDir(a.agent.Workspace().RootPath))

	displayText := fmt.Sprintf("/%s", s.Name)
	if args != "" {
		displayText += " " + args
	}

	a.appendChat(cellUser(displayText, a.width()))
	a.showWelcome = false

	input := []agent.Content{{Text: content}}
	input = append(input, a.pendingContent...)
	a.clearPendingContent()

	a.submitAgentInput(input, "")
}

func (a *App) submitAgentInput(input []agent.Content, echo string) {
	id := uuid.NewString()
	a.rememberTurn(id, input)

	snap, err := a.turns.Submit(a.ctx, a.sessionID, code.TurnInput{
		ID: id, Content: input, Intent: code.TurnInputSteer,
	})
	if err != nil {
		a.takeTurnCommit(id)
		a.appendChat(cellNotice(fmt.Sprintf("Could not submit turn: %v", err), theme.Default.Red, a.width()))
		return
	}

	// Only inputs waiting behind an active turn get a preview; an input that
	// starts immediately commits within a frame and a preview would flicker.
	if echo != "" && (snap.State == code.TurnInputQueued || snap.State == code.TurnInputSteered) {
		a.pendingEcho = append(a.pendingEcho, pendingEchoItem{ID: id, Text: echo})
	}

	a.syncMessages()
	a.invalidate()
}

func (a *App) showHelp() {
	t := theme.Default

	builtinCmds := a.builtinCommands()
	skillCmds := a.skillCommands()

	maxLen := 0
	for _, cmd := range append(append([]slashCommand{}, builtinCmds...), skillCmds...) {
		if len(cmd.Name) > maxLen {
			maxLen = len(cmd.Name)
		}
	}

	var lines []string
	lines = append(lines, cellIndent+bold("commands"))
	for _, cmd := range builtinCmds {
		pad := strings.Repeat(" ", maxLen-len(cmd.Name))
		lines = append(lines, cellIndent+"  "+colored(t.Cyan, cmd.Name)+pad+"  "+dim(cmd.Desc))
	}

	if len(skillCmds) > 0 {
		lines = append(lines, "")
		lines = append(lines, cellIndent+bold("skills"))
		for _, cmd := range skillCmds {
			pad := strings.Repeat(" ", maxLen-len(cmd.Name))
			lines = append(lines, cellIndent+"  "+colored(t.Cyan, cmd.Name)+pad+"  "+dim(cmd.Desc))
		}
	}

	lines = append(lines, "")

	a.appendChat(lines)
}

func (a *App) showModelPicker() {
	available, current := a.agent.Models(a.sessionID)
	if len(available) == 0 {
		return
	}

	var items []PopupItem
	for _, m := range available {
		items = append(items, PopupItem{ID: m.ID, Label: m.Name})
	}

	popup := newPopup(popupList, "model", items, func(ids []string) {
		_ = a.agent.SetModel(a.ctx, a.sessionID, ids[0])
		a.invalidate()
	})
	popup.SelectID(current)
	a.popup = popup
}

func (a *App) cycleModel() {
	sessionID := a.sessionID

	go func() {
		available, current := a.agent.Models(sessionID)
		if len(available) <= 1 {
			return
		}
		for i, m := range available {
			if m.ID == current {
				_ = a.agent.SetModel(context.Background(), sessionID, available[(i+1)%len(available)].ID)
				break
			}
		}
		a.post(a.invalidate)
	}()
}

func (a *App) showEffortPicker() {
	current, options := a.agent.Effort(a.sessionID)
	if len(options) == 0 {
		return
	}

	items := make([]PopupItem, 0, len(options))
	for _, v := range options {
		items = append(items, PopupItem{ID: v, Label: v})
	}

	popup := newPopup(popupList, "effort", items, func(ids []string) {
		_ = a.agent.SetEffort(a.ctx, a.sessionID, ids[0])
		a.invalidate()
	})
	popup.SelectID(current)
	a.popup = popup
}

func (a *App) showRewindPicker() {
	t := theme.Default

	checkpoints, err := a.agent.Workspace().Checkpoints()
	if err != nil {
		a.appendChat(cellNotice("Rewind unavailable in this workspace", t.Yellow, a.width()))
		return
	}
	if len(checkpoints) == 0 {
		a.appendChat(cellNotice("No checkpoints available", t.Yellow, a.width()))
		return
	}

	items := make([]PopupItem, len(checkpoints))
	for i, cp := range checkpoints {
		items[i] = PopupItem{
			ID:     cp.Hash,
			Label:  cp.Time.Format("15:04:05"),
			Detail: cp.Message,
		}
	}

	a.popup = newPopup(popupList, "rewind to", items, func(ids []string) {
		var label string
		for _, item := range items {
			if item.ID == ids[0] {
				label = item.Label + " - " + item.Detail
			}
		}
		if err := a.agent.Workspace().Restore(ids[0]); err != nil {
			a.appendChat(cellNotice(fmt.Sprintf("Failed to restore: %v", err), t.Red, a.width()))
			return
		}
		a.appendChat(cellNotice(fmt.Sprintf("Restored to: %s", label), t.Green, a.width()))
	})
}

func (a *App) showFilePicker() {
	go func() {
		files := a.collectFiles()

		a.post(func() {
			if a.popup != nil {
				return
			}

			items := make([]PopupItem, len(files))
			for i, f := range files {
				items[i] = PopupItem{ID: f.Path, Label: f.Path}
			}

			popup := newPopup(popupList, "@ add files (space to mark, enter to add)", items, func(ids []string) {
				for _, p := range ids {
					a.addFileToContext(p)
				}
			})
			popup.multi = true
			a.popup = popup
			a.invalidate()
		})
	}()
}
