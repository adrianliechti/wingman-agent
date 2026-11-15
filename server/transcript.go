package server

import (
	"fmt"
	"slices"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

// transcriptEntries projects visible history directly, preserving the identities
// assigned by the previous history reader, including tool-result images.
func transcriptEntries(messages []harness.Message) []TranscriptEntry {
	entries := []TranscriptEntry{}
	mi := 0
	for _, message := range messages {
		if message.Hidden {
			continue
		}
		messageIndex := mi
		mi++
		if message.Role == harness.RoleUser {
			input := code.TurnInput{}
			for _, part := range message.Content {
				if part.Hidden {
					continue
				}
				if text := part.AsText(); text != "" {
					input.Content = append(input.Content, harness.Content{Text: text})
				}
				if part.File != nil && part.File.Data != "" {
					input.Content = append(input.Content, harness.Content{File: &harness.File{Data: part.File.Data}})
				}
				if result := part.ToolResult; result != nil {
					for _, image := range result.Content.ImageURLs() {
						input.Content = append(input.Content, harness.Content{File: &harness.File{Data: image}})
					}
				}
			}
			visible := visibleInput(input)
			if visible.Text != "" || len(visible.Files)+len(visible.Images) > 0 {
				id := fmt.Sprintf("history:%d:user", messageIndex)
				if message.InputID != "" {
					id = "input:" + message.InputID
				}
				entries = append(entries, TranscriptEntry{ID: id, InputID: message.InputID, Type: "user", Content: visible.Text, Files: visible.Files, Images: visible.Images})
			}
			continue
		}
		ci := 0
		for _, part := range message.Content {
			if part.Hidden {
				continue
			}
			id := fmt.Sprintf("history:%d:%d", messageIndex, ci)
			ci++
			if text := part.AsText(); text != "" {
				textID := id + ":text"
				if part.TextID != "" {
					textID = "assistant:" + part.TextID
				}
				entries = append(entries, TranscriptEntry{ID: textID, Type: "assistant", Content: text, MessageID: part.TextID})
			}
			if part.File != nil && part.File.Data != "" {
				entries = append(entries, TranscriptEntry{ID: id + ":image", Type: "assistant", Images: []string{part.File.Data}})
			}
			if reason := part.Reasoning; reason != nil && reason.Summary != "" {
				reasonID := id + ":reasoning"
				if reason.ID != "" {
					reasonID = "reasoning:" + reason.ID
				}
				entries = append(entries, TranscriptEntry{ID: reasonID, Type: "reasoning", Content: reason.Summary, ReasoningID: reason.ID})
			}
			if call := part.ToolCall; call != nil {
				display := displayTool(call.Name, call.Kind, call.Args, call.Locations, call.Presentation)
				toolID := "tool:" + call.ID
				if call.ID == "" {
					toolID = id + ":tool"
				}
				entries = append(entries, TranscriptEntry{ID: toolID, Type: "tool", ToolID: call.ID, ToolName: display.name, ToolKind: display.kind, ToolArgs: display.args, ToolLocations: display.locations, ToolHint: display.hint})
			}
			if result := part.ToolResult; result != nil {
				content := result.Content.Text()
				if i := slices.IndexFunc(entries, func(e TranscriptEntry) bool { return result.ID != "" && e.ToolID == result.ID }); i >= 0 {
					entries[i].ToolResult = &content
				} else {
					display := displayTool(result.Name, result.Kind, result.Args, result.Locations, result.Presentation)
					entries = append(entries, TranscriptEntry{ID: id + ":result", Type: "tool", ToolID: result.ID, ToolName: display.name, ToolKind: display.kind, ToolArgs: display.args, ToolLocations: display.locations, ToolResult: &content})
				}
				for _, image := range result.Content.ImageURLs() {
					imageID := fmt.Sprintf("history:%d:%d:image", messageIndex, ci)
					ci++
					entries = append(entries, TranscriptEntry{ID: imageID, Type: "assistant", Images: []string{image}})
				}
			}
		}
	}
	return entries
}
