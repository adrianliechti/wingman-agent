package truncation

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
	"github.com/adrianliechti/wingman-agent/pkg/text"
)

func TestPersistedPreviewAccounting(t *testing.T) {
	for _, ending := range []string{"", "\n"} {
		result := strings.Repeat("日本語🙂\n", 6000) + "last line" + ending
		dir := t.TempDir()
		out, err := New(dir)(t.Context(), tool.ToolCall{Name: "shell"}, result)
		if err != nil || out.UpdatedResult == nil {
			t.Fatalf("truncate: %+v, %v", out, err)
		}
		preview := *out.UpdatedResult
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("saved files = %v, %v", entries, err)
		}
		full, err := os.ReadFile(dir + "/" + entries[0].Name())
		if err != nil || string(full) != result {
			t.Fatal("saved output was changed")
		}
		head, tail := text.HeadBytes(result, headPreviewBytes), text.TailBytes(result, tailPreviewBytes)
		omitted := len(result) - len(head) - len(tail)
		for _, want := range []string{fmt.Sprintf("%d bytes (6001 lines); %d bytes omitted", len(result), omitted), head, tail, "Use `read`", dir + "/" + entries[0].Name()} {
			if !strings.Contains(preview, want) {
				t.Fatalf("preview missing %q", text.HeadBytes(want, 100))
			}
		}
		if !utf8.ValidString(preview) {
			t.Fatal("preview split a UTF-8 character")
		}
	}
}

func TestPreviewWithoutPersistenceAndLineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		value string
		lines int
	}{{"", 0}, {"a", 1}, {"a\n", 1}, {"\n", 1}, {"a\n\n", 2}, {"a\nb", 2}} {
		if got := text.LineCount(tc.value); got != tc.lines {
			t.Fatalf("lineCount(%q)=%d, want %d", tc.value, got, tc.lines)
		}
	}
	out, err := New("")(t.Context(), tool.ToolCall{Name: "grep"}, strings.Repeat("x", grepMaxBytes+1))
	if err != nil || out.UpdatedResult == nil {
		t.Fatal("missing preview")
	}
	if strings.Contains(*out.UpdatedResult, "saved to:") || strings.Contains(*out.UpdatedResult, "Use `read`") {
		t.Fatal("claimed nonexistent saved output")
	}
	if got, _ := New("")(t.Context(), tool.ToolCall{Name: "grep"}, strings.Repeat("x", grepMaxBytes)); got.UpdatedResult != nil {
		t.Fatal("truncated output at the exact budget")
	}
}
