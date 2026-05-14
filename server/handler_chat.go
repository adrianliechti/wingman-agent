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

	// On connect: announce every in-memory session as one session_state frame.
	// Client uses it to populate per-session slots in one shot — no separate
	// session+messages+usage+phase chatter. Phase reflects the actual current
	// state so reconnecting mid-stream doesn't flash "idle" first.
	for _, sess := range s.allSessions() {
		u := sess.Agent.Usage
		sess.send(Frame{
			Type:         EvtSessionState,
			Phase:        sess.currentPhase(),
			Messages:     convertMessages(sess.Agent.Messages),
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
		})
	}

	ctx := r.Context()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		sess := s.getSession(msg.SessionID)
		if sess == nil {
			continue
		}

		switch msg.Type {
		case MsgSend:
			go s.handleSend(ctx, sess, msg)
		case MsgCancel:
			sess.cancel()
		}
	}
}

func (s *Server) handleSend(ctx context.Context, sess *Session, msg ClientMessage) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Atomic claim of the stream slot. agent.Send is not concurrent-safe per
	// agent (it mutates Messages), and two WS connections could otherwise
	// both race past a check-then-act busy guard.
	sess.mu.Lock()
	if sess.streamCancel != nil {
		sess.mu.Unlock()
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

	// Workspace-level changes (files touched, diffs shifted): every session's
	// view of the working dir is affected, so broadcast. Checkpoints are
	// per-session (each session has its own Rewind shadow repo), so tag.
	s.broadcast(Frame{Type: EvtFilesChanged})

	if sess.Agent.Rewind != nil {
		commitMsg := msg.Text
		if commitMsg == "" {
			commitMsg = "<unknown>"
		}
		go func() {
			if err := sess.Agent.Rewind.Commit(commitMsg); err == nil {
				sess.send(Frame{Type: EvtCheckpointsChanged})
			}
		}()
		s.broadcast(Frame{Type: EvtDiffsChanged})
	}
	if sess.Agent.LSP != nil {
		s.broadcast(Frame{Type: EvtDiagnosticsChanged})
	}

	state := agent.State{
		Messages: sess.Agent.Messages,
		Usage:    sess.Agent.Usage,
	}
	if err := session.Save(s.sessionsDir, sess.ID, state); err == nil && len(state.Messages) > 0 {
		s.broadcast(Frame{Type: EvtSessionsChanged})
	}

	sess.send(Frame{Type: EvtDone})
	sess.setPhase("idle")
}
