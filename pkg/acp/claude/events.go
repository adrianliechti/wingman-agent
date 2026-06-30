package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func emitStreamEvent(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var e streamEvent
	if err := json.Unmarshal(raw, &e); err != nil || e.Type != "content_block_delta" {
		return nil
	}
	var update acp.SessionUpdate
	switch e.Delta.Type {
	case "text_delta":
		if e.Delta.Text == "" {
			return nil
		}
		update = acp.UpdateAgentMessageText(e.Delta.Text)
	case "thinking_delta":
		if e.Delta.Thinking == "" {
			return nil
		}
		update = acp.UpdateAgentThoughtText(e.Delta.Thinking)
	default:
		return nil
	}
	return conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: update})
}

func emitAssistant(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage, cwd string, cache toolUseCache, tracker *toolCallTracker, streamed bool) error {
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
			if b.Text == "" || streamed {
				continue
			}
			update = acp.UpdateAgentMessageText(b.Text)
		case "thinking":
			if b.Thinking == "" || streamed {
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
			if err := emitToolUseCall(ctx, conn, sid, b, cwd, tracker); err != nil {
				return err
			}
			continue
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

// emitToolUseCall surfaces a streamed tool_use block as a tool_call, unless a
// concurrent permission request for the same id already claimed it (see
// toolCallTracker), in which case it sends a tool_call_update that refines
// the eagerly-emitted call with the now-complete info instead of duplicating
// it.
func emitToolUseCall(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, b cliMsgBlock, cwd string, tracker *toolCallTracker) error {
	send := func(u acp.SessionUpdate) error {
		return conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
	}
	start := func() error {
		return send(toolCallStartUpdate(b.ID, b.Name, b.Input, cwd, acp.ToolCallStatusInProgress))
	}

	if b.ID == "" || tracker == nil || !shouldEmitToolCall(b.Name) {
		return start()
	}

	refine := func() error {
		return send(toolCallRefineUpdate(b.ID, b.Name, b.Input, cwd, acp.ToolCallStatusInProgress))
	}
	return tracker.emit(b.ID, start, refine)
}

func emitToolResults(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, raw json.RawMessage, cache toolUseCache) error {
	if len(raw) == 0 {
		return nil
	}
	var m cliMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
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
	if name == "Bash" && !b.IsError {
		if blocks, ok := bashImageResultBlocks(b.Content); ok {
			return blocks
		}
	}
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

// bashImageResultBlocks handles a Bash tool_result whose content array
// contains a non-text block (e.g. an image from a command piping a base64
// data URI). extractToolResultText only collects "text" blocks, so without
// this, image output is silently dropped. ok is false for text-only or
// non-array content, telling the caller to fall back to the normal
// text-extraction path (which keeps the existing console code-fence
// formatting for plain Bash output).
func bashImageResultBlocks(raw json.RawMessage) ([]acp.ToolCallContent, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var parts []cliMsgBlock
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return nil, false
	}

	textOnly := true
	for _, p := range parts {
		if p.Type != "text" {
			textOnly = false
			break
		}
	}
	if textOnly {
		return nil, false
	}

	var blocks []acp.ToolCallContent
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, acp.ToolContent(acp.TextBlock(p.Text)))
			}
		case "image":
			if p.Source != nil && p.Source.Data != "" {
				blocks = append(blocks, acp.ToolContent(acp.ImageBlock(p.Source.Data, p.Source.MediaType)))
			}
		}
	}
	if len(blocks) == 0 {
		return nil, false
	}
	return blocks, true
}

func codeFence(text string) string {
	return "```\n" + text + "\n```"
}

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
