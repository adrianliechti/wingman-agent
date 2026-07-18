package code

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/tui/ansi"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

// cellUser renders the user's prompt echo: a `›` prefix on a subtle
// background band, text starting at the shared 2-column gutter.
func cellUser(text string, width int) []string {
	t := theme.Default
	band := ansi.Bg(t.Selection)

	bandWidth := width - len(cellIndent)
	if bandWidth < 14 {
		bandWidth = 14
	}
	inner := bandWidth - 3

	var lines []string
	first := true

	for _, line := range strings.Split(strings.TrimRight(markdown.Sanitize(text), "\n"), "\n") {
		for _, wl := range ansi.Wrap(line, inner) {
			prefix := "  "
			if first {
				prefix = "› "
				first = false
			}
			pad := inner - ansi.Width(wl)
			if pad < 0 {
				pad = 0
			}
			lines = append(lines, cellIndent+band+fg(t.BrBlack)+prefix+fg(t.Foreground)+wl+strings.Repeat(" ", pad+1)+ansi.Reset)
		}
	}

	lines = append(lines, "")
	return lines
}

// cellAssistant renders assistant markdown behind a status circle: dim while
// streaming, green when committed, red on failure.
func cellAssistant(text string, width int, circle ansi.Color) []string {
	inner := width - len(cellIndent) - 2
	if inner < 10 {
		inner = 10
	}

	var lines []string
	first := true

	for _, line := range strings.Split(strings.TrimRight(markdown.Render(text), "\n"), "\n") {
		for _, wl := range ansi.Wrap(line, inner) {
			prefix := "  "
			if first {
				prefix = colored(circle, "● ")
				first = false
			}
			lines = append(lines, cellIndent+prefix+wl)
		}
	}

	lines = append(lines, "")
	return lines
}

func cellReasoning(summary string, width int, full bool) []string {
	t := theme.Default
	style := fg(t.BrBlack) + ansi.Italic

	if !full {
		tail := lastNonEmptyLine(markdown.Sanitize(summary))
		line := style + "• " + tail
		return []string{cellIndent + ansi.Truncate(line, width-len(cellIndent), "…") + ansi.Reset}
	}

	lines := []string{cellIndent + style + "• thinking" + ansi.Reset}

	inner := width - len(cellIndent) - 2
	if inner < 10 {
		inner = 10
	}

	for _, line := range strings.Split(strings.TrimRight(markdown.Sanitize(summary), "\n"), "\n") {
		for _, wl := range ansi.Wrap(style+line, inner) {
			lines = append(lines, cellIndent+"  "+wl+ansi.Reset)
		}
	}

	return lines
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t != "" {
			return t
		}
	}
	return ""
}

func isShellTool(name string) bool {
	return name == "shell" || name == "exec_command" || name == "exec_session"
}

func isMutationTool(name string) bool {
	return name == "edit" || name == "write"
}

// toolTitleLine renders the header line for a tool call.
func toolTitleLine(name, hint string, width int, running bool) string {
	t := theme.Default
	hint = markdown.Sanitize(strings.ReplaceAll(hint, "\n", " "))

	var line string
	switch {
	case isShellTool(name):
		line = colored(t.Magenta, "$ ") + bold(hint)
	case name == "agent":
		line = dim("• ") + bold("agent") + " " + dim(hint)
	default:
		label := name
		line = dim("• ") + bold(label)
		if hint != "" {
			line += " " + dim(hint)
		}
	}

	if running {
		line += dim(" …")
	}

	return cellIndent + ansi.Truncate(line, width-len(cellIndent), "…") + ansi.Reset
}

// tailPreview keeps the last n lines — command verdicts live at the end.
func tailPreview(lines []string, n int) []string {
	if len(lines) <= n+1 {
		return lines
	}
	out := []string{fmt.Sprintf("… %d earlier lines (ctrl+o transcript)", len(lines)-n)}
	return append(out, lines[len(lines)-n:]...)
}

// headPreview keeps the first n lines — diffs read from the top.
func headPreview(lines []string, n int) []string {
	if len(lines) <= n+1 {
		return lines
	}
	out := append([]string(nil), lines[:n]...)
	return append(out, fmt.Sprintf("… +%d more (ctrl+o transcript)", len(lines)-n))
}

func cellTool(result *agent.ToolResult, width int, full bool) []string {
	name := result.Name

	if name == "todo" {
		return cellTodo(result.Args, width)
	}

	hint := tool.ExtractHint(result.Args, result.Name)
	lines := []string{toolTitleLine(name, hint, width, false)}

	output := strings.TrimRight(result.Content, "\n")
	if output == "" {
		return lines
	}

	colorize := func(s string) string { return dim(markdown.Sanitize(s)) }
	preview := strings.Split(output, "\n")

	switch {
	case isShellTool(name):
		if !full {
			preview = tailPreview(preview, 3)
		}
	case isMutationTool(name):
		t := theme.Default
		colorize = func(s string) string {
			switch {
			case strings.HasPrefix(s, "+"):
				return colored(t.Green, markdown.Sanitize(s))
			case strings.HasPrefix(s, "-"):
				return colored(t.Red, markdown.Sanitize(s))
			}
			return dim(markdown.Sanitize(s))
		}
		if !full {
			preview = headPreview(preview, 5)
		}
	default:
		if !full {
			return lines
		}
	}

	lines = append(lines, continuationWrap(strings.Join(preview, "\n"), width, colorize)...)

	return lines
}

