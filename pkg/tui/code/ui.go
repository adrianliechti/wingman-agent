package code

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"golang.org/x/term"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/skill"
	"github.com/adrianliechti/wingman-agent/pkg/tui"

	"github.com/adrianliechti/wingman-agent/pkg/tui/clipboard"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

const maxToolOutputLen = 500
const compactWidthThreshold = 100
const compactHeightThreshold = 22

func (a *App) isCompactMode() bool {
	if os.Getenv("WINGMAN_CALLER") == "vscode" {
		return true
	}

	if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		if width < compactWidthThreshold || height < compactHeightThreshold {
			return true
		}
	}

	if a.chatWidth > 0 && a.chatWidth+6 < compactWidthThreshold {
		return true
	}

	return false
}

func (a *App) getMargins() (int, int) {
	if a.isCompactMode() {
		return 0, 0
	}
	return 2, 4
}

func (a *App) getInputMargins() (int, int) {
	if a.isCompactMode() {
		return 0, 0
	}
	return 4, 4
}

func (a *App) isStreaming() bool {
	return a.getPhase() != PhaseIdle
}

func (a *App) handleInput(event *tcell.EventKey) *tcell.EventKey {
	if event.Key() == tcell.KeyEscape {
		if a.hasActiveModal() {
			a.closeActiveModal()
			return nil
		}
		if a.isStreaming() {
			a.cancelStream()
			return nil
		}
		a.input.SetText("", true)
		a.clearPendingContent()
		return nil
	}

	if event.Key() == tcell.KeyCtrlC {
		if !a.hasActiveModal() && a.input.HasSelection() {
			selectedText, _, _ := a.input.GetSelection()
			if selectedText != "" {
				a.copyTextToClipboard(selectedText)
				return nil
			}
		}
		if a.hasActiveModal() {
			a.closeActiveModal()
			return nil
		}
		if a.isStreaming() {
			a.cancelStream()
			return nil
		}
		a.stop()
		return nil
	}

	if event.Key() == tcell.KeyCtrlE && !a.hasActiveModal() {
		a.expandLevel = (a.expandLevel + 1) % 3

		if !a.showWelcome && !a.isStreaming() && !a.promptActive && !a.askActive && len(a.agent.Messages(a.sessionID)) > 0 {
			a.renderChat(a.agent.Messages(a.sessionID))
		}

		a.updateInputHint()

		return nil
	}

	if event.Key() == tcell.KeyCtrlT && !a.hasActiveModal() {
		a.toggleMouseCapture()
		return nil
	}

	if a.hasActiveModal() {
		return event
	}

	if event.Rune() == '@' && !a.isStreaming() {
		a.showFilePicker("", func(paths []string) {
			for _, p := range paths {
				a.addFileToContext(p)
			}
		})

		return nil
	}

	if a.askActive {
		if event.Key() == tcell.KeyEnter {
			text := strings.TrimSpace(a.input.GetText())

			if text == "" {
				return nil
			}

			a.input.SetText("", true)
			fmt.Fprint(a.chatView, a.formatUserMessage(text))
			a.setPhase(PhaseThinking)
			a.askResponse <- text

			return nil
		}

		return event
	}

	if a.promptActive {
		switch event.Rune() {
		case 'y', 'Y':
			fmt.Fprint(a.chatView, a.formatUserMessage("Yes"))
			a.setPhase(PhaseThinking)
			a.promptResponse <- true

			return nil

		case 'n', 'N':
			fmt.Fprint(a.chatView, a.formatUserMessage("No"))
			a.setPhase(PhaseThinking)
			a.promptResponse <- false

			return nil

		case 'a', 'A':
			a.confirmAll.Store(true)
			fmt.Fprint(a.chatView, a.formatUserMessage("Always"))
			fmt.Fprint(a.chatView, a.formatNotice("Auto-approving commands for this session", theme.Default.BrBlack))
			a.setPhase(PhaseThinking)
			a.promptResponse <- true

			return nil
		}

		return nil
	}

	if event.Key() == tcell.KeyCtrlY && !a.hasActiveModal() {
		a.copyLastResponse()
		return nil
	}

	if event.Key() == tcell.KeyCtrlL {
		a.clearChat()
		return nil
	}

	if event.Key() == tcell.KeyTab && !a.isStreaming() {
		text := a.input.GetText()
		if strings.HasPrefix(text, "/") && !strings.Contains(text, " ") {
			matches := a.matchingCommands(text)
			if len(matches) == 1 {
				a.input.SetText(matches[0].Name, true)
				return nil
			}
			if len(matches) > 1 {
				prefix := matches[0].Name
				for _, m := range matches[1:] {
					for !strings.HasPrefix(m.Name, prefix) {
						prefix = prefix[:len(prefix)-1]
					}
				}
				if len(prefix) > len(text) {
					a.input.SetText(prefix, true)
					return nil
				}
			}
			return nil
		}
		a.toggleMode()
		return nil
	}

	if event.Key() == tcell.KeyBacktab && !a.isStreaming() {
		a.cycleModel()
		return nil
	}

	if event.Key() == tcell.KeyEnter {

		a.submitInput()
		return nil
	}

	return event
}

