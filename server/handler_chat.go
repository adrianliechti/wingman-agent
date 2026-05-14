package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/session"
	"github.com/adrianliechti/wingman-agent/pkg/tui"
)

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	s.wsMu.Lock()
	s.wsConns[conn] = struct{}{}
	s.wsMu.Unlock()

	defer func() {
		s.wsMu.Lock()
		delete(s.wsConns, conn)
		s.wsMu.Unlock()
	}()

	for _, sess := range s.allSessions() {
		if len(sess.Agent.Messages) == 0 {
			continue
		}
		sess.sendState()
	}

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
			sess := s.getOrCreateSession(msg.SessionID)
			go s.handleSend(sess, msg)

		case MsgCancel:
			if sess := s.getSession(msg.SessionID); sess != nil {
				sess.cancel()
			}
		}
	}
}

func (s *Server) handleSend(sess *Session, msg ClientMessage) {
	// Server-lifetime ctx, not the WS request ctx: a tab refresh or WS
	// reconnect must not abort an in-flight agent turn.
	streamCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	// agent.Send is not concurrent-safe per agent (mutates Messages); the
	// stream-slot claim must be atomic to avoid two WS connections racing
	// past a check-then-act guard.
	sess.mu.Lock()
	if sess.streamCancel != nil {
		sess.mu.Unlock()
		sess.send(Frame{Type: EvtError, Message: "session is busy"})
		return
	}
	sess.streamCancel = cancel
	sess.mu.Unlock()

	defer func() {
		sess.mu.Lock()
		sess.streamCancel = nil
		sess.mu.Unlock()
	}()

	var input []agent.Content

	if msg.Text != "" {
		text := s.resolveSkill(msg.Text)
		input = append(input, agent.Content{Text: text})
	}

	for _, f := range msg.Files {
		input = append(input, agent.Content{Text: fmt.Sprintf("[File: %s]", f)})
	}

	sess.setPhase("thinking")

	for evMsg, err := range sess.Agent.Send(streamCtx, input) {
		if err != nil {
			text := err.Error()
			if errors.Is(err, context.Canceled) {
				text = "Cancelled"
			}
			sess.send(Frame{Type: EvtError, Message: text})
			break
		}

		for _, c := range evMsg.Content {
			switch {
			case c.ToolCall != nil:
				sess.send(Frame{
					Type: EvtToolCall,
					ID:   c.ToolCall.ID,
					Name: c.ToolCall.Name,
					Args: c.ToolCall.Args,
					Hint: tui.ExtractToolHint(c.ToolCall.Args, c.ToolCall.Name),
				})
				sess.setPhase("tool_running")

			case c.ToolResult != nil:
				sess.send(Frame{
					Type:    EvtToolResult,
					ID:      c.ToolResult.ID,
					Name:    c.ToolResult.Name,
					Content: c.ToolResult.Content,
				})

			case c.Reasoning != nil && c.Reasoning.Summary != "":
				sess.setPhase("thinking")
				sess.send(Frame{
					Type: EvtReasoningDelta,
					ID:   c.Reasoning.ID,
					Text: c.Reasoning.Summary,
				})

			case c.Text != "":
				sess.setPhase("streaming")
				sess.send(Frame{Type: EvtTextDelta, Text: c.Text})
			}
		}

		u := sess.Agent.Usage
		sess.send(Frame{
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

	state := agent.State{
		Messages: sess.Agent.Messages,
		Usage:    sess.Agent.Usage,
	}
	if err := session.Save(s.sessionsDir, sess.ID, state); err == nil && len(state.Messages) > 0 {
		s.broadcast(Frame{Type: EvtSessionsChanged})
	}

	sess.setPhase("idle")
}