func cellToolProgress(name, hint, progress string, width int) []string {
	lines := []string{toolTitleLine(name, hint, width, true)}

	if progress != "" {
		inner := width - len(cellIndent) - 2
		if inner < 10 {
			inner = 10
		}
		text := markdown.Sanitize(strings.ReplaceAll(progress, "\n", " "))
		lines = append(lines, cellIndent+"  "+dim(ansi.Truncate(text, inner, "…")))
	}

	return lines
}

func cellTodo(argsJSON string, width int) []string {
	items := tool.ParseTodoItems(argsJSON)
	if len(items) == 0 {
		return []string{toolTitleLine("todo", "", width, false)}
	}

	t := theme.Default

	completed := 0
	for _, item := range items {
		if item.Status == "completed" {
			completed++
		}
	}

	lines := []string{cellIndent + dim("• ") + bold("plan") + " " + dim(fmt.Sprintf("%d/%d", completed, len(items)))}

	inner := width - len(cellIndent) - 4
	if inner < 10 {
		inner = 10
	}

	for _, item := range items {
		var line string
		content := markdown.Sanitize(item.Content)
		switch item.Status {
		case "completed":
			line = fg(t.Green) + "✔ " + ansi.Reset + fg(t.BrBlack) + ansi.Strike + content
		case "in_progress":
			line = fg(t.Cyan) + ansi.Bold + "□ " + content
		default:
			line = fg(t.BrBlack) + "□ " + content
		}
		for i, wl := range ansi.Wrap(line, inner) {
			prefix := cellIndent + "  "
			if i > 0 {
				prefix += "  "
			}
			lines = append(lines, prefix+wl+ansi.Reset)
		}
	}

	return lines
}

func cellNotice(message string, color ansi.Color, width int) []string {
	lines := indentWrap(colored(color, markdown.Sanitize(message)), width)
	lines = append(lines, "")
	return lines
}

func cellError(title, message string, width int) []string {
	t := theme.Default

	inner := width - len(cellIndent) - 2
	if inner < 10 {
		inner = 10
	}

	var lines []string
	first := true

	for _, wl := range ansi.Wrap(fg(t.Red)+ansi.Bold+markdown.Sanitize(title), inner) {
		prefix := "  "
		if first {
			prefix = colored(t.Red, "● ")
			first = false
		}
		lines = append(lines, cellIndent+prefix+wl+ansi.Reset)
	}

	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		if line == "" {
			continue
		}
		for _, wl := range ansi.Wrap(dim(markdown.Sanitize(line)), inner) {
			lines = append(lines, cellIndent+"  "+wl)
		}
	}

	lines = append(lines, "")
	return lines
}

// cellPrompt renders a confirmation or elicitation request with a `▌` accent
// bar.
func cellPrompt(title, message, hint string, width int) []string {
	t := theme.Default
	bar := fg(t.Yellow) + "▌ " + ansi.Reset

	inner := width - len(cellIndent) - 2
	if inner < 10 {
		inner = 10
	}

	var lines []string

	if title != "" {
		for _, wl := range ansi.Wrap(ansi.Bold+markdown.Sanitize(title), inner) {
			lines = append(lines, cellIndent+bar+wl+ansi.Reset)
		}
		lines = append(lines, cellIndent+strings.TrimRight(bar, " "))
	}

	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		for _, wl := range ansi.Wrap(markdown.Sanitize(line), inner) {
			lines = append(lines, cellIndent+bar+wl+ansi.Reset)
		}
	}

	if hint != "" {
		lines = append(lines, cellIndent+bar+hint+ansi.Reset)
	}

	lines = append(lines, "")
	return lines
}

// cellTurnSeparator emits the between-turns rule with a work summary.
func cellTurnSeparator(elapsed string, tools, thoughts int, width int) []string {
	var parts []string
	if elapsed != "" {
		parts = append(parts, "worked for "+elapsed)
	}
	if tools > 0 {
		unit := "tools"
		if tools == 1 {
			unit = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s", tools, unit))
	}
	if thoughts > 0 {
		unit := "thoughts"
		if thoughts == 1 {
			unit = "thought"
		}
		parts = append(parts, fmt.Sprintf("%d %s", thoughts, unit))
	}

	label := strings.Join(parts, " · ")

	inner := width - 2*len(cellIndent)
	if inner < 10 {
		inner = 10
	}

	var line string
	if label == "" || ansi.Width(label)+8 > inner {
		line = strings.Repeat("─", inner)
	} else {
		rest := inner - ansi.Width(label) - 5
		line = "── " + label + " " + strings.Repeat("─", rest)
	}

	return []string{cellIndent + dim(line), ""}
}
