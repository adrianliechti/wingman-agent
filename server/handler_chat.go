package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func (s *backendRuntime) buildInput(sessionID string, msg Command) []harness.Content {
	var input []harness.Content
	if msg.Text != "" {
		input = append(input, harness.Content{Text: msg.Text})
		if !s.isPromptCommand(sessionID, msg.Text) {
			for _, block := range s.skillBlocks(msg.Text) {
				input = append(input, harness.Content{Text: block, Hidden: true})
			}
		}
	}
	for _, text := range msg.Context {
		input = append(input, harness.Content{Text: text, Hidden: true})
	}
	var rootPath string
	if s.workspace != nil {
		rootPath = s.workspace.RootPath
	}
	for _, f := range msg.Files {
		input = append(input, harness.Content{Text: fmt.Sprintf("[File: %s]", f)})
		if dataURL := readAttachedWorkspaceImage(f, rootPath); dataURL != "" {
			input = append(input, harness.Content{File: &harness.File{Name: f, Data: dataURL}})
		}
	}
	for _, img := range msg.Images {
		if img == "" {
			continue
		}
		input = append(input, harness.Content{File: &harness.File{Data: img}})
	}
	return input
}

func readAttachedWorkspaceImage(path, rootPath string) string {
	resolved := path
	if !filepath.IsAbs(resolved) {
		if rootPath == "" {
			return ""
		}
		resolved = filepath.Join(rootPath, resolved)
	}
	f, err := os.Open(filepath.Clean(resolved))
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > tool.MaxImageSourceBytes {
		return ""
	}
	var head [512]byte
	n, err := f.Read(head[:])
	if err != nil && err != io.EOF {
		return ""
	}
	if tool.ImageMIME(head[:n]) == "" {
		return ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	res, err := tool.ReadImage(context.Background(), f)
	if err != nil {
		return ""
	}
	return res.Content
}

func (s *backendRuntime) handleTurnEvent(ev code.TurnEvent) {
	if ev.Retry != nil {
		c := s.session(ev.SessionID)
		c.mu.Lock()
		c.state.Phase, c.state.Retry = "retrying", ev.Retry
		c.publishStateLocked()
		c.mu.Unlock()
		return
	}
	if ev.Compaction != nil {
		c := s.session(ev.SessionID)
		c.mu.Lock()
		c.compactedLocked(ev.InputID, *ev.Compaction)
		c.mu.Unlock()
		s.sendUsageIfChanged(ev.SessionID)
		return
	}
	if ev.StreamEvent != 0 {
		switch ev.StreamEvent {
		case harness.StreamEventReset:
			s.sendSession(ev.SessionID, Frame{Type: EvtStreamReset})
		case harness.StreamEventCommit:
			s.session(ev.SessionID).replaceHistory()
		}
		return
	}

	if ev.Message != nil {
		if ev.Message.Hidden || ev.Message.Role == harness.RoleUser {
			return
		}
		for _, c := range ev.Message.Content {
			if c.Hidden {
				continue
			}
			switch {
			case c.ToolCall != nil:
				display := displayTool(
					c.ToolCall.Name, c.ToolCall.Kind, c.ToolCall.Args, c.ToolCall.Locations,
					c.ToolCall.Presentation,
				)
				s.sendSession(ev.SessionID, Frame{
					Type:      EvtToolCall,
					ID:        c.ToolCall.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Hint:      display.hint,
					Partial:   c.ToolCall.Partial,
				})
				if c.ToolCall.Partial {
					s.setSessionPhase(ev.SessionID, "thinking")
				} else {
					s.setSessionPhase(ev.SessionID, "tool_running")
				}

			case c.ToolResult != nil:
				display := displayTool(
					c.ToolResult.Name, c.ToolResult.Kind, c.ToolResult.Args, c.ToolResult.Locations,
					c.ToolResult.Presentation,
				)
				s.sendSession(ev.SessionID, Frame{
					Type:      EvtToolResult,
					ID:        c.ToolResult.ID,
					Name:      display.name,
					Kind:      display.kind,
					Args:      display.args,
					Locations: display.locations,
					Hint:      display.hint,
					Content:   c.ToolResult.Content,
				})

				if s.files != nil {
					s.files.Notify()
				}
				s.sendSession(ev.SessionID, Frame{Type: EvtTasksChanged})

			case c.Reasoning != nil && c.Reasoning.Summary != "":
				s.setSessionPhase(ev.SessionID, "thinking")
				s.sendSession(ev.SessionID, Frame{
					Type: EvtReasoningDelta,
					ID:   c.Reasoning.ID,
					Part: c.Reasoning.Part,
					Text: c.Reasoning.Summary,
				})

			case c.AsText() != "":
				s.setSessionPhase(ev.SessionID, "streaming")
				s.sendSession(ev.SessionID, Frame{Type: EvtTextDelta, ID: c.TextID, Text: c.AsText()})
			}
			if c.File != nil && c.File.Data != "" {
				s.sendSession(ev.SessionID, Frame{Type: EvtImage, Content: c.File.Data})
			}
		}
		s.sendUsageIfChanged(ev.SessionID)
		return
	}

	if ev.State == "" {
		return
	}
	entry := turnQueueEntry(ev.Input, ev.State, ev.Position)
	s.sendSession(ev.SessionID, Frame{Type: EvtTurnInput, Input: &entry})

	switch ev.State {
	case code.TurnInputActive:
		s.setSessionPhase(ev.SessionID, "thinking")
	case code.TurnInputFailed:
		if ev.Err != nil && !errors.Is(ev.Err, context.Canceled) {
			s.sendSession(ev.SessionID, Frame{Type: EvtError, Message: ev.Err.Error()})
		}
	case code.TurnInputCancelled:
		if ev.Executed {
			s.sendSession(ev.SessionID, Frame{Type: EvtError, Message: "Cancelled"})
		}
	}

	if ev.Executed {
		s.finalizeTurn(ev.SessionID)
	}
	s.sendTurnSnapshot(ev.SessionID)
}

func (s *backendRuntime) finalizeTurn(sid string) {
	a := s.agent
	if a == nil {
		return
	}
	s.session(sid).replaceHistory()
	s.sendUsageIfChanged(sid)

	ws := s.workspace
	s.flushFiles()

	if ws != nil && ws.HasLSP() {
		s.refreshDiagnostics()
	}

	// Persistence belongs to the backend's journal, including partial and
	// interrupted turns. Web finalization only refreshes resource projections.
	if len(a.Messages(sid)) > 0 {
	}

}

func snapshotHasActive(snapshot code.TurnSnapshot) bool {
	for _, input := range snapshot.Inputs {
		if input.State == code.TurnInputActive || input.State == code.TurnInputSteered {
			return true
		}
	}
	return false
}

func (s *backendRuntime) sendTurnSnapshot(sessionID string) {
	if s.turns == nil {
		return
	}
	c := s.session(sessionID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Status == "deleted" {
		return
	}
	snapshot := s.turns.Snapshot(sessionID)
	queue := make([]TurnQueueEntry, 0, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		// Accepted active/steered inputs already belong to the transcript. Only
		// waiting user inputs are editable in the queue panel.
		if input.State != code.TurnInputQueued || input.Input.Origin == "task" {
			continue
		}
		queue = append(queue, turnQueueEntry(input.Input, input.State, input.Position))
	}
	c.state.PendingInputs = queue
	c.state.CanSteer = snapshot.Features.Steer
	if snapshot.Error != nil {
		message := snapshot.Error.Error()
		c.state.Error = &message
	}
	switch {
	case snapshot.Compacting:
		c.state.Phase = "compacting"
	case !snapshotHasActive(snapshot):
		c.state.Phase = "idle"
	case c.state.Phase == "idle" || c.state.Phase == "compacting":
		c.state.Phase = "thinking"
	}
	c.publishStateLocked()
}

// isCompactCommand recognizes "/compact [focus]", which runs as a compaction turn.
func isCompactCommand(text string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), "/compact")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\n')
}

func compactFocus(text string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/compact"))
}

func turnQueueEntry(input code.TurnInput, state code.TurnInputState, position int) TurnQueueEntry {
	display := visibleInput(code.CloneTurnInput(input))
	return TurnQueueEntry{ID: input.ID, State: string(state), Intent: string(input.Intent), Position: position, Text: display.Text, Files: display.Files, Images: display.Images, Origin: input.Origin, Compact: input.Compact}
}

func (s *backendRuntime) sendUsageIfChanged(sessionID string) {
	s.session(sessionID).refreshSettings()
}
