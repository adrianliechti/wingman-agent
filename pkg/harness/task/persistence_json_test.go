package task

import (
	"encoding/json"
	"testing"
)

func TestTaskSnapshotFinishedJSON(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot string
		finished string
	}{
		{"running", `{"status":"running"}`, ""},
		{"legacy running", `{"status":"running","finished":"0001-01-01T00:00:00Z"}`, ""},
		{"completed", `{"status":"done","finished":"2026-10-19T12:34:56.123Z"}`, `"2026-10-19T12:34:56.123Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot taskSnapshot
			if err := json.Unmarshal([]byte(tc.snapshot), &snapshot); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if got := string(fields["finished"]); got != tc.finished {
				t.Fatalf("finished = %q, want %q", got, tc.finished)
			}
			var restored taskSnapshot
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if !restored.Finished.Equal(snapshot.Finished) || restored.Status != snapshot.Status {
				t.Fatal("snapshot round trip changed task state")
			}
		})
	}
}
