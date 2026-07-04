package agent

import (
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

func toTools(tools []tool.Tool) []responses.ToolUnionParam {
	var result []responses.ToolUnionParam

	for _, t := range tools {
		if t.Name == "" {
			continue
		}

		f := &responses.FunctionToolParam{
			Name:       t.Name,
			Parameters: t.Parameters,
			Strict:     openai.Bool(false),
		}

		if t.Description != "" {
			f.Description = openai.String(t.Description)
		}

		result = append(result, responses.ToolUnionParam{
			OfFunction: f,
		})
	}

	return result
}

func toInput(messages []Message) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam

	for _, m := range messages {
		switch m.Role {
		case RoleAssistant:
			items = append(items, assistantToInput(m)...)
		case RoleSystem, RoleUser:
			items = append(items, userToInput(m)...)
		}
	}

	return items
}

func userToInput(m Message) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam

	input := &responses.ResponseInputItemMessageParam{
		Role: string(m.Role),
	}

	for _, c := range m.Content {
		if c.Text != "" {
			input.Content = append(input.Content, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: c.Text},
			})
		}

		if c.File != nil && c.File.Data != "" {
			input.Content = append(input.Content, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: openai.String(c.File.Data),
					Detail:   responses.ResponseInputImageDetailAuto,
				},
			})
		}

		if c.ToolResult != nil && c.ToolResult.ID != "" {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: c.ToolResult.ID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(c.ToolResult.Content),
					},
				},
			})
		}
	}

	if len(input.Content) > 0 {
		items = append(items, responses.ResponseInputItemUnionParam{OfInputMessage: input})
	}

	return items
}

func assistantToInput(m Message) []responses.ResponseInputItemUnionParam {
	var items []responses.ResponseInputItemUnionParam
	output := &responses.ResponseOutputMessageParam{}

	// Reasoning items must precede the output they belong to on the wire.
	for _, c := range m.Content {
		if c.Reasoning != nil {
			if p := reasoningToInput(c.Reasoning); p != nil {
				items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: p})
			}
		}
	}

	for _, c := range m.Content {
		if c.Text != "" {
			output.Content = append(output.Content, responses.ResponseOutputMessageContentUnionParam{
				OfOutputText: &responses.ResponseOutputTextParam{Text: c.Text},
			})
		}

		if c.Refusal != "" {
			output.Content = append(output.Content, responses.ResponseOutputMessageContentUnionParam{
				OfRefusal: &responses.ResponseOutputRefusalParam{Refusal: c.Refusal},
			})
		}
	}

	if len(output.Content) > 0 {
		items = append(items, responses.ResponseInputItemUnionParam{OfOutputMessage: output})
	}

	for _, c := range m.Content {
		if c.ToolCall != nil && c.ToolCall.ID != "" {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID:    c.ToolCall.ID,
					Name:      c.ToolCall.Name,
					Arguments: c.ToolCall.Args,
				},
			})
		}

		if c.ToolResult != nil && c.ToolResult.ID != "" {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: c.ToolResult.ID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(c.ToolResult.Content),
					},
				},
			})
		}
	}

	// Images attached to tool results travel as a user input message, since
	// function call outputs are string-only on the wire.
	attachments := &responses.ResponseInputItemMessageParam{Role: "user"}
	for _, c := range m.Content {
		if c.File != nil && c.File.Data != "" {
			attachments.Content = append(attachments.Content, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					ImageURL: openai.String(c.File.Data),
					Detail:   responses.ResponseInputImageDetailAuto,
				},
			})
		}
	}
	if len(attachments.Content) > 0 {
		items = append(items, responses.ResponseInputItemUnionParam{OfInputMessage: attachments})
	}

	return items
}

// reasoningToInput replays a reasoning item only when its opaque payload can
// actually be used: the provider requires the original item ID plus encrypted
// content. Payloads from other models never reach this point — the agent loop
// purges them from the transcript when the session model changes.
func reasoningToInput(r *Reasoning) *responses.ResponseReasoningItemParam {
	if r == nil || r.Content == "" || r.ID == "" {
		return nil
	}

	p := &responses.ResponseReasoningItemParam{
		ID:               r.ID,
		EncryptedContent: openai.String(r.Content),
	}

	if r.Summary != "" {
		p.Summary = []responses.ResponseReasoningItemSummaryParam{
			{Text: r.Summary},
		}
	}

	return p
}

func responseToUsage(r responses.Response) Usage {
	return Usage{
		InputTokens:  r.Usage.InputTokens,
		CachedTokens: r.Usage.InputTokensDetails.CachedTokens,
		OutputTokens: r.Usage.OutputTokens,
	}
}

