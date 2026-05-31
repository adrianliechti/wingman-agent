package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

// session holds per-conversation state. Each ACP session maps 1:1 to a codex
// thread (sessionId == threadId); a single long-lived `codex app-server`
// process serves every session.
type session struct {
	id acp.SessionId

	mu            sync.Mutex
	modelID       string
	effort        string // "" or "default" means no effort override
	mode          string
	currentTurnID string
	cancelTurn    context.CancelFunc
}

func newSession(id acp.SessionId, model, effort string) *session {
	return &session{id: id, modelID: model, effort: effort, mode: defaultModeID}
}

// interrupt cancels the active turn. We rely on codex's turn/interrupt to
// trigger the natural turn/completed event that unblocks runTurn; the local
// cancel is a fallback if codex never responds (e.g., turn/start still pending).
func (s *session) interrupt(ctx context.Context, cc *codexClient) {
	s.mu.Lock()
	turnID := s.currentTurnID
	cancel := s.cancelTurn
	s.mu.Unlock()

	if turnID != "" {
		_ = cc.turnInterrupt(ctx, turnInterruptParams{ThreadID: string(s.id), TurnID: turnID})
	}
	if cancel != nil {
		cancel()
	}
}

func (s *session) runTurn(ctx context.Context, conn *acp.AgentSideConnection, cc *codexClient, prompt []acp.ContentBlock) (acp.StopReason, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	threadID := string(s.id)

	s.mu.Lock()
	s.cancelTurn = cancel
	model := s.modelID
	effort := s.effort
	mode := modeFor(s.mode)
	s.mu.Unlock()

	// Register handlers before turn/start so we cannot miss an early
	// turn/completed notification.
	disp := newEventDispatcher(turnCtx, conn, s.id)
	app := newApprover(turnCtx, conn, s.id)
	cc.setThreadHandlers(threadID, &threadHandlers{
		onNotification: disp.handle,
		onExecApproval: app.handleExec,
		onFileApproval: app.handleFile,
		onElicitation:  app.handleElicitation,
	})
	defer cc.setThreadHandlers(threadID, nil)

	params := turnStartParams{
		ThreadID:       threadID,
		Input:          promptToInput(prompt),
		ApprovalPolicy: mode.approvalPolicy,
		SandboxPolicy:  mode.sandboxPolicy,
	}
	if model != "" && model != "default" {
		params.Model = model
	}
	if effort != "" && effort != "default" {
		params.Effort = effort
	}

	resp, err := cc.turnStart(turnCtx, params)
	if err != nil {
		if turnCtx.Err() != nil {
			return acp.StopReasonCancelled, nil
		}
		return "", fmt.Errorf("turn/start: %w", err)
	}

	s.mu.Lock()
	s.currentTurnID = resp.Turn.ID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.currentTurnID = ""
		s.cancelTurn = nil
		s.mu.Unlock()
	}()

	// A turn that completes synchronously inside turn/start (or whose
	// completion event already raced into disp.done) is handled by the same
	// select below.
	select {
	case <-turnCtx.Done():
		return acp.StopReasonCancelled, nil
	case tc := <-disp.done:
		return stopReasonFor(tc.Turn.Status), nil
	}
}

// streamThreadHistory replays a resumed thread as a sequence of session
// updates so the client can rehydrate UI state. Only the item types that carry
// useful display content are emitted; ephemeral runtime details (mcp startup,
// review-mode transitions, image generation, etc.) are skipped because they
// don't round-trip cleanly through ACP's update vocabulary.
func streamThreadHistory(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId, turns []rawTurn, toolOutputs map[string]string) {
	send := func(u acp.SessionUpdate) {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
	}
	for _, turn := range turns {
		for _, raw := range turn.Items {
			replayItem(send, raw, toolOutputs)
		}
	}
}

// replayToolResult emits a completion update after a replayed StartToolCall so
// the client surfaces the tool's output. A start call on its own only yields
// the tool invocation; the client produces a tool result from a terminal-status
// update.
func replayToolResult(send func(acp.SessionUpdate), id, status string, content []acp.ToolCallContent) {
	st := toolStatusFor(status)
	if st != acp.ToolCallStatusCompleted && st != acp.ToolCallStatusFailed {
		return
	}
	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(st)}
	if len(content) > 0 {
		opts = append(opts, acp.WithUpdateContent(content))
	}
	send(acp.UpdateToolCall(acp.ToolCallId(id), opts...))
}