func (a *App) resetPlaceholder() {
	a.app.QueueUpdateDraw(func() {
		a.input.SetPlaceholder("Ask anything...")
	})
}

func (a *App) Confirm(ctx context.Context, message string) (bool, error) {
	if a.confirmAll.Load() {
		return true, nil
	}

	a.elicitMu.Lock()
	defer a.elicitMu.Unlock()

	a.promptResponse = make(chan bool, 1)
	a.promptActive = true
	defer func() {
		a.promptActive = false
		a.resetPlaceholder()
	}()

	t := theme.Default
	hint := fmt.Sprintf("[%s]Press [-][%s::b]y[-::-][%s] approve · [-][%s::b]n[-::-][%s] deny · [-][%s::b]a[-::-][%s] always[-]", t.BrBlack, t.Green, t.BrBlack, t.Red, t.BrBlack, t.Cyan, t.BrBlack)

	a.app.QueueUpdateDraw(func() {
		fmt.Fprint(a.chatView, a.formatPrompt("Confirm Command", tview.Escape(message), hint))
		a.input.SetPlaceholder("y/n/a")
		a.app.SetFocus(a.input)
	})

	select {
	case result := <-a.promptResponse:
		return result, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-a.ctx.Done():
		return false, a.ctx.Err()
	}
}

func (a *App) Elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error) {
	// One lock across the whole request: a concurrent elicitation (parallel
	// tool calls, an MCP server) must not splice its prompt between the
	// fields of this one.
	a.elicitMu.Lock()
	defer a.elicitMu.Unlock()

	fields := req.Fields

	// A bare message (MCP confirmation-style elicitation) still needs an
	// accept/decline decision; model it as a yes/no pseudo-field whose value
	// stays out of the returned content.
	bare := len(fields) == 0
	if bare {
		fields = []tool.ElicitField{{Name: "confirmed", Type: "boolean", Required: true}}
	}

	content := map[string]any{}

	for i, field := range fields {
		message := ""
		if i == 0 {
			message = req.Message
		}

		prompt := formatElicitField(message, field, len(fields) > 1 && !bare)

		for {
			text, err := a.askLineLocked(ctx, prompt, elicitPlaceholder(field))
			if err != nil {
				return tool.ElicitResult{}, err
			}

			if strings.EqualFold(strings.TrimSpace(text), "/decline") {
				return tool.ElicitResult{Action: tool.ElicitDecline}, nil
			}

			value, ok := parseElicitValue(field, text)
			if !ok {
				prompt = fmt.Sprintf("Invalid %s — try again.", fieldKind(field))
				continue
			}

			if !bare {
				content[field.Name] = value
			} else if accepted, _ := value.(bool); !accepted {
				return tool.ElicitResult{Action: tool.ElicitDecline}, nil
			}
			break
		}
	}

	return tool.ElicitResult{Action: tool.ElicitAccept, Content: content}, nil
}

func fieldKind(field tool.ElicitField) string {
	if len(field.Enum) > 0 {
		return "choice — pick one of the listed options"
	}
	if field.Type == "" {
		return "value"
	}
	return field.Type
}

// askLineLocked expects a.elicitMu to be held by the caller.
func (a *App) askLineLocked(ctx context.Context, prompt, placeholder string) (string, error) {
	a.askResponse = make(chan string, 1)
	a.askActive = true
	defer func() {
		a.askActive = false
		a.resetPlaceholder()
	}()

	a.app.QueueUpdateDraw(func() {
		fmt.Fprint(a.chatView, a.formatPrompt("Question", tview.Escape(prompt), ""))
		a.input.SetPlaceholder(placeholder)
		a.app.SetFocus(a.input)
	})

	select {
	case result := <-a.askResponse:
		return result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.ctx.Done():
		return "", a.ctx.Err()
	}
}

