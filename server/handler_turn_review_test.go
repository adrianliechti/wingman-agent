package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

func TestTurnReviewListingSummarizesNetFileChanges(t *testing.T) {
	listing := newTurnReviewListing(harness.TurnReview{ID: "turn", Files: []tool.FileChange{
		{Path: "a.go", Before: "a\n", After: "a\nb\n", BeforeExists: true, AfterExists: true},
		{Path: "src/b.go", After: "y\n", AfterExists: true},
		{Path: "a.go", Before: "a\nb\n", After: "a\n", BeforeExists: true, AfterExists: true},
	}})
	want := []turnFileStats{
		{Path: "a.go", BeforeExists: true, AfterExists: true},
		{Path: "src/b.go", AfterExists: true, Added: 1},
	}
	if !reflect.DeepEqual(listing.Files, want) {
		t.Fatalf("files = %#v, want %#v (a.go reverted, src/b.go added)", listing.Files, want)
	}

	data, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID    string           `json:"id"`
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "turn" || len(got.Files) != 2 || got.Files[1]["added"] != 1.0 {
		t.Fatalf("listing = %s", data)
	}
	if _, ok := got.Files[0]["before"]; ok {
		t.Fatalf("listing sends file contents: %s", data)
	}
}