func toMessages(items []responses.ResponseInputItemUnionParam) []Message {
	var messages []Message
	toolCallsByID := make(map[string]ToolCall)

	for _, item := range items {
		switch {
		case item.OfMessage != nil:
			if m, ok := fromEasyInput(item.OfMessage); ok {
				messages = append(messages, m)
			}

		case item.OfInputMessage != nil:
			if m, ok := fromInput(item.OfInputMessage); ok {
				messages = append(messages, m)
			}

		case item.OfOutputMessage != nil:
			if m, ok := fromOutput(item.OfOutputMessage); ok {
				messages = append(messages, m)
			}

		case item.OfFunctionCall != nil:
			tc := ToolCall{
				ID:   item.OfFunctionCall.CallID,
				Name: item.OfFunctionCall.Name,
				Args: item.OfFunctionCall.Arguments,
			}
			toolCallsByID[tc.ID] = tc
			messages = append(messages, Message{
				Role:    RoleAssistant,
				Content: []Content{{ToolCall: &tc}},
			})

		case item.OfFunctionCallOutput != nil:
			tc := toolCallsByID[item.OfFunctionCallOutput.CallID]
			tr := ToolResult{
				ID:      item.OfFunctionCallOutput.CallID,
				Name:    tc.Name,
				Args:    tc.Args,
				Content: item.OfFunctionCallOutput.Output.OfString.Value,
			}
			messages = append(messages, Message{
				Role:    RoleAssistant,
				Content: []Content{{ToolResult: &tr}},
			})

		case item.OfReasoning != nil:
			if m, ok := fromReasoning(item.OfReasoning); ok {
				messages = append(messages, m)
			}
		}
	}

	return messages
}

func fromEasyInput(m *responses.EasyInputMessageParam) (Message, bool) {
	if m == nil {
		return Message{}, false
	}

	contents := inputContentToContents(m.Content.OfInputItemContentList)
	if text := m.Content.OfString.Value; text != "" {
		contents = append(contents, Content{Text: text})
	}

	if len(contents) == 0 {
		return Message{}, false
	}

	role := MessageRole(m.Role)
	switch m.Role {
	case responses.EasyInputMessageRoleAssistant:
		role = RoleAssistant
	case responses.EasyInputMessageRoleSystem, responses.EasyInputMessageRoleDeveloper:
		role = RoleSystem
	default:
		role = RoleUser
	}

	return Message{Role: role, Content: contents}, true
}

func fromInput(m *responses.ResponseInputItemMessageParam) (Message, bool) {
	if m == nil {
		return Message{}, false
	}

	contents := inputContentToContents(m.Content)
	if len(contents) == 0 {
		return Message{}, false
	}

	role := MessageRole(m.Role)
	switch m.Role {
	case string(responses.ResponseInputMessageItemRoleSystem), string(responses.ResponseInputMessageItemRoleDeveloper):
		role = RoleSystem
	case string(responses.ResponseInputMessageItemRoleUser):
		role = RoleUser
	}

	return Message{Role: role, Content: contents}, true
}

func fromOutput(m *responses.ResponseOutputMessageParam) (Message, bool) {
	if m == nil {
		return Message{}, false
	}

	var contents []Content
	for _, part := range m.Content {
		if text := part.OfOutputText; text != nil && text.Text != "" {
			contents = append(contents, Content{Text: text.Text})
		}

		if refusal := part.OfRefusal; refusal != nil && refusal.Refusal != "" {
			contents = append(contents, Content{Refusal: refusal.Refusal})
		}
	}

	if len(contents) == 0 {
		return Message{}, false
	}

	return Message{Role: RoleAssistant, Content: contents}, true
}

func inputContentToContents(contentList responses.ResponseInputMessageContentListParam) []Content {
	var contents []Content

	for _, part := range contentList {
		if text := part.OfInputText; text != nil && text.Text != "" {
			contents = append(contents, Content{Text: text.Text})
		}

		if image := part.OfInputImage; image != nil && image.ImageURL.Value != "" {
			contents = append(contents, Content{File: &File{Data: image.ImageURL.Value}})
		}
	}

	return contents
}

func fromReasoning(r *responses.ResponseReasoningItemParam) (Message, bool) {
	if r == nil {
		return Message{}, false
	}

	c := Content{Reasoning: &Reasoning{ID: r.ID}}

	var parts []string
	for _, s := range r.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	c.Reasoning.Summary = strings.Join(parts, "\n\n")

	if r.EncryptedContent.Valid() {
		c.Reasoning.Content = r.EncryptedContent.Value
	}

	if c.Reasoning.Summary == "" && c.Reasoning.Content == "" {
		return Message{}, false
	}

	return Message{Role: RoleAssistant, Content: []Content{c}}, true
}
