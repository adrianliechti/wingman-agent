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

	// On connect: announce every in-memory session so the client can hydrate
	// its per-session state without an extra REST roundtrip per session.
	for _, sess := range s.allSessions() {
		sess.sendMessage(SessionEvent{ID: sess.ID})
		if msgs := convertMessages(sess.Agent.Messages); len(msgs) > 0 {
			sess.sendMessage(MessagesEvent{Messages: msgs})
		}
		u := sess.Agent.Usage
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			sess.sendMessage(UsageEvent{
				InputTokens:  u.InputTokens,
				CachedTokens: u.CachedTokens,
				OutputTokens: u.OutputTokens,
			})
		}
		sess.sendMessage(PhaseEvent{Phase: "idle"})
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

	currentPhase := ""
	setPhase := func(p string) {
		if p == currentPhase {
			return
		}
		currentPhase = p
		sess.sendMessage(PhaseEvent{Phase: p})
	}

	setPhase("thinking")

	for evMsg, err := range sess.Agent.Send(streamCtx, input) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				sess.sendMessage(ErrorEvent{Message: "Cancelled"})
			} else {
				sess.sendMessage(ErrorEvent{Message: err.Error()})
			}
			break
		}

		for _, c := range evMsg.Content {
			switch {
			case c.ToolCall != nil:
				sess.sendMessage(ToolCallEvent{
					ID:   c.ToolCall.ID,
					Name: c.ToolCall.Name,
					Args: c.ToolCall.Args,
					Hint: tui.ExtractToolHint(c.ToolCall.Args, c.ToolCall.Name),
				})
				setPhase("tool_running")

			case c.ToolResult != nil:
				sess.sendMessage(ToolResultEvent{
					ID:      c.ToolResult.ID,
					Name:    c.ToolResult.Name,
					Content: c.ToolResult.Content,
				})

			case c.Reasoning != nil && c.Reasoning.Summary != "":
				setPhase("thinking")
				sess.sendMessage(ReasoningDeltaEvent{
					ID:   c.Reasoning.ID,
					Text: c.Reasoning.Summary,
				})

			case c.Text != "":
				setPhase("streaming")
				sess.sendMessage(TextDeltaEvent{Text: c.Text})
			}
		}

		u := sess.Agent.Usage
		sess.sendMessage(UsageEvent{
			InputTokens:  u.InputTokens,
			CachedTokens: u.CachedTokens,
			OutputTokens: u.OutputTokens,
		})
	}

	sess.sendMessage(FilesChangedEvent{})

	if sess.Agent.Rewind != nil {
		commitMsg := msg.Text
		if commitMsg == "" {
			commitMsg = "<unknown>"
		}
		go func() {
			if err := sess.Agent.Rewind.Commit(commitMsg); err == nil {
				sess.sendMessage(CheckpointsChangedEvent{})
			}
		}()
		sess.sendMessage(DiffsChangedEvent{})
	}
	if sess.Agent.LSP != nil {
		s.broadcast(DiagnosticsChangedEvent{})
	}

	state := agent.State{
		Messages: sess.Agent.Messages,
		Usage:    sess.Agent.Usage,
	}
	if err := session.Save(s.sessionsDir, sess.ID, state); err == nil && len(state.Messages) > 0 {
		s.broadcast(SessionsChangedEvent{})
	}

	sess.sendMessage(DoneEvent{})
	setPhase("idle")
}

