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

		case MsgFocus:

			s.files.Notify()
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

		return
	}

	s.setSessionPhase(sid, "thinking")

	var lastUsage agent.Usage

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

				s.files.Notify()

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

		if u := a.Usage(sid); u != lastUsage {
			lastUsage = u
			s.sendSession(sid, Frame{
				Type:         EvtUsage,
				InputTokens:  u.InputTokens,
				CachedTokens: u.CachedTokens,
				OutputTokens: u.OutputTokens,
			})
		}
	}

	if u := a.Usage(sid); u != (agent.Usage{}) && u != lastUsage {
		s.sendSession(sid, Frame{
			Type:         EvtUsage,
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
		})
	}

	ws := s.workspace
	s.flushFiles()

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
	}
	if ws.LSP != nil {
		s.broadcast(Frame{Type: EvtDiagnosticsChanged})
	}

	saved := true
	if w, ok := a.(*coder.Agent); ok {
		saved = w.Save(sid) == nil
	}
	if saved && len(a.Messages(sid)) > 0 {
		s.broadcast(Frame{Type: EvtSessionsChanged})
	}

	s.setSessionPhase(sid, "idle")
}