func formatElicitField(message string, field tool.ElicitField, showLabel bool) string {
	var b strings.Builder

	if message != "" {
		b.WriteString(message)
	}

	if showLabel || field.Title != "" || field.Description != "" {
		label := field.Title
		if label == "" {
			label = field.Name
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(label)
		if field.Description != "" {
			b.WriteString(" — " + field.Description)
		}
	}

	for i, option := range field.Enum {
		if i == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "\n  [%d] %s", i+1, option)
		if i < len(field.EnumDescriptions) && field.EnumDescriptions[i] != "" {
			b.WriteString(" — " + field.EnumDescriptions[i])
		}
		if i < len(field.EnumPreviews) && field.EnumPreviews[i] != "" {
			for _, line := range previewLines(field.EnumPreviews[i]) {
				b.WriteString("\n      " + line)
			}
		}
	}

	if field.Default != nil {
		fmt.Fprintf(&b, "\n(default: %v)", field.Default)
	}

	return b.String()
}

const maxPreviewLines = 12

func previewLines(preview string) []string {
	lines := strings.Split(strings.TrimRight(preview, "\n"), "\n")
	if len(lines) > maxPreviewLines {
		lines = append(lines[:maxPreviewLines], "…")
	}
	return lines
}

func elicitPlaceholder(field tool.ElicitField) string {
	switch {
	case field.Multiple && len(field.Enum) > 0:
		if field.Strict {
			return fmt.Sprintf("Pick options like 1,3 (1-%d) · /decline", len(field.Enum))
		}
		return fmt.Sprintf("Pick options like 1,3 (1-%d), or type your own answer · /decline", len(field.Enum))
	case len(field.Enum) > 0:
		if field.Strict {
			return fmt.Sprintf("1-%d picks an option · /decline", len(field.Enum))
		}
		return fmt.Sprintf("1-%d picks an option, or type your own answer · /decline", len(field.Enum))
	case field.Type == "boolean":
		return "y/n · /decline"
	case field.Type == "number" || field.Type == "integer":
		return "Enter a number · /decline"
	default:
		return "Type your answer and press Enter · /decline"
	}
}

func parseElicitValue(field tool.ElicitField, text string) (any, bool) {
	text = strings.TrimSpace(text)

	if field.Multiple && len(field.Enum) > 0 {
		picks := make([]string, 0, 4)
		for _, part := range strings.Split(text, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			value, ok := resolveEnumToken(field, part)
			if !ok {
				return nil, false
			}
			picks = append(picks, value)
		}
		if len(picks) == 0 {
			return nil, false
		}
		return picks, true
	}

	if len(field.Enum) > 0 {
		return resolveEnumToken(field, text)
	}

	switch field.Type {
	case "boolean":
		switch strings.ToLower(text) {
		case "y", "yes", "true", "1":
			return true, true
		case "n", "no", "false", "0":
			return false, true
		}
		return nil, false
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		return n, err == nil
	case "number":
		f, err := strconv.ParseFloat(text, 64)
		return f, err == nil
	default:
		return text, true
	}
}

// resolveEnumToken maps a typed token to an enum value: an option number, an
// exact enum member, or — for advisory (non-strict) options — any free text.
func resolveEnumToken(field tool.ElicitField, token string) (string, bool) {
	if n, err := strconv.Atoi(token); err == nil && n >= 1 && n <= len(field.Enum) {
		return field.Enum[n-1], true
	}
	for _, value := range field.Enum {
		if strings.EqualFold(value, token) {
			return value, true
		}
	}
	if field.Strict {
		return "", false
	}
	return token, true
}

func (a *App) copyTextToClipboard(text string) {
	go func() {
		err := clipboard.WriteText(text)

		a.app.QueueUpdateDraw(func() {
			message := "Copied to clipboard"
			color := theme.Default.BrBlack

			if err != nil {
				message = fmt.Sprintf("Clipboard copy failed: %v", err)
				color = theme.Default.Red
			}

			fmt.Fprint(a.chatView, a.formatNotice(message, color))
		})
	}()
}

func (a *App) copyLastResponse() {
	messages := a.agent.Messages(a.sessionID)

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleAssistant {
			for _, c := range messages[i].Content {
				if c.Text != "" {
					a.copyTextToClipboard(c.Text)

					return
				}
			}
		}
	}
}

