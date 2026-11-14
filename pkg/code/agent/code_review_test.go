package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
)

func TestSlashReviewRunsIsolatedReadOnlyChildAndRecordsSummary(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	initCmd := exec.Command("git", "-C", ws.RootPath, "init")
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, string(out))
	}
	_ = exec.Command("git", "-C", ws.RootPath, "config", "user.email", "test@example.com").Run()
	_ = exec.Command("git", "-C", ws.RootPath, "config", "user.name", "Test").Run()

	srcPath := filepath.Join(ws.RootPath, "calc.go")
	if err := os.WriteFile(srcPath, []byte("package calc\n\nfunc Div(a, b int) int { return a / b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("git", "-C", ws.RootPath, "add", "calc.go").Run()
	_ = exec.Command("git", "-C", ws.RootPath, "commit", "-m", "init").Run()

	// Introduce an uncommitted regression diff for /review to inspect.
	if err := os.WriteFile(srcPath, []byte("package calc\n\nfunc Div(a, b int) int { return a / 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var bodies []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()

		var output any
		if n == 1 {
			// Reviewer inspects surrounding file read-only, keeping that tool noise out of parent history.
			output = map[string]any{
				"type": "function_call", "status": "completed", "call_id": "rev-read-1",
				"name": "read", "arguments": `{"file_path":"calc.go"}`,
			}
		} else {
			output = map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "[P0] `calc.go:3` — Division by zero (`a / 0`) panics unconditionally at runtime.",
				}},
			}
		}
		event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{output}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	t.Cleanup(provider.Close)
	t.Setenv("WINGMAN_URL", provider.URL)

	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.RequireFinish = func(string) bool { return false }
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	cmds := a.Commands(id)
	if len(cmds) == 0 || cmds[0].Name != "review" {
		t.Fatalf("expected /review in Commands(), got %+v", cmds)
	}

	stream, err := a.Send(t.Context(), id, []harness.Content{{Text: "/review focus on arithmetic panics"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(t.Context(), id, []harness.Content{{Text: "another turn"}}); !errors.Is(err, harness.ErrTurnInProgress) {
		t.Fatalf("review did not reserve the parent: %v", err)
	}
	var streamed strings.Builder
	for msg, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range msg.Content {
			streamed.WriteString(c.Text)
		}
	}
	if !strings.Contains(streamed.String(), "[P0] `calc.go:3`") {
		t.Fatalf("unexpected review output: %s", streamed.String())
	}

	if usage := a.Usage(id); usage.InputTokens != 20 || usage.OutputTokens != 6 {
		t.Fatalf("review usage lost: %+v", usage)
	}

	mu.Lock()
	firstReq := bodies[0]
	mu.Unlock()
	if !strings.Contains(firstReq, "return a / 0") {
		t.Fatalf("review request did not attach uncommitted git diff:\n%s", firstReq)
	}
	if strings.Contains(firstReq, `"name":"edit"`) {
		t.Fatalf("isolated reviewer exposed mutating edit tool:\n%s", firstReq)
	}

	// Parent session history contains only visible user /review prompt + final report
	// (no intermediate read tool calls polluting the main coding context).
	var visible []harness.Message
	for _, m := range a.Messages(id) {
		if !m.Hidden {
			visible = append(visible, m)
		}
	}
	if len(visible) != 2 {
		t.Fatalf("expected 2 clean visible messages in parent history, got %d: %+v", len(visible), visible)
	}
}

func TestSlashReviewCancelBeforeConsumption(t *testing.T) {
	ws := newOptionsTestWorkspace(t)
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	a := New(ws, cfg, nil)
	t.Cleanup(func() { _ = a.Close() })
	id, err := a.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := a.Send(t.Context(), id, []harness.Content{{Text: "/review"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Cancel(id)
	var got error
	for _, err := range stream {
		got = errors.Join(got, err)
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("cancel did not reach reviewer: %v", got)
	}
}

func TestReviewTargetUsesRequestedCommitAndUntrackedScope(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("original\n")
	git("add", ".")
	git("commit", "-m", "initial")
	write("selected change\n")
	git("commit", "-am", "selected")
	selected := git("rev-parse", "HEAD")
	write("later unrelated change\n")
	git("commit", "-am", "later")
	prompt := buildReviewTargetContext(t.Context(), root, selected)
	if !strings.Contains(prompt, "+selected change") || strings.Contains(prompt, "later unrelated") {
		t.Fatalf("wrong commit diff: %s", prompt)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("untracked change"), 0644); err != nil {
		t.Fatal(err)
	}
	prompt = buildReviewTargetContext(t.Context(), root, "")
	if !strings.Contains(prompt, "new.txt") || strings.Contains(prompt, "git show HEAD") {
		t.Fatalf("untracked-only scope used unrelated HEAD: %s", prompt)
	}
}