// replayToolText surfaces a tool's recovered output (from the rollout log) as a
// completed tool result. thread/resume omits tool output, so the rollout is the
// only source on replay; tool ids match the rollout's call_id.
func replayToolText(send func(acp.SessionUpdate), id, status string, outputs map[string]string) {
	out := outputs[id]
	if out == "" {
		return
	}
	replayToolResult(send, id, status, []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(out))})
}

func replayItem(send func(acp.SessionUpdate), raw json.RawMessage, toolOutputs map[string]string) {
	var probe struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Type == "" {
		return
	}
	switch probe.Type {
	case "userMessage":
		var it struct {
			Content []json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &it)
		for _, input := range it.Content {
			if block, ok := userInputToBlock(input); ok {
				send(acp.UpdateUserMessage(block))
			}
		}

	case "agentMessage":
		var it struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &it)
		if it.Text != "" {
			send(acp.UpdateAgentMessageText(it.Text))
		}

	case "reasoning":
		var it struct {
			Summary []string `json:"summary"`
			Content []string `json:"content"`
		}
		_ = json.Unmarshal(raw, &it)
		parts := it.Summary
		if len(parts) == 0 {
			parts = it.Content
		}
		for _, p := range parts {
			if p != "" {
				send(acp.UpdateAgentThoughtText(p))
			}
		}

	case "plan":
		var it struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &it)
		if it.Text != "" {
			send(acp.UpdateAgentMessageText("Plan:\n" + it.Text))
		}

	case "commandExecution":
		var it struct {
			Command        string          `json:"command"`
			Cwd            string          `json:"cwd"`
			Status         string          `json:"status"`
			CommandActions []commandAction `json:"commandActions"`
		}
		_ = json.Unmarshal(raw, &it)
		if title, kind, locs, ok := commandActionToolCall(it.CommandActions); ok {
			opts := []acp.ToolCallStartOpt{
				acp.WithStartKind(kind),
				acp.WithStartStatus(toolStatusFor(it.Status)),
			}
			if len(locs) > 0 {
				opts = append(opts, acp.WithStartLocations(locs))
			}
			send(acp.StartToolCall(acp.ToolCallId(probe.ID), title, opts...))
			break
		}
		title := stripShellPrefix(it.Command)
		if title == "" {
			title = "Run command"
		}
		send(acp.StartToolCall(acp.ToolCallId(probe.ID), title,
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(toolStatusFor(it.Status)),
			acp.WithStartRawInput(map[string]any{"command": it.Command, "cwd": it.Cwd}),
		))
		replayToolText(send, probe.ID, it.Status, toolOutputs)

	case "fileChange":
		var it struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &it)
		send(acp.StartToolCall(acp.ToolCallId(probe.ID), "Editing files",
			acp.WithStartKind(acp.ToolKindEdit),
			acp.WithStartStatus(toolStatusFor(it.Status)),
		))
		if content := fileChangeContent(raw); len(content) > 0 {
			replayToolResult(send, probe.ID, it.Status, content)
		}

	case "mcpToolCall":
		var it struct {
			Server string          `json:"server"`
			Tool   string          `json:"tool"`
			Args   json.RawMessage `json:"arguments"`
			Status string          `json:"status"`
		}
		_ = json.Unmarshal(raw, &it)
		var args map[string]any
		_ = json.Unmarshal(it.Args, &args)
		send(acp.StartToolCall(acp.ToolCallId(probe.ID), fmt.Sprintf("mcp.%s.%s", it.Server, it.Tool),
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(toolStatusFor(it.Status)),
			acp.WithStartRawInput(map[string]any{"server": it.Server, "tool": it.Tool, "arguments": args}),
		))
		replayToolText(send, probe.ID, it.Status, toolOutputs)

	case "dynamicToolCall":
		var it struct {
			Tool   string          `json:"tool"`
			Args   json.RawMessage `json:"arguments"`
			Status string          `json:"status"`
		}
		_ = json.Unmarshal(raw, &it)
		var args map[string]any
		_ = json.Unmarshal(it.Args, &args)
		send(acp.StartToolCall(acp.ToolCallId(probe.ID), it.Tool,
			acp.WithStartKind(acp.ToolKindExecute),
			acp.WithStartStatus(toolStatusFor(it.Status)),
			acp.WithStartRawInput(map[string]any{"arguments": args}),
		))
		replayToolText(send, probe.ID, it.Status, toolOutputs)

	case "webSearch":
		var it struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &it)
		title := "Web search"
		if it.Query != "" {
			title = "Web search: " + it.Query
		}
		send(acp.StartToolCall(acp.ToolCallId(probe.ID), title,
			acp.WithStartKind(acp.ToolKindSearch),
			acp.WithStartStatus(acp.ToolCallStatusCompleted),
		))
		replayToolText(send, probe.ID, "completed", toolOutputs)
	}
}

