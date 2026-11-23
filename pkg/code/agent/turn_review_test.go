package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func TestTurnReviewAndUndoSurviveJournalReload(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	path := filepath.Join(ws.RootPath, "notes.txt")
	if err := os.WriteFile(path, []byte("user baseline\n"), 0644); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(ws.RootPath, "asset.bin")
	moved := filepath.Join(ws.RootPath, "renamed.bin")
	original := []byte{0, 0xff, 2, 3}
	if err := os.WriteFile(asset, original, 0644); err != nil {
		t.Fatal(err)
	}
	request := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		request++
		var output any
		switch request {
		case 1:
			output = map[string]any{"type": "function_call", "id": "read", "call_id": "read", "name": "read", "arguments": `{"file_path":"notes.txt"}`, "status": "completed"}
		case 2:
			output = map[string]any{"type": "function_call", "id": "edit", "call_id": "edit", "name": "edit", "arguments": `{"edits":[{"file_path":"notes.txt","old_string":"user baseline","new_string":"user baseline plus agent edit"},{"file_path":"asset.bin","move_to":"renamed.bin","old_string":"","new_string":""}]}`, "status": "completed"}
		default:
			output = map[string]any{"type": "message", "id": "final", "role": "assistant", "phase": "final_answer", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Done", "annotations": []any{}}}}
		}
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": map[string]any{"output": []any{output}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer provider.Close()
	t.Setenv("WINGMAN_URL", provider.URL)
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	a := New(ws, cfg, nil)
	defer a.Close()
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := a.Send(harness.WithInputID(t.Context(), "review-input"), id, []harness.Content{{Text: "update notes and move asset.bin to renamed.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := a.TurnReviews(t.Context(), id)
	if err != nil || len(reviews) != 1 || len(reviews[0].Files) != 3 || reviews[0].Files[0].Before != "user baseline\n" || reviews[0].InputID != "review-input" || reviews[0].Validation != "not_run" {
		t.Fatalf("review = %+v, %v", reviews, err)
	}
	turn := reviews[0].ID
	a.Close()
	reopened := New(ws, cfg, nil)
	defer reopened.Close()
	reloaded, err := reopened.TurnReviews(t.Context(), id)
	if err != nil || len(reloaded) != 1 || len(reloaded[0].Files) != 3 {
		t.Fatalf("reloaded review=%+v, %v", reloaded, err)
	}
	dest := reloaded[0].Files[2]
	if dest.MovedFrom != "asset.bin" || dest.AfterRef == nil || dest.AfterRef.Size != int64(len(original)) || !dest.AfterRef.Equal(reloaded[0].Files[1].BeforeRef) {
		t.Fatalf("reloaded move=%+v", dest)
	}
	if err := os.WriteFile(moved, []byte{0, 0xfe, 2, 3}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := reopened.UndoTurn(t.Context(), id, turn); err == nil {
		t.Fatal("undid an externally changed rename")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != reviews[0].Files[0].After {
		t.Fatal("failed undo changed the text edit")
	}
	if err := os.WriteFile(moved, original, 0644); err != nil {
		t.Fatal(err)
	}
	// User edits after the turn must prevent undo without changing any content.
	if err := os.WriteFile(path, []byte("new user work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := reopened.UndoTurn(t.Context(), id, turn); err == nil {
		t.Fatal("undo discarded newer user edits")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new user work\n" {
		t.Fatal("failed undo changed file")
	}
	if err := os.WriteFile(path, []byte(reviews[0].Files[0].After), 0644); err != nil {
		t.Fatal(err)
	}
	if err := reopened.UndoTurn(t.Context(), id, turn); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "user baseline\n" {
		t.Fatalf("undo lost baseline: %s", got)
	}
	if data, err := os.ReadFile(asset); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("restored asset=%x, %v", data, err)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Fatalf("unexpected destination after undo: %v", err)
	}
	reopened.Close()
	restored := New(ws, cfg, nil)
	defer restored.Close()
	reviews, err = restored.TurnReviews(t.Context(), id)
	if err != nil || len(reviews) != 1 || !reviews[0].Undone {
		t.Fatalf("undo marker not durable: %+v, %v", reviews, err)
	}
	if err := restored.UndoTurn(t.Context(), id, turn); err == nil {
		t.Fatal("undid twice")
	}
}
