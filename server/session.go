package server

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// Session wraps one code.Agent (own MCP/LSP/Rewind/Messages/Usage) with the
// per-conversation BFF state: a stream cancel so concurrent sends in different
// sessions can be controlled independently.
type Session struct {
	ID    string
	Agent *code.Agent

	server *Server

	mu           sync.Mutex
	streamCancel context.CancelFunc
}

func newSessionID() string {
	return uuid.New().String()
}

// newSession creates a fresh code.Agent for this session and wires
// per-session instructions/hooks. Heavy workspace resources (MCP/LSP/Rewind)
// are initialized lazily in a goroutine so creating a session stays cheap.
func (s *Server) newSession(id string) (*Session, error) {
	a, err := code.New(s.workDir, nil)
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	sess := &Session{ID: id, Agent: a, server: s}

	// Late-bind model/effort/instructions to the server. Changing the model
	// in the picker updates s.model under cfgMu; every session's next Send
	// picks it up automatically — no iterate-and-overwrite needed.
	a.Config.Model = s.currentModel
	a.Config.Effort = s.currentEffort
	a.Config.Instructions = sess.currentInstructions

	// Mirror the single-session truncation hook: cap large tool outputs and
	// dump the full text to this session's scratch dir.
	a.Config.Hooks.PostToolUse = append(a.Config.Hooks.PostToolUse,
		truncation.New(truncation.DefaultMaxBytes, a.ScratchPath),
	)

	// Warm workspace probes + MCP off the request goroutine. Using
	// context.Background rather than the request ctx because the session
	// outlives the request that created it — cancelling InitMCP mid-flight
	// would leave the session without MCP tools.
	go func() {
		a.WarmUp()
		if err := a.InitMCP(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "MCP init warning (session %s): %v\n", id, err)
		}
	}()

	return sess, nil
}

func (sess *Session) currentInstructions() string {
	return code.BuildInstructions(sess.Agent.InstructionsData())
}

// sendMessage broadcasts a server event tagged with this session's id so the
// React client can dispatch it into per-session state.
func (sess *Session) sendMessage(e ServerEvent) {
	sess.server.sendMessage(sess.ID, e)
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

func (sess *Session) close() {
	sess.cancel()
	if sess.Agent != nil {
		sess.Agent.Close()
	}
}
