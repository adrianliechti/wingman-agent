package tool

import (
	"encoding/json"
	"testing"
)

func TestNetFileChangesPreservesModesAndDetectsGaps(t *testing.T) {
	executable := uint32(0755)
	changes := []FileChange{
		{Path: "run.sh", Before: "first", After: "second", BeforeExists: true, AfterExists: true, Mode: 0644, AfterMode: &executable},
		{Path: "run.sh", Before: "second", After: "third", BeforeExists: true, AfterExists: true, Mode: 0755},
	}
	// Exercise the durable representation, as undo often happens after resume.
	raw, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	var restored []FileChange
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	net, gap := NetFileChanges(restored)
	if gap != "" || len(net) != 1 || net[0].Before != "first" || net[0].After != "third" || net[0].Mode != 0644 || net[0].ResultMode() != 0755 {
		t.Fatalf("net=%+v gap=%q", net, gap)
	}
	// An external chmod between agent edits must block undo.
	restored[1].Mode = 0600
	if _, gap := NetFileChanges(restored); gap != "run.sh" {
		t.Fatalf("missed permission gap: %q", gap)
	}
	// Delete/recreate permits a new mode, with no continuity check while absent.
	changes[1].AfterExists = false
	changes[1].After = ""
	changes = append(changes, FileChange{Path: "run.sh", After: "recreated", AfterExists: true, Mode: 0600})
	net, gap = NetFileChanges(changes)
	if gap != "" || len(net) != 1 || net[0].Mode != 0644 || net[0].ResultMode() != 0600 || net[0].After != "recreated" {
		t.Fatalf("recreated net=%+v gap=%q", net, gap)
	}
}
