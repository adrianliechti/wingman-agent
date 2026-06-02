package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// emitAssistant translates an assistant message (text / thinking / tool_use)
// into ACP session updates.
func emitAssistant(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage, cwd string, cache toolUseCache) error {
	if len(raw) == 0 {
		return nil
	}
	var m cliMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse assistant message: %w", err)
	}
	for _, b := range m.Content {
		var update acp.SessionUpdate
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			update = acp.UpdateAgentMessageText(b.Text)
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			update = acp.UpdateAgentThoughtText(b.Thinking)
		case "tool_use":
			if cache != nil && b.ID != "" {
				cache[b.ID] = b.Name
			}
			if isPlanTool(b.Name) {
				entries, ok := planEntriesFromTodoWrite(b.Input)
				if !ok {
					continue
				}
				update = acp.UpdatePlan(entries...)
				break
			}
			info := toolInfoFromToolUse(b.Name, b.Input, cwd)
			var input any
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			opts := []acp.ToolCallStartOpt{
				acp.WithStartKind(info.kind),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
				acp.WithStartRawInput(input),
			}
			if len(info.content) > 0 {
				opts = append(opts, acp.WithStartContent(info.content))
			}
			if len(info.locations) > 0 {
				opts = append(opts, acp.WithStartLocations(info.locations))
			}
			update = acp.StartToolCall(acp.ToolCallId(b.ID), info.title, opts...)
		default:
			continue
		}
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    update,
		}); err != nil {
			return err
		}
	}
	return nil
}

// emitToolResults surfaces the tool_result blocks the CLI echoes back as
// ToolCallUpdate completions so the client sees tool output.
func emitToolResults(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage, cache toolUseCache) error {
	if len(raw) == 0 {
		return nil
	}
	var m cliMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil // best-effort; user echoes can be skipped on parse error
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		name := cache[b.ToolUseID]
		if isPlanTool(name) {
			continue
		}
		status := acp.ToolCallStatusCompleted
		if b.IsError {
			status = acp.ToolCallStatusFailed
		}
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
		if content := toolResultContent(name, b); len(content) > 0 {
			opts = append(opts, acp.WithUpdateContent(content))
		}
		if len(b.Content) > 0 {
			var rawOutput any
			if json.Unmarshal(b.Content, &rawOutput) == nil {
				opts = append(opts, acp.WithUpdateRawOutput(rawOutput))
			}
		}
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateToolCall(acp.ToolCallId(b.ToolUseID), opts...),
		}); err != nil {
			return err
		}
	}
	return nil
}

func toolResultContent(name string, b cliMsgBlock) []acp.ToolCallContent {
	text := extractToolResultText(b.Content)
	if b.IsError {
		if text == "" {
			return nil
		}
		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(codeFence(text)))}
	}
	switch name {
	case "Read":
		if text == "" {
			return nil
		}
		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(markdownEscape(text)))}
	case "Bash":
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("```console\n" + strings.TrimRight(text, "\n") + "\n```"))}
	case "Edit", "Write", "MultiEdit":
		return nil
	default:
		if text == "" {
			return nil
		}
		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}
	}
}

func codeFence(text string) string {
	return "```\n" + text + "\n```"
}

// extractToolResultText flattens tool_result.content (string OR
// [{type:"text",text:...}, ...]) into a single string.
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []cliMsgBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, blk := range blocks {
			if blk.Type == "text" {
				out += blk.Text
			}
		}
		return out
	}
	return string(raw)
}

func toolKindFor(name string) acp.ToolKind {
	switch name {
	case "Read", "Glob", "Grep", "WebFetch", "WebSearch":
		return acp.ToolKindRead
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return acp.ToolKindEdit
	case "Bash", "BashOutput", "KillShell":
		return acp.ToolKindExecute
	case "Task":
		return acp.ToolKindThink
	}
	return acp.ToolKindOther
}
