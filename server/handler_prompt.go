package server

import (
	"context"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

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

func (s *Server) Ask(ctx context.Context, message string) (string, error) {
	reply, err := s.elicit(ctx, PromptKindAsk, message)
	if err != nil {
		return "", err
	}
	return reply.text, nil
}

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
