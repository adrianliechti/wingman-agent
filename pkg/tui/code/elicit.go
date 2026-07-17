package code

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) Confirm(ctx context.Context, message string) (bool, error) {
	if a.confirmAll.Load() {
		return true, nil
	}

	a.elicitMu.Lock()
	defer a.elicitMu.Unlock()

	a.promptResponse = make(chan bool, 1)

	t := theme.Default
	hint := fmt.Sprintf("%s approve · %s deny · %s always",
		colored(t.Green, "y"), colored(t.Red, "n"), colored(t.Cyan, "a"))

	a.post(func() {
		a.promptActive = true
		a.flushCells(cellPrompt("Confirm command", message, hint, a.width()))
		a.editor.SetPlaceholder("y/n/a")
		a.invalidate()
	})

	defer a.post(func() {
		a.promptActive = false
		a.editor.SetPlaceholder("Ask anything...")
		a.invalidate()
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

	a.post(func() {
		a.askActive = true
		a.flushCells(cellPrompt("Question", prompt, "", a.width()))
		a.editor.SetPlaceholder(placeholder)
		a.invalidate()
	})

	defer a.post(func() {
		a.askActive = false
		a.editor.SetPlaceholder("Ask anything...")
		a.invalidate()
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
