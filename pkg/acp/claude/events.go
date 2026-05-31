package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

// emitAssistant translates an assistant message (text / thinking / tool_use)
// into ACP session updates.
func emitAssistant(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage) error {
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
			title := b.Name
			if title == "" {
				title = "Tool call"
			}
			var input map[string]any
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			update = acp.StartToolCall(
				acp.ToolCallId(b.ID),
				title,
				acp.WithStartKind(toolKindFor(b.Name)),
				acp.WithStartStatus(acp.ToolCallStatusInProgress),
				acp.WithStartRawInput(input),
			)
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
func emitToolResults(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage) error {
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
		status := acp.ToolCallStatusCompleted
		if b.IsError {
			status = acp.ToolCallStatusFailed
		}
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
		if text := extractToolResultText(b.Content); text != "" {
			opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{
				acp.ToolContent(acp.TextBlock(text)),
			}))
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
