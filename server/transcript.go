package server

import (
	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func convertMessages(messages []harness.Message) []ConversationMessage {
	var result []ConversationMessage
	for _, m := range messages {
		if m.Hidden {
			continue
		}
		cm := ConversationMessage{Role: string(m.Role), InputID: m.InputID}
		for _, c := range m.Content {
			if c.Hidden {
				continue
			}
			cc := ConversationContent{}
			if c.AsText() != "" {
				cc.Text = c.AsText()
				cc.TextID = c.TextID
			}
			if c.File != nil && c.File.Data != "" {
				cc.Image = &ConversationImage{Data: c.File.Data, Name: c.File.Name}
			}
			if c.Reasoning != nil && c.Reasoning.Summary != "" {
				cc.Reasoning = &ConversationReasoning{ID: c.Reasoning.ID, Summary: c.Reasoning.Summary}
			}
			if c.ToolCall != nil {
				display := displayTool(
					c.ToolCall.Name, c.ToolCall.Kind, c.ToolCall.Args, c.ToolCall.Locations,
					c.ToolCall.Presentation,
				)
				cc.ToolCall = &ConversationTool{
					ID:        c.ToolCall.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Hint:      display.hint,
				}
			}
			if c.ToolResult != nil {
				display := displayTool(
					c.ToolResult.Name, c.ToolResult.Kind, c.ToolResult.Args, c.ToolResult.Locations,
					c.ToolResult.Presentation,
				)
				cc.ToolResult = &ConversationResult{
					ID:        c.ToolResult.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Content:   c.ToolResult.Content.Text(),
				}
			}
			cm.Content = append(cm.Content, cc)
			if c.ToolResult != nil {
				for _, url := range c.ToolResult.Content.ImageURLs() {
					cm.Content = append(cm.Content, ConversationContent{Image: &ConversationImage{Data: url}})
				}
			}
		}
		result = append(result, cm)
	}
	return result
}