// userInputToBlock reverses promptToInput for replay of historical user
// messages. Unsupported variants (audio, unknown skill payloads) are dropped.
func userInputToBlock(raw json.RawMessage) (acp.ContentBlock, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return acp.ContentBlock{}, false
	}
	switch probe.Type {
	case "text":
		var it struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &it)
		if it.Text == "" {
			return acp.ContentBlock{}, false
		}
		return acp.TextBlock(it.Text), true
	case "image":
		var it struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &it)
		return acp.TextBlock(formatURIAsLink("image", it.URL)), true
	case "localImage":
		var it struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &it)
		uri := it.Path
		if !strings.HasPrefix(uri, "file://") {
			uri = "file://" + uri
		}
		return acp.TextBlock(formatURIAsLink("", uri)), true
	case "skill":
		var it struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &it)
		return acp.TextBlock(fmt.Sprintf("skill:%s (%s)", it.Name, it.Path)), true
	}
	return acp.ContentBlock{}, false
}

func stopReasonFor(status string) acp.StopReason {
	switch status {
	case "interrupted":
		return acp.StopReasonCancelled
	case "failed":
		return acp.StopReasonRefusal
	default:
		return acp.StopReasonEndTurn
	}
}

func promptToInput(blocks []acp.ContentBlock) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			out = append(out, textInput(b.Text.Text))
		case b.Image != nil:
			img := b.Image
			switch {
			case img.Uri != nil && *img.Uri != "":
				out = append(out, map[string]any{"type": "image", "url": *img.Uri})
			case img.Data != "":
				out = append(out, map[string]any{
					"type": "image",
					"url":  "data:" + img.MimeType + ";base64," + img.Data,
				})
			}
		case b.ResourceLink != nil:
			out = append(out, textInput(formatURIAsLink(b.ResourceLink.Name, b.ResourceLink.Uri)))
		case b.Resource != nil:
			if text, uri := resourceContents(b.Resource.Resource); text != "" {
				link := formatURIAsLink("", uri)
				body := fmt.Sprintf("%s\n<context ref=%q>\n%s\n</context>", link, uri, text)
				out = append(out, textInput(body))
			}
		}
	}
	return out
}

// textInput builds a codex `userInput` variant. text_elements must be present
// (and non-null) per the codex schema even when empty.
func textInput(text string) map[string]any {
	return map[string]any{"type": "text", "text": text, "text_elements": []any{}}
}

// resourceContents extracts a (text, uri) pair from an embedded resource. The
// SDK encodes the resource as a tagged union, so we marshal + re-parse.
func resourceContents(res any) (text, uri string) {
	b, err := json.Marshal(res)
	if err != nil {
		return "", ""
	}
	var v struct {
		Text string `json:"text"`
		URI  string `json:"uri"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Text, v.URI
}

func formatURIAsLink(name, uri string) string {
	if name != "" {
		return fmt.Sprintf("[@%s](%s)", name, uri)
	}
	if p, ok := strings.CutPrefix(uri, "file://"); ok {
		return fmt.Sprintf("[@%s](%s)", path.Base(p), uri)
	}
	return uri
}
