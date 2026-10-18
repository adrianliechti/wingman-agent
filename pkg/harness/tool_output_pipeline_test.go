package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/harness/hook"
	"github.com/adrianliechti/wingman-agent/pkg/harness/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool/fs"
)

func TestHookContextOverflowPersistsCompleteResult(t *testing.T) {
	for _, originalBytes := range []int{100, tool.MaxInlineResultBytes - 100, tool.MaxInlineResultBytes + 100} {
		t.Run(fmt.Sprint(originalBytes), func(t *testing.T) {
			dir := t.TempDir()
			original := strings.Repeat("x", originalBytes)
			preContext := strings.Repeat("pre context\n", 100)
			postContext := strings.Repeat("post context\n", 5000)
			cfg := &Config{Hooks: hook.Hooks{
				PreToolUse: []hook.PreToolUse{func(context.Context, tool.ToolCall) (hook.PreToolUseOutcome, error) {
					return hook.PreToolUseOutcome{Outcome: hook.Outcome{AdditionalContext: []string{preContext}}}, nil
				}},
				PostToolUse: []hook.PostToolUse{func(_ context.Context, _ tool.ToolCall, content string) (hook.Outcome, error) {
					if content != original {
						t.Error("post-tool hook did not receive original tool output")
					}
					return hook.Outcome{AdditionalContext: []string{postContext}}, nil
				}},
			}}
			cfg.Hooks.Append(hook.Hooks{TransformToolResult: []hook.TransformToolResult{truncation.Transform(dir)}})
			// Derived agents must retain the same persistence behavior.
			a := &Agent{Config: cfg.Derive()}
			result := a.runSingleToolCall(t.Context(), ToolCall{Name: "probe"}, []tool.Tool{{
				Name: "probe", Execute: func(context.Context, map[string]any) (tool.Result, error) {
					return tool.Text(original), nil
				},
			}})
			if result.IsError || len(result.Content) > tool.MaxInlineResultBytes || !strings.Contains(result.Content, "Full output saved to:") || strings.Contains(result.Content, "<truncated-output>") {
				t.Fatalf("invalid final output: %d bytes, error=%t", len(result.Content), result.IsError)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("saved results: %v, %v", files, err)
			}
			saved, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
			want := original + "\n\n<hook-context>\n" + preContext + "\n\n" + postContext + "\n</hook-context>"
			if err != nil || string(saved) != want {
				t.Fatal("saved result lost tool output or hook context")
			}
		})
	}
}

func TestReadPaginationSurvivesFullOutputPipeline(t *testing.T) {
	for _, line := range []string{"x", strings.Repeat("x", 40), strings.Repeat("日🙂", 143)} {
		t.Run(fmt.Sprint(len(line)), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "large.txt")
			if err := os.WriteFile(path, []byte(strings.Repeat(line+"\n", 2300)), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			read := fs.ReadTool(root)
			persist := truncation.New(dir)
			next := 1
			for next <= 2301 {
				out, err := read.Execute(t.Context(), map[string]any{"file_path": path, "offset": next})
				if err != nil {
					t.Fatal(err)
				}
				if len(out.Content) > tool.MaxInlineResultBytes || !utf8.ValidString(out.Content) {
					t.Fatalf("invalid read result: %d bytes", len(out.Content))
				}
				hooked, err := persist(t.Context(), tool.ToolCall{Name: "read"}, out.Content)
				if err != nil {
					t.Fatal(err)
				}
				if hooked.UpdatedResult != nil {
					t.Fatal("read required a second truncation")
				}
				if boundToolResult(out.Content) != out.Content {
					t.Fatal("harness removed read content")
				}
				body, _, _ := strings.Cut(out.Content, "\n\n[Showing")
				for _, row := range strings.Split(body, "\n") {
					number, value, ok := strings.Cut(row, "\t")
					if !ok {
						t.Fatalf("missing line number: %q", row)
					}
					n, err := strconv.Atoi(number)
					if err != nil || n != next {
						t.Fatalf("pagination skipped line %d: %q", next, number)
					}
					if next <= 2300 && value != line {
						t.Fatalf("line %d was incomplete", next)
					}
					next++
				}
				if next <= 2301 && !strings.Contains(out.Content, fmt.Sprintf("offset=%d", next)) {
					t.Fatalf("bad continuation at %d", next)
				}
			}
		})
	}
}

func TestOversizedToolResultsPersistBeforeHarnessBounds(t *testing.T) {
	for _, name := range []string{"read", "fetch", "exec", "mcp_tool", "lsp"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			content := strings.Repeat("x", tool.MaxInlineResultBytes+1)
			out, err := truncation.New(dir)(t.Context(), tool.ToolCall{Name: name}, content)
			if err != nil || out.UpdatedResult == nil {
				t.Fatalf("missing persisted result: %v", err)
			}
			if boundToolResult(*out.UpdatedResult) != *out.UpdatedResult {
				t.Fatal("preview truncated again")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatalf("saved results: %v, %v", files, err)
			}
			saved, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
			if err != nil || string(saved) != content {
				t.Fatal("full output was lost")
			}
		})
	}
}
