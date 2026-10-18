package lsp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/devtools"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/graph"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

// Run this same file in the baseline and changed checkout with an explicitly
// configured model. Unlike the deterministic LSP evaluation, this measures real
// model tool selection, complete-task correctness, round trips, and token usage.
func TestLiveLSPToolSelection(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("WINGMAN_LSP_EVAL_MODEL"))
	if model == "" {
		t.Skip("set WINGMAN_LSP_EVAL_MODEL and configure an inference provider for model-level A/B evaluation")
	}
	for _, available := range []bool{true, false} {
		name := "without_lsp"
		if available {
			name = "with_lsp"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range map[string]string{
				"go.mod":   "module lsp-selection.test\n\ngo 1.26\n",
				"types.go": "package example\n\ntype Alpha struct{}\nfunc (Alpha) Close() {}\ntype Beta struct{}\nfunc (Beta) Close() {}\n",
				"uses.go":  "package example\n\nfunc First() { var b Beta; b.Close() }\nfunc Second(b Beta) { b.Close() }\nfunc Unrelated(a Alpha) { a.Close() }\n// Beta.Close is mentioned in a comment.\nvar Text = \"Beta.Close\"\n",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var options []lsp.ManagerOption
			if available {
				managed, err := devtools.New()
				if err != nil {
					t.Fatal(err)
				}
				if managed.Resolve("gopls") == "" {
					t.Fatal("managed gopls required")
				}
				options = append(options, lsp.WithManagedTools(managed))
			}
			service := language.New(root, filepath.Join(t.TempDir(), "graph.json"), options...)
			defer service.Close()
			fsRoot, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer fsRoot.Close()
			var tools []tool.Tool
			for _, candidate := range fs.Tools(fsRoot, nil) {
				if slices.Contains([]string{"read", "grep", "glob"}, candidate.Name) {
					tools = append(tools, candidate)
				}
			}
			tools = append(tools, lsptool.NewTools(service)...)
			tools = append(tools, graph.NewTools(service.Graph())...)
			cfg, err := harness.ConfigFromEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Model = func() string { return model }
			cfg.Effort = func() string { return "low" }
			cfg.MaxTurns = 12
			cfg.ContextWindow = -1
			cfg.Tools = func() []tool.Tool { return tools }
			cfg.Instructions = func() string {
				return prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}) + "\nWorkspace: " + root + "\nFor this evaluation, inspect files using the available read-only tools. Do not delegate or request new tools. Return the requested JSON as your final answer."
			}
			a := &harness.Agent{Config: cfg}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			start := time.Now()
			stream, err := a.Send(ctx, []harness.Content{{Text: "Find the declaration and every call site of the method Beta.Close. Exclude comments, strings, and calls to other receivers. Final answer must be JSON with definition as a file:line string and callers as an array of file:line strings. Cite the line containing each call, not its enclosing function declaration."}})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			var final string
			calls, failures := 0, 0
			counts := map[string]int{}
			for _, message := range a.MessagesSnapshot() {
				var text strings.Builder
				for _, content := range message.Content {
					if content.ToolCall != nil {
						calls++
						counts[content.ToolCall.Name]++
						t.Logf("TOOL_TRACE name=%s args=%s", content.ToolCall.Name, content.ToolCall.Args)
					}
					if content.ToolResult != nil && content.ToolResult.IsError {
						failures++
						t.Logf("TOOL_ERROR name=%s content=%s", content.ToolResult.Name, content.ToolResult.Content)
					}
					if message.Role == harness.RoleAssistant {
						text.WriteString(content.Text)
					}
				}
				if text.Len() > 0 {
					final = text.String()
				}
			}
			usage := a.UsageSnapshot()
			t.Logf("MODEL_METRIC model=%s scenario=%s elapsed_ms=%d tool_calls=%d tool_errors=%d tools=%v input_tokens=%d output_tokens=%d", model, name, time.Since(start).Milliseconds(), calls, failures, counts, usage.InputTokens, usage.OutputTokens)
			var answer struct {
				Definition string   `json:"definition"`
				Callers    []string `json:"callers"`
			}
			final = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(final), "```json"), "```"), "```"))
			if err := json.Unmarshal([]byte(final), &answer); err != nil {
				t.Fatalf("invalid final JSON: %v; %s", err, final)
			}
			slices.Sort(answer.Callers)
			if answer.Definition != "types.go:6" || !slices.Equal(answer.Callers, []string{"uses.go:3", "uses.go:4"}) {
				t.Fatalf("incorrect semantic answer: %+v", answer)
			}
		})
	}
}