func (a *App) pasteFromClipboard() {
	go func() {
		contents, err := clipboard.Read()

		if err != nil || len(contents) == 0 {
			return
		}

		a.app.QueueUpdateDraw(func() {
			for _, c := range contents {
				if c.Image != nil {
					a.pendingContent = append(a.pendingContent, agent.Content{File: &agent.File{Data: *c.Image}})
				}

				if c.Text != "" {
					paths := detectFilePaths(c.Text, a.agent.Workspace().RootPath)
					if len(paths) > 0 {
						for _, p := range paths {
							a.addFileToContext(normalizeFilePath(p, a.agent.Workspace().RootPath))
						}

						continue
					}

					_, start, end := a.input.GetSelection()
					a.input.Replace(start, end, c.Text)
				}
			}

			a.updateInputHint()
		})
	}()
}

func (a *App) cancelStream() {
	a.streamMu.Lock()
	if a.streamCancel != nil {
		a.streamCancel()
	}
	a.streamMu.Unlock()

	if a.askActive {
		a.input.SetText("", true)

		select {
		case a.askResponse <- "":
		default:
		}
	}

	if a.promptActive {
		select {
		case a.promptResponse <- false:
		default:
		}
	}
}

func (a *App) clearPendingContent() {
	a.pendingContent = nil
	a.pendingFiles = nil
	a.updateInputHint()
}

func (a *App) clearChat() {
	a.chatView.Clear()

	id, err := a.agent.NewSession(a.ctx)
	if err == nil {
		a.sessionID = id
	}
	a.inputTokens = 0
	a.cachedTokens = 0
	a.outputTokens = 0
	a.updateStatusBar()
}

func (a *App) resumeSession() {
	t := theme.Default

	sessions, err := a.agent.ListSessions(a.ctx)
	if err != nil || len(sessions) == 0 {
		fmt.Fprint(a.chatView, a.formatNotice("No sessions to resume", t.Yellow))
		return
	}

	last := sessions[0]
	if err := a.agent.LoadSession(a.ctx, last.ID); err != nil {
		fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Failed to load session: %v", err), t.Red))
		return
	}

	a.sessionID = last.ID

	usage := a.agent.Usage(a.sessionID)
	a.inputTokens = usage.InputTokens
	a.cachedTokens = usage.CachedTokens
	a.outputTokens = usage.OutputTokens

	a.switchToChat()
	a.renderChat(a.agent.Messages(a.sessionID))
	a.updateStatusBar()

	fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Resumed session from %s", last.UpdatedAt.Format("Jan 2 15:04")), t.Green))
	a.chatView.ScrollToEnd()
}

func (a *App) showError(title string, err error) {
	a.switchToChat()
	fmt.Fprint(a.chatView, a.formatError(title, err.Error()))
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

func (a *App) countPendingImages() int {
	count := 0

	for _, c := range a.pendingContent {
		if c.File != nil {
			count++
		}
	}

	return count
}

func (a *App) submitInput() {
	query := strings.TrimSpace(a.input.GetText())

	if query == "" {
		return
	}

	if a.getPhase() != PhaseIdle && isBuiltinCommand(query) {
		return
	}

	switch query {
	case "/quit":
		a.stop()

		return

	case "/clear":
		a.clearChat()
		a.input.SetText("", true)
		return

	case "/resume":
		a.resumeSession()
		a.input.SetText("", true)
		return

	case "/help":
		a.switchToChat()
		a.input.SetText("", true)
		t := theme.Default
		builtinCmds := a.builtinCommands()
		skillCmds := a.skillCommands()

		maxLen := 0
		for _, cmd := range builtinCmds {
			if len(cmd.Name) > maxLen {
				maxLen = len(cmd.Name)
			}
		}
		for _, cmd := range skillCmds {
			if len(cmd.Name) > maxLen {
				maxLen = len(cmd.Name)
			}
		}

		fmt.Fprintf(a.chatView, "  [%s]┃[-] [%s::b]Commands[-::-]\n", t.Cyan, t.Cyan)
		for _, cmd := range builtinCmds {
			pad := strings.Repeat(" ", maxLen-len(cmd.Name))
			fmt.Fprintf(a.chatView, "  [%s]┃[-]   [%s]%s[-]%s    %s\n", t.Cyan, t.BrCyan, cmd.Name, pad, cmd.Desc)
		}

		if len(skillCmds) > 0 {
			fmt.Fprintf(a.chatView, "  [%s]┃[-]\n", t.Cyan)
			fmt.Fprintf(a.chatView, "  [%s]┃[-] [%s::b]Skills[-::-]\n", t.Cyan, t.Cyan)
			for _, cmd := range skillCmds {
				pad := strings.Repeat(" ", maxLen-len(cmd.Name))
				fmt.Fprintf(a.chatView, "  [%s]┃[-]   [%s]%s[-]%s    %s\n", t.Cyan, t.BrCyan, cmd.Name, pad, cmd.Desc)
			}
		}

		fmt.Fprint(a.chatView, "\n")
		a.chatView.ScrollToEnd()

		return

	case "/models", "/model":
		a.input.SetText("", true)
		a.showModelPicker()

		return

	case "/effort":
		a.input.SetText("", true)
		a.showEffortPicker()

		return

	case "/plan":
		a.input.SetText("", true)
		a.switchToChat()
		a.enterPlanMode()

		return

	case "/agent":
		a.input.SetText("", true)
		a.switchToChat()
		a.exitPlanMode()

		return

	case "/rewind":
		a.input.SetText("", true)
		a.switchToChat()
		a.showRewindPicker()

		return

	case "/diff":
		a.input.SetText("", true)
		a.switchToChat()
		a.showDiffView()

		return

	case "/problems":
		a.input.SetText("", true)
		a.switchToChat()
		a.showDiagnosticsView()

		return

	case "/copy":
		a.input.SetText("", true)
		a.copyLastResponse()

		return

	case "/paste":
		a.input.SetText("", true)
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
			a.input.SetText("", true)
			a.invokeSkill(s, skillArgs)
			return
		}

		a.input.SetText("", true)
		a.switchToChat()
		fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Unknown command: /%s", skillName), theme.Default.Yellow))
		return
	}

	a.switchToChat()
	a.app.ForceDraw()
	a.input.SetText("", true)

	imageCount := a.countPendingImages()

	displayText := query
	if imageCount > 0 || len(a.pendingFiles) > 0 {
		var attachments []string
		if imageCount == 1 {
			attachments = append(attachments, "📷 1 image")
		} else if imageCount > 1 {
			attachments = append(attachments, fmt.Sprintf("📷 %d images", imageCount))
		}
		for _, f := range a.pendingFiles {
			attachments = append(attachments, fmt.Sprintf("📄 %s", filepath.Base(f)))
		}
		displayText = fmt.Sprintf("%s\n[%s]%s[-]", query, theme.Default.BrBlack, strings.Join(attachments, ", "))
	}
	fmt.Fprint(a.chatView, a.formatUserMessage(displayText))

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

	go a.streamResponse(input)
}

