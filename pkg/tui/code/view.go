package code

import (
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

var logoLines = []string{
	"██╗    ██╗██╗███╗   ██╗ ██████╗ ███╗   ███╗ █████╗ ███╗   ██╗",
	"██║    ██║██║████╗  ██║██╔════╝ ████╗ ████║██╔══██╗████╗  ██║",
	"██║ █╗ ██║██║██╔██╗ ██║██║  ███╗██╔████╔██║███████║██╔██╗ ██║",
	"██║███╗██║██║██║╚██╗██║██║   ██║██║╚██╔╝██║██╔══██║██║╚██╗██║",
	"╚███╔███╔╝██║██║ ╚████║╚██████╔╝██║ ╚═╝ ██║██║  ██║██║ ╚████║",
	" ╚══╝╚══╝ ╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═══╝",
}

func (a *App) welcomeLines(width int) []string {
	t := theme.Default

	center := func(text string) string {
		pad := (width - ansi.Width(text)) / 2
		if pad < 0 {
			pad = 0
		}
		return strings.Repeat(" ", pad) + text
	}

	var lines []string

	if width > 66 {
		colors := []string{
			fg(t.Blue), fg(t.Cyan), fg(t.Green), fg(t.Yellow), fg(t.Red), fg(t.Magenta),
		}
		for i, l := range logoLines {
			lines = append(lines, center(colors[i%len(colors)]+l+ansi.Reset))
		}
	} else {
		lines = append(lines, center(bold("wingman")))
	}

	lines = append(lines, "")

	cwd := a.agent.Workspace().RootPath
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(cwd, home) {
		cwd = "~" + strings.TrimPrefix(cwd, home)
	}
	lines = append(lines, center(dim(cwd)))
	lines = append(lines, "")
	lines = append(lines, center(colored(theme.Default.Foreground, "/")+dim(" commands   ")+colored(theme.Default.Foreground, "@")+dim(" add files   ")+colored(theme.Default.Foreground, "tab")+dim(" plan mode")))
	lines = append(lines, "")

	return lines
}

// render paints the live region: welcome (until first message), the
// in-flight streaming cell, status row, composer, and popup or footer.
func (a *App) render() {
	width, height := a.term.Size()
	if width <= 0 {
		width = 80
	}

	if a.overlay != nil {
		a.term.RenderAlt(a.overlay.Render(width, height))
		return
	}

	var lines []string

	if a.showWelcome && height > 16 {
		lines = append(lines, a.welcomeLines(width)...)
	}

	toolName, toolHint, streamingText, streamingReasoning := a.snapshotStreamState()

	if streamingReasoning != "" {
		lines = append(lines, cellReasoning(streamingReasoning, width, a.verbose, false)...)
	}

	if streamingText != "" {
		lines = append(lines, cellAssistant(streamingText, width)...)
	}

	if toolName != "" && !a.isToolHidden(toolName) {
		lines = append(lines, cellToolProgress(toolName, toolHint, width)...)
	}

	for _, echo := range a.pendingEcho {
		lines = append(lines, cellIndent+dim("queued: ")+dim(ansi.Truncate(echo, width-12, "…")))
	}

	lines = append(lines, a.statusLine(width))
	lines = append(lines, "")

	t := theme.Default
	switch {
	case a.promptActive || a.askActive:
		a.editor.SetRuleColor(t.Red)
	case a.currentMode == ModePlan:
		a.editor.SetRuleColor(t.Yellow)
	default:
		a.editor.SetRuleColor(t.BrBlack)
	}

	maxEditorRows := height / 3
	if maxEditorRows < 5 {
		maxEditorRows = 5
	}

	editorLines, cursor := a.editor.Render(width, maxEditorRows)
	cursor.Row += len(lines)
	lines = append(lines, editorLines...)

	if a.popup != nil {
		lines = append(lines, a.popup.Render(width)...)
	} else {
		lines = append(lines, a.footerLine(width))
	}

	cursorPtr := &cursor
	if a.overlay != nil {
		cursorPtr = nil
	}

	a.term.Render(lines, cursorPtr)
}
