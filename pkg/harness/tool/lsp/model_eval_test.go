package lsp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/code/prompt"
	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
	lsptool "github.com/adrianliechti/wingman-agent/pkg/harness/tool/lsp"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

// Run this same file in the baseline and changed checkout with an explicitly
// configured model. Unlike the deterministic LSP evaluation, this measures real
// model tool selection, complete-task correctness, round trips, and token usage.
// answerJSON takes the JSON object from a final answer, which models may
// fence or follow an explanation with.
func answerJSON(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.LastIndex(text, "```json"); index >= 0 {
		block := text[index+len("```json"):]
		if end := strings.Index(block, "```"); end >= 0 {
			block = block[:end]
		}
		return strings.TrimSpace(block)
	}
	if start, end := strings.Index(text, "{"), strings.LastIndex(text, "}"); start >= 0 && end > start {
		return text[start : end+1]
	}
	return text
}

func TestAnswerJSON(t *testing.T) {
	const want = `{"definition":"types.go:6","callers":["uses.go:3"]}`
	for _, text := range []string{
		want,
		"```json\n" + want + "\n```",
		"I found it at `types.go:6`.\n\n```json\n" + want + "\n```",
		"The answer is " + want + ".",
	} {
		if got := answerJSON(text); got != want {
			t.Errorf("answerJSON(%q) = %q", text, got)
		}
	}
}

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
			var servers []lsp.Server
			if !available {
				// Shadows the built-in gopls with a command that does not exist.
				servers = append(servers, lsp.Server{Name: "gopls", Command: filepath.Join(root, "missing-gopls"), ExtensionToLanguage: map[string]string{".go": "go"}})
			} else if _, err := exec.LookPath("gopls"); err != nil {
				t.Fatal("gopls is required on PATH")
			}
			service := language.New(root, servers...)
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
						t.Logf("TOOL_ERROR name=%s content=%s", content.ToolResult.Name, content.ToolResult.Content.Text())
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
			if err := json.Unmarshal([]byte(answerJSON(final)), &answer); err != nil {
				t.Fatalf("invalid final JSON: %v; %s", err, final)
			}
			slices.Sort(answer.Callers)
			if answer.Definition != "types.go:6" || !slices.Equal(answer.Callers, []string{"uses.go:3", "uses.go:4"}) {
				t.Fatalf("incorrect semantic answer: %+v", answer)
			}
		})
	}
}
