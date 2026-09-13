package agent

import (
	"fmt"
	"github.com/adrianliechti/wingman-agent/pkg/agent/hook/truncation"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

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
	for _, name := range []string{"read", "fetch", "exec_command", "mcp_tool", "lsp"} {
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
