package server

import (
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestToolImagesSurviveLiveAndRestoredTranscripts(t *testing.T) {
	for _, name := range []string{"read", "fetch", "view_image"} {
		t.Run(name, func(t *testing.T) {
			_, backend, model := newProtocolServer(t)
			controller := backend.session("saved")
			controller.load()
			const image = "data:image/png;base64,AA=="
			messages := []agent.Message{
				{Role: agent.RoleAssistant, Content: []agent.Content{{ToolCall: &agent.ToolCall{ID: "image", Name: name, Args: `{"file_path":"shot.png"}`}}}},
				{Role: agent.RoleAssistant, Content: []agent.Content{
					{ToolResult: &agent.ToolResult{ID: "image", Name: name, Content: "[image attached below]"}},
					{File: &agent.File{Data: image}},
					{File: &agent.File{Data: "hidden"}, Hidden: true},
				}},
			}
			for _, message := range messages {
				backend.handleTurnEvent(code.TurnEvent{SessionID: "saved", Message: &message})
			}
			check := func() {
				t.Helper()
				controller.mu.Lock()
				defer controller.mu.Unlock()
				entries := controller.entries
				if len(entries) != 2 || entries[0].Type != "tool" || entries[0].ToolResult == nil ||
					entries[1].Type != "assistant" || len(entries[1].Images) != 1 || entries[1].Images[0] != image {
					t.Fatalf("transcript = %+v", entries)
				}
			}
			check()
			controller.apply(Frame{Type: EvtStreamReset})
			check() // completed tool images survive provider retries
			model.mu.Lock()
			model.messages = messages
			model.mu.Unlock()
			controller.replaceHistory()
			check()
		})
	}
}
