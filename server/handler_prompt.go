package server

import (
	"context"

	"github.com/google/uuid"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

type pendingPrompt struct {
	sid     string
	kind    string
	message string
	fields  []tool.ElicitField
	reply   chan ClientMessage
}

func (s *Server) Elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error) {
	reply, err := s.prompt(ctx, PromptKindAsk, req.Message, req.Fields)
	if err != nil {
		return tool.ElicitResult{}, err
	}

	switch tool.ElicitAction(reply.Action) {
	case tool.ElicitAccept:
		return tool.ElicitResult{Action: tool.ElicitAccept, Content: reply.Content}, nil
	case tool.ElicitDecline:
		return tool.ElicitResult{Action: tool.ElicitDecline}, nil
	case tool.ElicitCancel:
		return tool.ElicitResult{Action: tool.ElicitCancel}, nil
	}

	// Plain-text reply (no structured action): map onto a single-field request.
	if reply.Text != "" && len(req.Fields) == 1 {
		return tool.ElicitResult{
			Action:  tool.ElicitAccept,
			Content: map[string]any{req.Fields[0].Name: reply.Text},
		}, nil
	}

	return tool.ElicitResult{Action: tool.ElicitCancel}, nil
}

func (s *Server) Confirm(ctx context.Context, message string) (bool, error) {
	sid := code.SessionIDFromContext(ctx)

	s.promptsMu.Lock()
	all := s.confirmAll[sid]
	s.promptsMu.Unlock()
	if all {
		return true, nil
	}

	reply, err := s.prompt(ctx, PromptKindConfirm, message, nil)
	if err != nil {
		return false, err
	}

	if reply.Approved && reply.Always {
		s.promptsMu.Lock()
		s.confirmAll[sid] = true
		s.promptsMu.Unlock()
	}

	return reply.Approved, nil
}

func (s *Server) prompt(ctx context.Context, kind, message string, fields []tool.ElicitField) (ClientMessage, error) {
	sid := code.SessionIDFromContext(ctx)
	id := uuid.NewString()
	p := pendingPrompt{
		sid:     sid,
		kind:    kind,
		message: message,
		fields:  fields,
		reply:   make(chan ClientMessage, 1),
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
		Type:         EvtPrompt,
		PromptID:     id,
		PromptKind:   kind,
		Message:      message,
		PromptFields: fields,
	})

	select {
	case reply := <-p.reply:
		return reply, nil
	case <-ctx.Done():
		return ClientMessage{}, ctx.Err()
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
	case p.reply <- msg:
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
			Type:         EvtPrompt,
			Session:      sid,
			PromptID:     id,
			PromptKind:   p.kind,
			Message:      p.message,
			PromptFields: p.fields,
		})
	}
	return out
}
