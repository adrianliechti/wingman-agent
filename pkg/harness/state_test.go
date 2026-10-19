package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateSnapshotIsIndependent(t *testing.T) {
	a := &Agent{
		Messages: []Message{{
			Role: RoleAssistant,
			Content: []Content{{
				Text:      "answer",
				Reasoning: &Reasoning{Summary: "thought"},
			}},
		}},
		Usage:    Usage{InputTokens: 10},
		Revision: 2,
	}

	snapshot := a.StateSnapshot()
	snapshot.Messages[0].Content[0].Text = "changed"
	snapshot.Messages[0].Content[0].Reasoning.Summary = "changed"
	snapshot.Usage.InputTokens = 99

	if a.Messages[0].Content[0].Text != "answer" || a.Messages[0].Content[0].Reasoning.Summary != "thought" {
		t.Fatalf("snapshot mutated agent messages: %+v", a.Messages)
	}
	if a.Usage.InputTokens != 10 || snapshot.Revision != 2 {
		t.Fatalf("snapshot state = %+v, agent usage = %+v", snapshot, a.Usage)
	}
}

func TestStateSnapshotClonesToolResultMetadata(t *testing.T) {
	a := &Agent{Messages: []Message{{Role: RoleAssistant, Content: []Content{{
		ToolResult: &ToolResult{ID: "call-1", Metadata: map[string]any{"exit_code": 7}},
	}}}}}

	snapshot := a.StateSnapshot()
	snapshot.Messages[0].Content[0].ToolResult.Metadata["exit_code"] = 0
	if got := a.Messages[0].Content[0].ToolResult.Metadata["exit_code"]; got != 7 {
		t.Fatalf("snapshot metadata mutated retained state: %v", got)
	}
}

func TestStateSnapshotsOwnToolLocations(t *testing.T) {
	projections := map[string]func(State) Message{
		"events":   func(s State) Message { return *s.Events[0].Message },
		"messages": func(s State) Message { return s.Messages[0] },
		"context":  func(s State) Message { return s.Context[0] },
	}
	for name, project := range projections {
		t.Run(name, func(t *testing.T) {
			want := ToolLocation{Path: "original.go", Line: 3}
			a := &Agent{Messages: []Message{{Role: RoleAssistant, Content: []Content{{
				ToolCall: &ToolCall{
					Locations: []ToolLocation{want}, Presentation: &ToolPresentation{Locations: []ToolLocation{want}},
				},
				ToolResult: &ToolResult{
					Locations: []ToolLocation{want}, Presentation: &ToolPresentation{Locations: []ToolLocation{want}},
				},
			}}}}}
			message := project(a.StateSnapshot())
			message.Content[0].ToolCall.Locations[0].Path = "changed.go"
			message.Content[0].ToolCall.Presentation.Locations[0].Path = "changed.go"
			message.Content[0].ToolResult.Locations[0].Path = "changed.go"
			message.Content[0].ToolResult.Presentation.Locations[0].Path = "changed.go"
			retained := a.StateSnapshot()
			for projection, read := range projections {
				content := read(retained).Content[0]
				if content.ToolCall.Locations[0] != want || content.ToolCall.Presentation.Locations[0] != want ||
					content.ToolResult.Locations[0] != want || content.ToolResult.Presentation.Locations[0] != want {
					t.Errorf("editing %s snapshot changed retained %s locations", name, projection)
				}
			}
		})
	}
}

func TestStateVersionDoesNotCloneHistory(t *testing.T) {
	a := &Agent{Messages: []Message{{Role: RoleUser}}, Revision: 3}
	messageCount, revision := a.StateVersion()
	if messageCount != 1 || revision != 3 {
		t.Fatalf("state version = (%d, %d), want (1, 3)", messageCount, revision)
	}
}

func TestRestoreRejectsInvalidStateWithoutChangingAgent(t *testing.T) {
	a := &Agent{Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "keep"}}}}, Usage: Usage{InputTokens: 10}}
	before := a.StateSnapshot()
	invalid := a.StateSnapshot()
	invalid.Events[len(invalid.Events)-1].Sequence = 0
	if err := a.Restore(invalid); err == nil {
		t.Fatal("accepted invalid event sequence")
	}
	if got := a.StateSnapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed restore changed state: got %+v, want %+v", got, before)
	}
	if err := a.appendMessages(Message{Role: RoleAssistant, Content: []Content{{Text: "continued"}}}); err != nil {
		t.Fatalf("cannot continue after failed restore: %v", err)
	}
}

func TestStateLoadReplacesOnlyAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		fail bool
	}{
		{name: "omitted fields", data: `{}`},
		{name: "invalid field", data: `{"messages":[{"role":"assistant"}],"usage":"invalid"}`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := State{Messages: []Message{{Role: RoleUser, Content: []Content{{Text: "keep"}}}}, Usage: Usage{InputTokens: 10}}
			before := State{Messages: CloneMessages(state.Messages), Usage: state.Usage}
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			err := state.Load(path)
			if (err != nil) != tc.fail {
				t.Fatalf("Load error = %v, want failure %t", err, tc.fail)
			}
			want := State{}
			if tc.fail {
				want = before
			}
			if !reflect.DeepEqual(state, want) {
				t.Fatalf("loaded state = %+v, want %+v", state, want)
			}
		})
	}
}
