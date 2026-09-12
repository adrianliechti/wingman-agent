package code

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/tui/markdown"
	"github.com/adrianliechti/wingman-agent/pkg/tui/theme"
)

func (a *App) showCopyPicker() {
	response := lastAssistantText(a.agent.Messages(a.sessionID))
	if response == "" {
		a.showToast("No assistant response to copy", theme.Default.Yellow)
		return
	}
	targets := append([]markdown.CopyTarget{{Label: "Whole response", Text: response}}, markdown.CopyTargets(response)...)
	items := make([]PopupItem, len(targets))
	byID := make(map[string]string, len(targets))
	for i, target := range targets {
		id := fmt.Sprintf("copy:%d", i)
		preview := strings.TrimSpace(target.Text)
		if line, _, ok := strings.Cut(preview, "\n"); ok {
			preview = line
		}
		items[i] = PopupItem{ID: id, Label: target.Label, Detail: markdown.Sanitize(preview)}
		byID[id] = target.Text
	}
	a.popup = newPopup(popupList, "copy to clipboard", items, func(ids []string) {
		a.copyTextToClipboard(byID[ids[0]])
	})
	a.invalidate()
}

func (a *App) showExportPicker() {
	if transcriptMarkdown(a.agent.Messages(a.sessionID), a.isToolHidden) == "" {
		a.showToast("No conversation to export", theme.Default.Yellow)
		return
	}
	filename := "wingman-" + time.Now().Format("20060102-150405") + ".md"
	a.popup = newPopup(popupList, "export conversation", []PopupItem{
		{ID: "copy", Label: "Copy Markdown transcript", Detail: "Visible messages, reasoning summaries, and tool output"},
		{ID: "save", Label: "Save Markdown file", Detail: filename},
	}, func(ids []string) {
		if ids[0] == "copy" {
			a.copyTextToClipboard(transcriptMarkdown(a.agent.Messages(a.sessionID), a.isToolHidden))
		} else {
			a.exportTranscript(filename)
		}
	})
	a.invalidate()
}

func (a *App) exportTranscript(filename string) {
	if filename == "" {
		a.showExportPicker()
		return
	}
	document := transcriptMarkdown(a.agent.Messages(a.sessionID), a.isToolHidden)
	if document == "" {
		a.showToast("No conversation to export", theme.Default.Yellow)
		return
	}
	if len(filename) >= 2 && (filename[0] == '"' && filename[len(filename)-1] == '"' || filename[0] == '\'' && filename[len(filename)-1] == '\'') {
		filename = filename[1 : len(filename)-1]
	}
	path := resolveFilePath(filename, a.agent.Workspace().RootPath)
	id := a.sessionID
	go func() {
		err := writeTranscriptFile(path, document)
		a.post(func() {
			if a.sessionID != id {
				return
			}
			if err != nil {
				a.showToast("Export failed: "+err.Error(), theme.Default.Red)
			} else {
				a.showToast("Saved "+path, theme.Default.Green)
			}
		})
	}()
}

func writeTranscriptFile(path, document string) error {
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(document)
	closeErr := file.Close()
	if err != nil {
		os.Remove(path)
		return err
	}
	return closeErr
}

// Export source content rather than rendered terminal rows. Hidden context and
// opaque provider reasoning are excluded, and attachments are named without
// embedding their base64 payloads.
func transcriptMarkdown(messages []agent.Message, isToolHidden func(string) bool) string {
	var transcript strings.Builder
	for _, message := range messages {
		if message.Hidden || message.Role == agent.RoleSystem {
			continue
		}
		var body strings.Builder
		for _, content := range message.Content {
			if content.Hidden {
				continue
			}
			switch {
			case content.ToolResult != nil:
				result := content.ToolResult
				if isToolHidden != nil && isToolHidden(result.Name) {
					continue
				}
				fmt.Fprintf(&body, "### Tool: %s\n\n", markdown.Sanitize(result.Name))
				if result.Args != "" {
					body.WriteString(exportFence(result.Args, "json"))
				}
				body.WriteString(exportFence(result.Content, "text"))
			case content.ToolCall != nil:
				// Completed tool results carry both the arguments and output.
				continue
			case content.Reasoning != nil:
				if summary := content.Reasoning.Summary; summary != "" {
					fmt.Fprintf(&body, "### Reasoning summary\n\n%s\n\n", summary)
				}
			case content.File != nil:
				name := content.File.Name
				if name == "" {
					name = "image"
				}
				fmt.Fprintf(&body, "Attachment: %s\n\n", markdown.Sanitize(name))
			default:
				if text := content.AsText(); strings.TrimSpace(text) != "" {
					body.WriteString(text + "\n\n")
				}
			}
		}
		if body.Len() == 0 {
			continue
		}
		role := "Assistant"
		if message.Role == agent.RoleUser {
			role = "User"
		}
		fmt.Fprintf(&transcript, "## %s\n\n%s", role, body.String())
	}
	if transcript.Len() == 0 {
		return ""
	}
	return "# Wingman conversation\n\n" + transcript.String()
}

func exportFence(text, language string) string {
	longest, run := 0, 0
	for _, char := range text {
		if char == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return fence + language + "\n" + text + fence + "\n\n"
}