func (a *App) invokeSkill(s *skill.Skill, args string) {
	content, err := s.GetContent(a.agent.Workspace().RootPath)
	if err != nil {
		a.switchToChat()
		fmt.Fprint(a.chatView, a.formatNotice(fmt.Sprintf("Failed to load skill %q: %v", s.Name, err), theme.Default.Red))
		return
	}

	if s.Bundled {
		_, _ = skill.MaterializeBundled(s)
	}

	content = s.ApplyArguments(content, args, s.AbsoluteDir(a.agent.Workspace().RootPath))

	a.switchToChat()
	a.app.ForceDraw()

	displayText := fmt.Sprintf("/%s", s.Name)
	if args != "" {
		displayText += " " + args
	}
	fmt.Fprint(a.chatView, a.formatUserMessage(displayText))

	input := []agent.Content{{Text: content}}
	input = append(input, a.pendingContent...)
	a.clearPendingContent()

	go a.streamResponse(input)
}

func (a *App) switchToChat() {
	if !a.showWelcome {
		return
	}
	a.showWelcome = false
	a.rebuildContentPages()
}

func (a *App) rebuildContentPages() {
	a.contentPages.Clear()

	if a.showWelcome && !a.isCompactMode() {
		a.contentPages.AddItem(nil, 0, 2, false)
		a.contentPages.AddItem(a.welcomeView, 13, 0, false)
		a.contentPages.AddItem(a.chatView, 0, 3, false)
	} else {
		a.contentPages.AddItem(a.chatView, 0, 1, false)
	}
}

func (a *App) updateWelcome() {
	t := theme.Default

	var b strings.Builder
	b.WriteString(tui.Logo)
	b.WriteString("\n")

	cwd := a.agent.Workspace().RootPath
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	fmt.Fprintf(&b, "[%s]%s[-]\n", t.BrBlack, tview.Escape(cwd))

	b.WriteString("\n")
	fmt.Fprintf(&b, "[%s]/help[-][%s] commands   [-][%s]@[-][%s] add files   [-][%s]tab[-][%s] plan mode[-]",
		t.Foreground, t.BrBlack, t.Foreground, t.BrBlack, t.Foreground, t.BrBlack)

	a.welcomeView.SetText(b.String())
}