// Exercise the model-facing preview -> ordinary edit path, including the private
// freshness connection used by sessions. No digest or preview token is an input.
func TestLiveLSPRenamePreview(t *testing.T) {
	model := strings.TrimSpace(os.Getenv("WINGMAN_LSP_EVAL_MODEL"))
	if model == "" {
		t.Skip("set WINGMAN_LSP_EVAL_MODEL and configure an inference provider")
	}
	managed, err := devtools.New()
	if err != nil {
		t.Fatal(err)
	}
	if managed.Resolve("gopls") == "" {
		t.Fatal("managed gopls required")
	}
	root := t.TempDir()
	files := map[string]string{
		"go.mod":    "module lsp-rename.test\n\ngo 1.26\n",
		"target.go": "package example\n\nfunc Target() int { return 42 }\n",
		"uses.go":   "package example\n\nfunc First() int { return Target() }\nfunc Second() int { return Target() }\n",
		"decoy.go":  "package example\n\n// Target remains here.\nvar Text = \"Target\"\nfunc Shadow() int { Target := 7; return Target }\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service := language.New(root, filepath.Join(t.TempDir(), "graph.json"), lsp.WithManagedTools(managed))
	defer service.Close()
	fsRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fsRoot.Close()
	freshness := fs.NewFreshness(fsRoot)
	var tools []tool.Tool
	for _, candidate := range fs.Tools(fsRoot, &fs.Options{Freshness: freshness}) {
		if slices.Contains([]string{"read", "grep", "glob", "edit"}, candidate.Name) {
			tools = append(tools, candidate)
		}
	}
	tools = append(tools, freshness.TrackSnapshots(lsptool.NewTools(service)[0]))
	cfg, err := harness.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = func() string { return model }
	cfg.Effort = func() string { return "low" }
	cfg.MaxTurns = 12
	cfg.ContextWindow = -1
	cfg.Tools = func() []tool.Tool { return tools }
	cfg.Instructions = func() string {
		return prompt.BuildBaseInstructions(prompt.VariantFor(model).Agent, prompt.SectionData{}) + "\nWorkspace: " + root + "\nUse the available tools; do not delegate or request new tools."
	}
	a := &harness.Agent{Config: cfg}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	stream, err := a.Send(ctx, []harness.Content{{Text: "Rename the function Target declared in target.go to Renamed and update its callers. Preserve comments, strings and the unrelated local variable with the same name. Preview the language-server rename, review it, and apply the authorized changes."}})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	calls, failures, previews, edits := 0, 0, 0, 0
	for _, message := range a.MessagesSnapshot() {
		for _, content := range message.Content {
			if call := content.ToolCall; call != nil {
				calls++
				t.Logf("RENAME_TRACE name=%s args=%s", call.Name, call.Args)
				if call.Name == "lsp" && strings.Contains(call.Args, "previewRename") {
					previews++
				}
				if call.Name == "edit" {
					edits++
					if strings.Contains(call.Args, "sha256") {
						t.Fatal("model supplied a digest")
					}
				}
			}
			if content.ToolResult != nil && content.ToolResult.IsError {
				failures++
				t.Logf("RENAME_ERROR name=%s content=%s", content.ToolResult.Name, content.ToolResult.Content)
				if content.ToolResult.Name == "lsp" || content.ToolResult.Name == "edit" {
					t.Errorf("preview/apply failed: %s", content.ToolResult.Content)
				}
			}
		}
	}
	usage := a.UsageSnapshot()
	t.Logf("RENAME_METRIC model=%s elapsed_ms=%d tool_calls=%d tool_errors=%d previews=%d edits=%d input_tokens=%d output_tokens=%d", model, time.Since(start).Milliseconds(), calls, failures, previews, edits, usage.InputTokens, usage.OutputTokens)
	if previews != 1 || edits != 1 {
		t.Fatalf("expected one preview and one atomic edit, got %d/%d", previews, edits)
	}
	for name, before := range files {
		want := before
		if name == "target.go" || name == "uses.go" {
			want = strings.ReplaceAll(before, "Target", "Renamed")
		}
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("incorrect rename in %s: %q %v", name, got, err)
		}
	}
}
