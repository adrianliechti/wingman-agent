package agent

import (
	"encoding/json"
	"fmt"
	harness "github.com/adrianliechti/wingman-agent/pkg/agent"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTurnReviewAndUndoSurviveJournalReload(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	path := filepath.Join(ws.RootPath, "notes.txt")
	if err := os.WriteFile(path, []byte("user baseline\n"), 0644); err != nil {
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
			output = map[string]any{"type": "function_call", "id": "edit", "call_id": "edit", "name": "edit", "arguments": `{"edits":[{"file_path":"notes.txt","old_string":"user baseline","new_string":"user baseline plus agent edit"}]}`, "status": "completed"}
		default:
			output = map[string]any{"type": "message", "id": "final", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Done", "annotations": []any{}}}}
		}
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": map[string]any{"output": []any{output}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer provider.Close()
	t.Setenv("WINGMAN_URL", provider.URL)
	cfg, err := harness.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	a := New(ws, cfg, nil)
	defer a.Close()
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := a.Send(harness.WithInputID(t.Context(), "review-input"), id, []harness.Content{{Text: "update notes"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := a.TurnReviews(t.Context(), id)
	if err != nil || len(reviews) != 1 || len(reviews[0].Files) != 1 || reviews[0].Files[0].Before != "user baseline\n" || reviews[0].InputID != "review-input" || reviews[0].Validation != "not_run" {
		t.Fatalf("review = %+v, %v", reviews, err)
	}
	turn := reviews[0].ID
	a.Close()
	reopened := New(ws, cfg, nil)
	defer reopened.Close()
	if _, err := reopened.TurnReviews(t.Context(), id); err != nil {
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
