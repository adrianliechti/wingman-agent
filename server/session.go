package server

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// Session is one conversation bound to the server's Workspace. Lightweight:
// own Messages/Usage/PlanMode via code.NewAgent (which Derives its Config
// from the workspace's), plus per-conversation BFF state (stream cancel,
// phase).
type Session struct {
	ID    string
	Agent *code.Agent

	server *Server

	mu           sync.Mutex
	streamCancel context.CancelFunc
	phase        string // "idle" | "thinking" | "streaming" | "tool_running"
}

func newSessionID() string {
	return uuid.New().String()
}

// newSession spins up a fresh Agent against the server's Workspace and
// late-binds model/effort to server-wide settings — Workspace.NewAgent
// already wires Instructions, ContextMessages, and the truncation hook.
func (s *Server) newSession(id string) *Session {
	a := s.workspace.NewAgent(s.config, nil)
	// Every Send reads the current server-wide model/effort, so changing
	// the picker doesn't need to iterate sessions.
	a.Config.Model = s.currentModel
	a.Config.Effort = s.currentEffort
	return &Session{ID: id, Agent: a, server: s}
}

// send fans a server event out to every WS client, tagged with this
// session's id so the React client can dispatch it into per-session state.
func (sess *Session) send(f Frame) {
	f.Session = sess.ID
	sess.server.send(f)
}

// sendState pushes a session_state frame carrying the current snapshot
// (phase, messages, usage). Used by WS hello and handleLoadSession — every
// "populate this slot" path goes through here.
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

// setPhase records the session's current phase on the struct (so reconnects
// can replay it) and emits a Phase event.
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

// cancel halts any in-flight Send for this session. No-op if idle.
func (sess *Session) cancel() {
	sess.mu.Lock()
	cancel := sess.streamCancel
	sess.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// close cancels any in-flight stream. The session's Agent shares the
// workspace's resources; there's nothing else to release.
func (sess *Session) close() {
	sess.cancel()
}
