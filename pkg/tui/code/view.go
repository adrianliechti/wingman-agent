package code

import (
	"fmt"
	"os"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/tui"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/inline"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

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
		for i, l := range tui.LogoLines {
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

	return lines
}

// streamCells renders the in-flight turn tail shown below the committed chat.
// The same blank line committed cells get after a tool run is inserted here so
// spacing doesn't change when the turn finalizes.
func (a *App) streamCells(width int) []string {
	toolName, toolHint, toolProgress, streamingText, streamingReasoning := a.snapshotStreamState()

	var lines []string

	if a.prevWasTool && (streamingReasoning != "" || streamingText != "") {
		lines = append(lines, "")
	}

	if streamingReasoning != "" {
		lines = append(lines, cellReasoning(streamingReasoning, width, false)...)
	}

	if streamingText != "" {
		lines = append(lines, cellAssistant(streamingText, width, theme.Default.BrBlack)...)
	}

	if toolName != "" && !a.isToolHidden(toolName) {
		lines = append(lines, cellToolProgress(toolName, toolHint, toolProgress, width)...)
	}

	// While the spinner is visible the tail always ends blank, so tool and
	// reasoning cells never sit tight against the status row.
	if a.isStreaming() && (a.prevWasTool || len(lines) > 0) && (len(lines) == 0 || lines[len(lines)-1] != "") {
		lines = append(lines, "")
	}

	return lines
}

// render paints the full-screen frame: scrollable chat on top, then queued
// echoes, status row, composer, and popup or footer pinned at the bottom.
func (a *App) render() {
	width, height := a.term.Size()
	if width <= 0 || height <= 0 {
		return
	}

	if a.overlay != nil {
		a.term.RenderAlt(a.overlay.Render(width, height), nil)
		return
	}

	t := theme.Default

	// Selection mode: the popup is the only live element — the status
	// spinner, composer, and footer would just be noise around it.
	listPopup := a.popup != nil && a.popup.kind == popupList

	// Bottom section, built first so the chat viewport gets the remainder.
	var bottom []string
	editorStart := 0
	var cursor inline.Pos
	hasCursor := false

	if listPopup {
		bottom = append(bottom, "")
		bottom = append(bottom, a.popup.Render(width)...)
		bottom = append(bottom, "")

		// A long question must not push the options off-screen: keep the
		// tail, which holds the items.
		if len(bottom) > height {
			bottom = append([]string{dim("…")}, bottom[len(bottom)-height+1:]...)
		}
	} else {
		bottom = append(bottom, a.statusLine(width))

		if a.askActive && len(a.askHeader) > 0 {
			bottom = append(bottom, a.askHeader...)
		}

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

		var editorLines []string
		editorLines, cursor = a.editor.Render(width, maxEditorRows)
		hasCursor = true
		editorStart = len(bottom)
		bottom = append(bottom, editorLines...)

		if a.popup != nil {
			bottom = append(bottom, a.popup.Render(width)...)
		} else {
			bottom = append(bottom, a.footerLine(width))
		}
	}

	// A too-short window drops rows from the top of the bottom section — the
	// editor and footer at its tail must stay visible.
	if len(bottom) > height {
		drop := len(bottom) - height
		bottom = bottom[drop:]
		editorStart -= drop
	}

	chatRows := height - len(bottom)
	if chatRows < 0 {
		chatRows = 0
	}
	a.lastChatRows = chatRows

	view := a.chatViewLines(width)

	if a.showWelcome && len(view) == 0 {
		welcome := a.welcomeLines(width)
		pad := (chatRows - len(welcome)) / 2
		for i := 0; i < pad; i++ {
			view = append(view, "")
		}
		view = append(view, welcome...)
	}

	maxScroll := len(view) - chatRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	a.lastMaxScroll = maxScroll

	if a.follow || a.chatScroll >= maxScroll {
		a.chatScroll = maxScroll
		a.follow = true
	}

	// Bottom-anchor short conversations so content hugs the composer.
	topPad := 0
	if !a.showWelcome && len(view) < chatRows {
		topPad = chatRows - len(view)
	}
	a.lastTopPad = topPad

	selStart, selEnd := a.orderedSelection()
	showSelection := a.selActive || a.selecting

	frame := make([]string, 0, height)

	for i := 0; i < chatRows; i++ {
		idx := a.chatScroll + i - topPad
		line := ""
		if idx >= 0 && idx < len(view) {
			line = view[idx]
		}

		if showSelection && idx >= selStart.Line && idx <= selEnd.Line {
			from, to := 0, ansi.Width(line)
			if idx == selStart.Line {
				from = selStart.Col
			}
			if idx == selEnd.Line && selEnd.Col+1 < to {
				to = selEnd.Col + 1
			}
			if to <= from {
				to = from + 1
			}
			line = ansi.Highlight(line, from, to, ansi.Reverse)
		}

		frame = append(frame, line)
	}

	frame = append(frame, bottom...)

	// Scroll indicator on the status row when the newest content is
	// off-screen.
	if hidden := maxScroll - a.chatScroll; !listPopup && !a.follow && hidden > 0 {
		idx := chatRows + editorStart - 1
		if idx >= 0 && idx < len(frame) {
			indicator := dim(fmt.Sprintf("↓ %d more", hidden))
			pad := width - ansi.Width(frame[idx]) - ansi.Width(indicator) - len(cellIndent)
			if pad > 0 {
				frame[idx] += strings.Repeat(" ", pad) + indicator
			}
		}
	}

	var cursorPtr *inline.Pos
	if hasCursor {
		cursor.Row += chatRows + editorStart
		if cursor.Row >= 0 && cursor.Row < height {
			cursorPtr = &cursor
		}
	}

	a.term.RenderAlt(frame, cursorPtr)
}
