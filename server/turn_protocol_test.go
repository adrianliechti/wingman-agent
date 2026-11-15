package server

import (
	"encoding/json"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestTurnQueueEntryPreservesEditableInput(t *testing.T) {
	input := code.TurnInput{ID: "input-1", Intent: code.TurnInputFollowUp, Display: &code.TurnInputDisplay{Text: "fix it", Files: []string{"main.go"}, Images: []string{"image"}}}
	entry := turnQueueEntry(input, code.TurnInputQueued, 2)
	input.Display.Files[0] = "mutated"
	if entry.ID != "input-1" || entry.Text != "fix it" || entry.Files[0] != "main.go" || entry.Position != 2 {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestToolCallFrameCarriesPartialState(t *testing.T) {
	b, err := json.Marshal(Frame{
		Type: EvtToolCall, ID: "call-1", Kind: "edit", Partial: true,
		Locations: []harness.ToolLocation{{Path: "/workspace/main.go", Line: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Kind      string                 `json:"kind"`
		Partial   bool                   `json:"partial"`
		Locations []harness.ToolLocation `json:"locations"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Kind != "edit" || !decoded.Partial || len(decoded.Locations) != 1 || decoded.Locations[0].Line != 8 {
		t.Fatalf("frame = %s", b)
	}
}

func TestSnapshotHasActive(t *testing.T) {
	if snapshotHasActive(code.TurnSnapshot{Inputs: []code.TurnInputSnapshot{{State: code.TurnInputQueued}}}) {
		t.Fatal("queued-only snapshot reported active")
	}
	if !snapshotHasActive(code.TurnSnapshot{Inputs: []code.TurnInputSnapshot{{State: code.TurnInputActive}}}) {
		t.Fatal("active snapshot reported idle")
	}
}

func TestTranscriptPreservesAssistantTextIdentity(t *testing.T) {
	for _, content := range []harness.Content{
		{Text: "answer", TextID: "message-1"},
		{Refusal: "answer", TextID: "message-1"},
	} {
		entries := transcriptEntries([]harness.Message{{Role: harness.RoleAssistant, Content: []harness.Content{content}}})
		if len(entries) != 1 {
			t.Fatalf("entries = %+v", entries)
		}
		if got := entries[0]; got.Content != "answer" || got.MessageID != "message-1" {
			t.Fatalf("content = %+v", got)
		}
	}
}

func TestTranscriptPreservesToolKind(t *testing.T) {
	entries := transcriptEntries([]harness.Message{{
		Role: harness.RoleAssistant,
		Content: []harness.Content{
			{ToolCall: &harness.ToolCall{ID: "edit-1", Name: "Editing files", Kind: "edit", Locations: []harness.ToolLocation{{Path: "/workspace/main.go"}}}},
			{ToolResult: &harness.ToolResult{ID: "edit-1", Name: "Editing files", Kind: "edit", Locations: []harness.ToolLocation{{Path: "/workspace/main.go"}}, Content: tool.TextContent("+new")}},
		},
	}})

	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if got := entries[0]; got.ToolKind != "edit" || len(got.ToolLocations) != 1 || got.ToolResult == nil || *got.ToolResult != "+new" {
		t.Fatalf("tool = %+v", got)
	}
}