func (a *App) setupUI() {
	t := theme.Default

	a.welcomeView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	a.welcomeView.SetBackgroundColor(tcell.ColorDefault)
	a.updateWelcome()

	a.chatView = tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(false).
		SetScrollable(true).
		SetChangedFunc(func() {
			a.app.Draw()
		})
	a.chatView.SetBorder(false)
	a.chatView.SetBackgroundColor(tcell.ColorDefault)

	inputBgColor := t.Selection
	a.input = tview.NewTextArea().
		SetPlaceholder("Ask anything...")
	a.input.SetBackgroundColor(inputBgColor)
	a.input.SetBorder(false)
	a.input.SetTextStyle(tcell.StyleDefault.Foreground(t.Foreground).Background(inputBgColor))
	a.input.SetPlaceholderStyle(tcell.StyleDefault.Foreground(t.BrBlack).Background(inputBgColor))

	a.contentPages = tview.NewFlex().SetDirection(tview.FlexRow)
	a.contentPages.SetBackgroundColor(tcell.ColorDefault)
	a.rebuildContentPages()
}

func (a *App) buildLayout() *tview.Flex {
	t := theme.Default
	inputBgColor := t.Selection

	a.inputFrame = tview.NewFrame(a.input).
		SetBorders(1, 1, 0, 0, 1, 1)
	a.inputFrame.SetBackgroundColor(inputBgColor)
	a.inputFrame.SetBorder(false)

	a.input.SetChangedFunc(func() {
		a.updateInputHeight()
		a.updateInputHint()
	})

	a.input.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		isPaste := event.Key() == tcell.KeyCtrlV || (event.Modifiers()&tcell.ModMeta != 0 && (event.Rune() == 'v' || event.Rune() == 'V'))

		if isPaste {
			a.pasteFromClipboard()

			return nil
		}

		return event
	})

	bottomBar := tview.NewFlex().SetDirection(tview.FlexColumn)

	a.inputHint = tview.NewTextView().
		SetDynamicColors(true)
	a.inputHint.SetBackgroundColor(tcell.ColorDefault)
	a.updateInputHint()

	a.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignRight)
	a.updateStatusBar()
	a.statusBar.SetBackgroundColor(tcell.ColorDefault)

	bottomBar.AddItem(a.inputHint, 0, 1, false)
	bottomBar.AddItem(a.statusBar, 40, 0, false)

	inputLeftMargin, inputRightMargin := a.getInputMargins()

	bottomBarContainer := tview.NewFlex().SetDirection(tview.FlexColumn)
	bottomBarContainer.AddItem(nil, inputLeftMargin, 0, false)
	bottomBarContainer.AddItem(bottomBar, 0, 1, false)
	bottomBarContainer.AddItem(nil, inputRightMargin, 0, false)

	inputContainer := tview.NewFlex().SetDirection(tview.FlexColumn)
	inputContainer.AddItem(nil, inputLeftMargin, 0, false)
	inputContainer.AddItem(a.inputFrame, 0, 1, true)
	inputContainer.AddItem(nil, inputRightMargin, 0, false)

	a.inputSection = tview.NewFlex().SetDirection(tview.FlexRow)
	a.inputSection.AddItem(inputContainer, 0, 1, true)
	a.inputSection.AddItem(bottomBarContainer, 1, 0, false)

	leftMargin, rightMargin := a.getMargins()
	totalMargin := leftMargin + rightMargin

	a.chatContainer = tview.NewFlex().SetDirection(tview.FlexColumn)
	a.chatContainer.AddItem(nil, leftMargin, 0, false)
	a.chatContainer.AddItem(a.contentPages, 0, 1, false)
	a.chatContainer.AddItem(nil, rightMargin, 0, false)

	a.lastCompact = a.isCompactMode()

	a.chatContainer.SetDrawFunc(func(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
		newWidth := width - totalMargin

		if newWidth != a.chatWidth {
			a.chatWidth = newWidth

			if !a.showWelcome && !a.promptActive && !a.askActive && len(a.agent.Messages(a.sessionID)) > 0 {
				a.renderChat(a.agent.Messages(a.sessionID))
			}
		}

		if a.showWelcome {
			compact := a.isCompactMode()
			if compact != a.lastCompact {
				a.lastCompact = compact
				a.rebuildContentPages()
			}
		}

		return x, y, width, height
	})

	a.mainLayout = tview.NewFlex().SetDirection(tview.FlexRow)
	a.mainLayout.
		AddItem(a.chatContainer, 0, 1, false).
		AddItem(a.inputSection, 6, 0, true)

	a.app.SetInputCapture(a.handleInput)

	return a.mainLayout
}

