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
	reply   chan promptReply
}

type promptReply struct {
	text     string
	approved bool
	action   string
	content  map[string]any
}

func (s *Server) Elicit(ctx context.Context, req tool.ElicitRequest) (tool.ElicitResult, error) {
	reply, err := s.prompt(ctx, PromptKindAsk, req.Message, req.Fields)
	if err != nil {
		return tool.ElicitResult{}, err
	}

	switch tool.ElicitAction(reply.action) {
	case tool.ElicitAccept:
		return tool.ElicitResult{Action: tool.ElicitAccept, Content: reply.content}, nil
	case tool.ElicitDecline:
		return tool.ElicitResult{Action: tool.ElicitDecline}, nil
	case tool.ElicitCancel:
		return tool.ElicitResult{Action: tool.ElicitCancel}, nil
	}

	// Plain-text reply (no structured action): map onto a single-field request.
	if reply.text != "" && len(req.Fields) == 1 {
		return tool.ElicitResult{
			Action:  tool.ElicitAccept,
			Content: map[string]any{req.Fields[0].Name: reply.text},
		}, nil
	}

	return tool.ElicitResult{Action: tool.ElicitCancel}, nil
}

func (s *Server) Confirm(ctx context.Context, message string) (bool, error) {
	reply, err := s.prompt(ctx, PromptKindConfirm, message, nil)
	if err != nil {
		return false, err
	}
	return reply.approved, nil
}

func (s *Server) prompt(ctx context.Context, kind, message string, fields []tool.ElicitField) (promptReply, error) {
	sid := code.SessionIDFromContext(ctx)
	id := uuid.NewString()
	p := pendingPrompt{
		sid:     sid,
		kind:    kind,
		message: message,
		fields:  fields,
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
	case p.reply <- promptReply{text: msg.Text, approved: msg.Approved, action: msg.Action, content: msg.Content}:
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
