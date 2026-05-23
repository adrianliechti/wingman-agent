package server

import (
	"context"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

// pendingPrompt is the server-side bookkeeping for an outstanding
// Ask/Confirm. The full details are kept (not just the reply channel)
// so a client that just reloaded the page can be brought up to speed
// via [Server.sendSessionState].
type pendingPrompt struct {
	sid     string
	kind    string
	message string
	reply   chan promptReply
}

type promptReply struct {
	text     string
	approved bool
}

// Ask implements [code.UI]: broadcast a prompt frame addressed to the
// triggering session and block until the WebUI replies (or the ctx
// cancels).
func (s *Server) Ask(ctx context.Context, message string) (string, error) {
	reply, err := s.elicit(ctx, PromptKindAsk, message)
	if err != nil {
		return "", err
	}
	return reply.text, nil
}

// Confirm implements [code.UI]. Falls back to "deny" on ctx cancel so
// pending destructive operations don't accidentally proceed when the
// turn is being torn down.
func (s *Server) Confirm(ctx context.Context, message string) (bool, error) {
	reply, err := s.elicit(ctx, PromptKindConfirm, message)
	if err != nil {
		return false, err
	}
	return reply.approved, nil
}

func (s *Server) elicit(ctx context.Context, kind, message string) (promptReply, error) {
	sid := code.SessionIDFromContext(ctx)
	id := uuid.NewString()
	p := pendingPrompt{
		sid:     sid,
		kind:    kind,
		message: message,
		reply:   make(chan promptReply, 1),
	}

	s.promptsMu.Lock()
	s.pendingPrompts[id] = p
	s.promptsMu.Unlock()

	// Always tell clients the prompt is closed when this returns —
	// covers both ctx cancel and the multi-tab case where a different
	// tab answered (the tab that answered already cleared optimistically).
	defer func() {
		s.promptsMu.Lock()
		delete(s.pendingPrompts, id)
		s.promptsMu.Unlock()
		s.sendSession(sid, Frame{Type: EvtPromptCancel, PromptID: id})
	}()

	s.sendSession(sid, Frame{
		Type:       EvtPrompt,
		PromptID:   id,
		PromptKind: kind,
		Message:    message,
	})

	select {
	case reply := <-p.reply:
		return reply, nil
	case <-ctx.Done():
		return promptReply{}, ctx.Err()
	}
}

// resolvePrompt is called from the WS read loop when a prompt_response
// arrives. No-op for unknown ids (stale answer for a prompt that
// already timed out or was answered by another tab).
func (s *Server) resolvePrompt(msg ClientMessage) {
	s.promptsMu.Lock()
	p, ok := s.pendingPrompts[msg.PromptID]
	s.promptsMu.Unlock()
	if !ok {
		return
	}
	select {
	case p.reply <- promptReply{text: msg.Text, approved: msg.Approved}:
	default:
	}
}

// pendingPromptFramesFor returns a snapshot of prompts still awaiting a
// reply for sid, as ready-to-send frames. Used by sendSessionState to
// replay them so a client reloading mid-elicit isn't stuck staring at a
// frozen turn.
func (s *Server) pendingPromptFramesFor(sid string) []Frame {
	s.promptsMu.Lock()
	defer s.promptsMu.Unlock()
	var out []Frame
	for id, p := range s.pendingPrompts {
		if p.sid != sid {
			continue
		}
		out = append(out, Frame{
			Type:       EvtPrompt,
			Session:    sid,
			PromptID:   id,
			PromptKind: p.kind,
			Message:    p.message,
		})
	}
	return out
}
