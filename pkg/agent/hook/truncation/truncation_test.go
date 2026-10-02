package truncation

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
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
		head, tail := text.HeadLines(result, headPreviewBytes), text.TailLines(result, tailPreviewBytes)
		omitted := len(result) - len(head) - len(tail)
		for _, want := range []string{fmt.Sprintf("%d bytes (6001 lines); %d bytes omitted", len(result), omitted), head, tail, "Use `read`", dir + "/" + entries[0].Name()} {
			if !strings.Contains(preview, want) {
				t.Fatalf("preview missing %q", text.HeadBytes(want, 100))
			}
		}
		if !utf8.ValidString(preview) {
			t.Fatal("preview split a UTF-8 character")
		}
		if !strings.HasSuffix(head, "🙂") || !strings.HasPrefix(tail, "日本語") {
			t.Fatalf("preview split a line: head ends %q, tail starts %q", text.TailBytes(head, 8), text.HeadBytes(tail, 8))
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

// Ported from codex's hook output spill tests: in-budget output stays inline
// and leaves no file behind, and image payloads are never rewritten even when
// they exceed the text budget.
func TestInBudgetAndImageResultsStayInlineWithoutScratchFiles(t *testing.T) {
	dir := t.TempDir()
	run := New(dir)
	image := "data:image/png;base64," + base64.StdEncoding.EncodeToString(noisyPNG(t))
	if len(image) <= MaxBytes {
		t.Fatalf("test image must exceed the budget: %d bytes", len(image))
	}
	for name, result := range map[string]string{"short": "short", "exact": strings.Repeat("x", MaxBytes), "image": image} {
		out, err := run(t.Context(), tool.ToolCall{Name: "shell"}, result)
		if err != nil || out.UpdatedResult != nil {
			t.Fatalf("%s: result was rewritten: %+v, %v", name, out, err)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("scratch files written for inline results: %v, %v", entries, err)
	}
}

func noisyPNG(t *testing.T) []byte {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	img := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	for y := range 256 {
		for x := range 256 {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(rng.IntN(256)), G: uint8(rng.IntN(256)), B: uint8(rng.IntN(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
