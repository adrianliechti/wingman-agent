package server

import (
	"encoding/json"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestNetLineStatsNetsOutRevertedEdits(t *testing.T) {
	added, removed := netLineStats([]tool.FileChange{
		{Path: "a.go", Before: "a\n", After: "a\nb\n"},
		{Path: "b.go", Before: "x\n", After: "y\n"},
		{Path: "a.go", Before: "a\nb\n", After: "a\n"},
	})
	if added != 1 || removed != 1 {
		t.Fatalf("got +%d -%d, want +1 -1 (a.go reverted, b.go changed one line)", added, removed)
	}
}

func TestTurnReviewListingKeepsReviewFieldsAndCounts(t *testing.T) {
	data, err := json.Marshal(turnReviewListing{
		TurnReview: harness.TurnReview{ID: "turn", Files: []tool.FileChange{{Path: "a.go"}}},
		Added:      3,
		Removed:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "turn" || got["added"] != 3.0 || got["removed"] != 1.0 {
		t.Fatalf("listing = %s", data)
	}
	if files, _ := got["files"].([]any); len(files) != 1 {
		t.Fatalf("listing files = %s", data)
	}
}
