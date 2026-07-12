package server

import (
	"encoding/json"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestTurnQueueEntryPreservesEditableInput(t *testing.T) {
	meta := ClientMessage{
		ID: "input-1", Intent: string(code.TurnInputFollowUp), Text: "fix it",
		Files: []string{"main.go"}, Images: []string{"data:image/png;base64,abc"},
	}
	entry := turnQueueEntry(meta, code.TurnInputQueued, 2)
	meta.Files[0] = "mutated"
	meta.Images[0] = "mutated"

	if entry.ID != "input-1" || entry.State != "queued" || entry.Position != 2 || entry.Text != "fix it" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.Files[0] != "main.go" || entry.Images[0] != "data:image/png;base64,abc" || entry.ImageCount != 1 {
		t.Fatalf("attachments = %#v", entry)
	}
}

func TestTurnQueueFrameJSONCarriesCapabilitiesAndOrdering(t *testing.T) {
	frame := Frame{
		Type: EvtTurnQueue, Session: "session-1", Paused: true, CanSteer: true,
		Queue: []TurnQueueEntry{{ID: "input-2", State: "queued", Intent: "follow_up", Position: 1, Text: "next"}},
	}
	b, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type     string           `json:"type"`
		Session  string           `json:"session"`
		Paused   bool             `json:"paused"`
		CanSteer bool             `json:"can_steer"`
		Queue    []TurnQueueEntry `json:"queue"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != EvtTurnQueue || decoded.Session != "session-1" || !decoded.Paused || !decoded.CanSteer {
		t.Fatalf("frame = %#v", decoded)
	}
	if len(decoded.Queue) != 1 || decoded.Queue[0].ID != "input-2" || decoded.Queue[0].Position != 1 {
		t.Fatalf("queue = %#v", decoded.Queue)
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