func (a *App) updateInputHeight() {
	text := a.input.GetText()
	lines := strings.Count(text, "\n") + 1

	minHeight := 6
	maxHeight := 13
	height := min(max(lines+5, minHeight), maxHeight)

	a.mainLayout.ResizeItem(a.inputSection, height, 0)
}

func (a *App) estimatedStreamOutputTokens() int64 {
	if !a.isStreaming() {
		return 0
	}

	_, _, text, reasoning := a.snapshotStreamState()

	chars := utf8.RuneCountInString(text) + utf8.RuneCountInString(reasoning)
	return int64(chars / 4)
}

func (a *App) updateStatusBar() {
	t := theme.Default

	modeLabel := "Agent"

	if a.currentMode == ModePlan {
		modeLabel = "Plan"
	}

	var parts []string

	outputTokens := a.outputTokens
	outputPrefix := "↓"
	if est := a.estimatedStreamOutputTokens(); est > 0 {
		outputTokens += est
		outputPrefix = "↓~"
	}

	if a.inputTokens > 0 || outputTokens > 0 {
		if a.cachedTokens > 0 {
			parts = append(parts, fmt.Sprintf("[%s]↑%s (%s cached) %s%s[-]", t.BrBlack, tui.FormatTokens(a.inputTokens), tui.FormatTokens(a.cachedTokens), outputPrefix, tui.FormatTokens(outputTokens)))
		} else {
			parts = append(parts, fmt.Sprintf("[%s]↑%s %s%s[-]", t.BrBlack, tui.FormatTokens(a.inputTokens), outputPrefix, tui.FormatTokens(outputTokens)))
		}
	}

	_, currentModel := a.agent.Models(a.sessionID)

	if a.lastInputTokens > 0 {
		if window := int64(agent.ContextWindowFor(currentModel, false)); window > 0 {
			left := max(0, (window-a.lastInputTokens)*100/window)
			color := t.BrBlack
			switch {
			case left <= 10:
				color = t.Red
			case left <= 30:
				color = t.Yellow
			}
			parts = append(parts, fmt.Sprintf("[%s]%d%% left[-]", color, left))
		}
	}

	parts = append(parts, fmt.Sprintf("[%s]%s[-]", t.Cyan, code.ModelName(currentModel)))

	if effort, _ := a.agent.Effort(a.sessionID); effort != "" && effort != "auto" {
		parts = append(parts, fmt.Sprintf("[%s]%s[-]", t.Cyan, strings.ToUpper(effort[:1])+effort[1:]))
	}

	parts = append(parts, fmt.Sprintf("[%s]%s[-]", t.Yellow, modeLabel))

	a.statusBar.SetText(strings.Join(parts, " • "))
}

