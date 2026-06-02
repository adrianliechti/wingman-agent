package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	coder "github.com/adrianliechti/wingman-agent/pkg/code/agent"
)

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	// Default is 32KB — too small for image data URLs. Allow up to 32MB
	// so pasted/attached screenshots don't trip the read limit and tear
	// the WS down (which the client then auto-reconnects, looking like
	// a page reload).
	conn.SetReadLimit(32 << 20)

	client := newWSClient(conn)
	s.wsMu.Lock()
	s.wsConns[conn] = client
	s.wsMu.Unlock()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		client.run()
	}()

	defer func() {
		s.wsMu.Lock()
		delete(s.wsConns, conn)
		s.wsMu.Unlock()
		client.close()
		<-writerDone
	}()

	// Request ctx is used only for the conn.Read loop. Agent turns must
	// not inherit from r.Context() — see Server.ctx.
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case MsgSend:
			if msg.SessionID == "" {
				continue
			}
			go s.handleSend(msg)

		case MsgCancel:
			if msg.SessionID == "" {
				continue
			}
			if a := s.activeAgent(); a != nil {
				a.Cancel(msg.SessionID)
			}

		case MsgPromptResponse:
			if msg.PromptID == "" {
				continue
			}
			s.resolvePrompt(msg)
		}
	}
}

func (s *Server) buildInput(msg ClientMessage) []agent.Content {
	var input []agent.Content
	if msg.Text != "" {
		text := s.resolveSkill(msg.Text)
		input = append(input, agent.Content{Text: text})
	}
	for _, f := range msg.Files {
		input = append(input, agent.Content{Text: fmt.Sprintf("[File: %s]", f)})
	}
	for _, img := range msg.Images {
		if img == "" {
			continue
		}
		input = append(input, agent.Content{File: &agent.File{Data: img}})
	}
	return input
}

// handleSend runs one turn through the active agent and streams events
// to all WS clients. Server-lifetime ctx (not the WS request ctx) so a
// tab refresh or WS reconnect doesn't abort an in-flight agent turn.
func (s *Server) handleSend(msg ClientMessage) {
	a := s.activeAgent()
	if a == nil {
		return
	}
	sid := msg.SessionID
	input := s.buildInput(msg)

	streamCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	stream := a.Send(streamCtx, sid, input)
	if stream == nil {
		// Turn already in flight for this session — input was queued
		// (wingman) or dropped (acp single-session contract).
		return
	}

	s.setSessionPhase(sid, "thinking")

	for evMsg, err := range stream {
		if err != nil {
			text := err.Error()
			if errors.Is(err, context.Canceled) {
				text = "Cancelled"
			}
			s.sendSession(sid, Frame{Type: EvtError, Message: text})
			break
		}

		for _, c := range evMsg.Content {
			switch {
			case c.ToolCall != nil:
				s.sendSession(sid, Frame{
					Type: EvtToolCall,
					ID:   c.ToolCall.ID,
					Name: c.ToolCall.Name,
					Args: c.ToolCall.Args,
					Hint: tool.ExtractHint(c.ToolCall.Args, c.ToolCall.Name),
				})
				s.setSessionPhase(sid, "tool_running")

			case c.ToolResult != nil:
				s.sendSession(sid, Frame{
					Type:    EvtToolResult,
					ID:      c.ToolResult.ID,
					Name:    c.ToolResult.Name,
					Content: c.ToolResult.Content,
				})

			case c.Reasoning != nil && c.Reasoning.Summary != "":
				s.setSessionPhase(sid, "thinking")
				s.sendSession(sid, Frame{
					Type: EvtReasoningDelta,
					ID:   c.Reasoning.ID,
					Text: c.Reasoning.Summary,
				})

			case c.Text != "":
				s.setSessionPhase(sid, "streaming")
				s.sendSession(sid, Frame{Type: EvtTextDelta, Text: c.Text})
			}
		}

		u := a.Usage(sid)
		s.sendSession(sid, Frame{
			Type:         EvtUsage,
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
		})
	}

	ws := s.workspace
	s.broadcast(Frame{Type: EvtFilesChanged})

	if ws.Rewind != nil {
		commitMsg := msg.Text
		if commitMsg == "" {
			commitMsg = "<unknown>"
		}
		go func() {
			if err := ws.Commit(commitMsg); err == nil {
				s.broadcast(Frame{Type: EvtCheckpointsChanged})
			}
		}()
		s.broadcast(Frame{Type: EvtDiffsChanged})
	}
	if ws.LSP != nil {
		s.broadcast(Frame{Type: EvtDiagnosticsChanged})
	}

	// Wingman persists per-session transcripts to disk; ACP servers store
	// their own state. Only wingman needs an explicit save here, but both
	// may have created/updated the on-disk session, so refresh the sidebar
	// for either backend once the turn produced messages.
	saved := true
	if w, ok := a.(*coder.Agent); ok {
		saved = w.Save(sid) == nil
	}
	if saved && len(a.Messages(sid)) > 0 {
		s.broadcast(Frame{Type: EvtSessionsChanged})
	}

	s.setSessionPhase(sid, "idle")
}
