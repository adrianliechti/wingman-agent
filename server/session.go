package server

import (
	"context"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

type Session struct {
	ID    string
	Agent *code.Agent

	server *Server

	mu           sync.Mutex
	streamCancel context.CancelFunc
	phase        string // "idle" | "thinking" | "streaming" | "tool_running"
}

func (s *Server) newSession(id string) *Session {
	a := s.workspace.NewAgent(s.config, nil)
	a.Config.Model = s.currentModel
	a.Config.Effort = s.currentEffort
	return &Session{ID: id, Agent: a, server: s}
}

func (sess *Session) send(f Frame) {
	f.Session = sess.ID
	sess.server.send(f)
}

func (sess *Session) sendState() {
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

func (sess *Session) setPhase(p string) {
	sess.mu.Lock()
	if sess.phase == p {
		sess.mu.Unlock()
		return
	}
	sess.phase = p
	sess.mu.Unlock()
	sess.send(Frame{Type: EvtPhase, Phase: p})
}

func (sess *Session) currentPhase() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.phase == "" {
		return "idle"
	}
	return sess.phase
}

func (sess *Session) cancel() {
	sess.mu.Lock()
	cancel := sess.streamCancel
	sess.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