func (a *App) formatShortcut(key, label string) string {
	t := theme.Default
	return fmt.Sprintf("[%s]%s[-] [%s]%s[-]", t.BrBlack, key, t.Foreground, label)
}

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

	if a.agent.Workspace().Rewind != nil {
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

func (a *App) matchingCommands(prefix string) []slashCommand {
	if prefix == "/" {
		return a.availableCommands()
	}

	var matches []slashCommand
	for _, cmd := range a.availableCommands() {
		if strings.HasPrefix(cmd.Name, prefix) {
			matches = append(matches, cmd)
		}
	}
	return matches
}

func (a *App) toggleMouseCapture() {
	a.mouseEnabled = !a.mouseEnabled
	a.app.EnableMouse(a.mouseEnabled)
	a.updateInputHint()
}

func (a *App) updateInputHint() {

	if a.isStreaming() {
		return
	}

	t := theme.Default

	text := a.input.GetText()
	if strings.HasPrefix(text, "/") && !strings.Contains(text, " ") {
		matches := a.matchingCommands(text)
		if len(matches) > 0 {
			var parts []string
			for _, m := range matches {
				parts = append(parts, fmt.Sprintf("[%s]%s[-]", t.Cyan, m.Name))
			}
			a.inputHint.SetText(strings.Join(parts, "  "))
			return
		}
	}

	var parts []string

	imageCount := a.countPendingImages()
	if imageCount == 1 {
		parts = append(parts, fmt.Sprintf("[%s]📎 1 image[-]", t.Cyan))
	} else if imageCount > 1 {
		parts = append(parts, fmt.Sprintf("[%s]📎 %d images[-]", t.Cyan, imageCount))
	}

	if len(a.pendingFiles) == 1 {
		parts = append(parts, fmt.Sprintf("[%s]📄 %s[-]", t.Cyan, filepath.Base(a.pendingFiles[0])))
	} else if len(a.pendingFiles) > 1 {
		parts = append(parts, fmt.Sprintf("[%s]📄 %d files[-]", t.Cyan, len(a.pendingFiles)))
	}

	expandLabel := "expand"
	switch a.expandLevel {
	case 1:
		expandLabel = "expand more"
	case 2:
		expandLabel = "collapse"
	}

	if !a.mouseEnabled {
		parts = append(parts, fmt.Sprintf("[%s]select mode[-]", t.Yellow))
	}

	parts = append(parts,
		a.formatShortcut("tab", "mode"),
		a.formatShortcut("shift+tab", "model"),
		a.formatShortcut("@", "file"),
		a.formatShortcut("ctrl+e", expandLabel),
		a.formatShortcut("esc", "clear"),
	)

	a.inputHint.SetText(strings.Join(parts, "  "))
}

func (a *App) renderChat(messages []agent.Message) {
	a.chatView.Clear()

	turns := buildTurns(messages)
	prevWasTool := false

	separateFromTools := func() {
		if prevWasTool {
			fmt.Fprint(a.chatView, "\n")
			prevWasTool = false
		}
	}

	for i, t := range turns {
		isLast := i == len(turns)-1
		active := isLast && a.isStreaming()

		if t.user != nil {
			separateFromTools()
			a.renderMessage(*t.user)
		}

		showSummary := !active && a.expandLevel == 0 && len(t.working) > 0
		if showSummary {
			separateFromTools()
			fmt.Fprint(a.chatView, a.formatTurnSummary(&t))
		} else {
			for _, m := range t.working {

				if !hasVisibleContent(m) {
					continue
				}
				isTool := isToolMessage(m)
				if prevWasTool && !isTool {
					fmt.Fprint(a.chatView, "\n")
				}
				a.renderMessage(m)
				prevWasTool = isTool
			}
		}

		if t.final != nil {
			separateFromTools()
			a.renderMessage(*t.final)
		}
	}

	toolName, toolHint, streamingText, streamingReasoning := a.snapshotStreamState()

	if streamingReasoning != "" {
		separateFromTools()
		fmt.Fprint(a.chatView, a.formatReasoningProgress(streamingReasoning))
	}

	if streamingText != "" {
		separateFromTools()
		fmt.Fprint(a.chatView, a.formatAssistantMessage(streamingText))
	}

	if toolName != "" && !a.isToolHidden(toolName) {
		fmt.Fprint(a.chatView, a.formatToolProgress(toolName, toolHint))
	}

	a.chatView.ScrollToEnd()
}

func isToolMessage(msg agent.Message) bool {
	for _, c := range msg.Content {
		if c.ToolResult != nil {
			return true
		}
	}
	return false
}

func hasVisibleContent(msg agent.Message) bool {
	for _, c := range msg.Content {
		if c.ToolResult != nil {
			return true
		}
		if c.Reasoning != nil && c.Reasoning.Summary != "" {
			return true
		}
		if c.Text != "" {
			return true
		}
	}
	return false
}

func (a *App) renderMessage(msg agent.Message) {
	for _, c := range msg.Content {
		if c.ToolResult != nil {
			if a.isToolHidden(c.ToolResult.Name) {
				continue
			}

			if c.ToolResult.Name == "todo" {
				fmt.Fprint(a.chatView, a.formatTodoCell(c.ToolResult.Args))
				continue
			}

			hint := tool.ExtractHint(c.ToolResult.Args, c.ToolResult.Name)

			if a.expandLevel >= 2 {
				output := headTailPreview(c.ToolResult.Content, 4, 8)
				if len(output) > maxToolOutputLen {
					output = output[:maxToolOutputLen] + "..."
				}
				fmt.Fprint(a.chatView, a.formatToolCall(c.ToolResult.Name, hint, output))
			} else {
				fmt.Fprint(a.chatView, a.formatToolCallCollapsed(c.ToolResult.Name, hint))
			}

			continue
		}

		if c.ToolCall != nil {
			continue
		}

		if c.Reasoning != nil && c.Reasoning.Summary != "" {
			if a.expandLevel >= 2 {
				fmt.Fprint(a.chatView, a.formatReasoning(c.Reasoning.Summary))
			} else {
				fmt.Fprint(a.chatView, a.formatReasoningCollapsed(c.Reasoning.Summary))
			}
			continue
		}

		if c.Text != "" {
			switch msg.Role {
			case agent.RoleUser:
				fmt.Fprint(a.chatView, a.formatUserMessage(c.Text))
			case agent.RoleAssistant:
				fmt.Fprint(a.chatView, a.formatAssistantMessage(c.Text))
			}
		}
	}
}
