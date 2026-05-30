package acp

import (
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

func TestToolCallContentTextRendersDiff(t *testing.T) {
	old := "line one\nold line\nline three\n"
	items := []acpsdk.ToolCallContent{
		{Diff: &acpsdk.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/a.go",
			OldText: &old,
			NewText: "line one\nnew line\nline three\n",
		}},
	}
	got := toolCallContentText(items)
	if !strings.Contains(got, "/p/a.go") {
		t.Errorf("expected path in output:\n%s", got)
	}
	if !strings.Contains(got, "-old line") || !strings.Contains(got, "+new line") {
		t.Errorf("expected -/+ diff lines:\n%s", got)
	}
	if !strings.Contains(got, " line one") {
		t.Errorf("expected unchanged context line:\n%s", got)
	}
}

func TestToolCallContentTextAddedFile(t *testing.T) {
	// New file: no old text, every line added.
	items := []acpsdk.ToolCallContent{
		{Diff: &acpsdk.ToolCallContentDiff{
			Type:    "diff",
			Path:    "/p/new.go",
			NewText: "package main\n\nfunc main() {}\n",
		}},
	}
	got := toolCallContentText(items)
	if strings.Contains(got, "-") {
		t.Errorf("added file should have no removed lines:\n%s", got)
	}
	if !strings.Contains(got, "+package main") || !strings.Contains(got, "+func main() {}") {
		t.Errorf("expected all lines added:\n%s", got)
	}
}

func TestToolCallContentTextPlainText(t *testing.T) {
	items := []acpsdk.ToolCallContent{
		{Content: &acpsdk.ToolCallContentContent{Content: acpsdk.TextBlock("hello output")}},
	}
	if got := toolCallContentText(items); got != "hello output" {
		t.Errorf("plain text = %q", got)
	}
}
