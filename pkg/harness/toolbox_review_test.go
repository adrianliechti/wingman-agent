package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/codemode"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

// reviewOf runs one tool call in a turn and returns that turn's review, read
// back from the ledger as a reloaded session would see it.
func reviewOf(t *testing.T, a *Agent, tools []tool.Tool, call ToolCall) (tool.Result, TurnReview) {
	t.Helper()
	if err := a.recordEvents(RuntimeEvent{Type: EventTurnStarted, TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	result := a.runSingleToolCall(t.Context(), call, tools)
	raw, err := json.Marshal(a.Events)
	if err != nil {
		t.Fatal(err)
	}
	var events []RuntimeEvent
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	reviews := BuildTurnReviews(events)
	return result, reviews[len(reviews)-1]
}

func TestScriptEditsAreReviewedAndCanBeUndone(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, content := range map[string]string{"a.txt": "one\n", "b.txt": "one\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tools := append(fs.Tools(root, &fs.Options{}), codemode.Tools(&codemode.Options{
		CacheDir: filepath.Join(os.TempDir(), "wingman-codemode-test"),
	})...)
	script := `
for name in (await glob(pattern="*.txt")).splitlines():
    await edit(edits=[{"file_path": name, "old_string": "one", "new_string": "two"}])
"edited"
`
	args, _ := json.Marshal(map[string]any{"code": script})
	result, review := reviewOf(t, &Agent{Config: &Config{}}, tools, ToolCall{ID: "c1", Name: "code", Args: string(args)})
	if result.IsError || result.Content.Text() != "edited" {
		t.Fatalf("result = %q", result.Content.Text())
	}
	if len(review.Files) != 2 || review.Untracked || review.Uncertain {
		t.Fatalf("review = %+v", review)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if data, _ := os.ReadFile(filepath.Join(dir, name)); string(data) != "two\n" {
			t.Fatalf("%s = %q", name, data)
		}
	}
	if err := fs.UndoFileChanges(t.Context(), root, review.Files); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if data, _ := os.ReadFile(filepath.Join(dir, name)); string(data) != "one\n" {
			t.Fatalf("after undo %s = %q", name, data)
		}
	}
}

func TestNestedCallsCountInTheTurnReview(t *testing.T) {
	nested := func(calls ...string) tool.Tool {
		return tool.Tool{Name: "script", Composite: true, Execute: func(ctx context.Context, _ map[string]any) (tool.Result, error) {
			box := tool.ToolboxFromContext(ctx)
			for _, name := range calls {
				box.Call(ctx, name, map[string]any{"command": "go test ./...", "validation": true})
			}
			return tool.Text("done"), nil
		}}
	}
	exitCode := 0
	peers := []tool.Tool{
		{Name: "read", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("text"), nil }},
		{Name: "exec", Execute: func(context.Context, map[string]any) (tool.Result, error) {
			return tool.Result{Content: tool.TextContent("ok"), Metadata: map[string]any{
				"exit_code": exitCode, "session_id": 1, "workspace_snapshot_complete": true,
			}}, nil
		}},
		{Name: "deploy", Execute: func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("shipped"), nil }},
	}
	run := func(calls ...string) TurnReview {
		t.Helper()
		_, review := reviewOf(t, &Agent{Config: &Config{}}, append(peers, nested(calls...)), ToolCall{ID: "c1", Name: "script", Args: `{}`})
		return review
	}

	// A script that only reads leaves nothing to review.
	if review := run("read", "read"); review.Untracked || len(review.Checks) != 0 || review.Validation != "not_run" {
		t.Fatalf("read-only script: %+v", review)
	}
	// A check it ran counts as one the model ran itself.
	if review := run("exec"); review.Untracked || len(review.Checks) != 1 || review.Checks[0].Command != "go test ./..." || review.Validation != "passed" {
		t.Fatalf("script with a check: %+v", review)
	}
	exitCode = 1
	if review := run("exec"); review.Validation != "failed" {
		t.Fatalf("script with a failed check: %+v", review)
	}
	// A tool whose effects are unknown makes the turn untracked, as a direct call does.
	if review := run("read", "deploy"); !review.Untracked {
		t.Fatalf("script calling an unknown tool: %+v", review)
	}
	// And a script that calls nothing is not itself an unknown effect.
	if review := run(); review.Untracked {
		t.Fatalf("empty script: %+v", review)
	}
}
